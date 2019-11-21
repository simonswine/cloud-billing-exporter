package aws

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/organizations"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

type Clock interface {
	Now() time.Time
}

type realClock struct {
}

func (r *realClock) Now() time.Time {
	return time.Now()
}

type awsBillingElement struct {
	ProjectName string
	ProjectID   string
	ServiceName string
	Costs       float64
	Currency    string
}

const (
	AccountTypeProject AccountType = iota
	AccountTypeOrganizationUnit
	AccountTypeOrganization
)

type Account struct {
	ID     AccountID
	Name   AccountName
	Owner  AccountOwner
	Parent AccountID
	Path   AccountPath
	Type   AccountType
}

type (
	AccountID    string
	AccountName  string
	AccountOwner string
	AccountPath  string
	AccountType  int
)

type AWSBilling struct {
	time Clock

	BucketName string
	Region     string

	OwnerTag     string
	ProjectIDTag string

	// accountNameByIDOverride contains account name mappings specified
	// manually through CLI arguments (take precedence)
	accountNameByIDOverride map[AccountID]AccountName

	// accountNameByIDCachedAPI contains account name mappings specified
	accountNameByIDAPI           map[AccountID]*Account
	accountNameByIDAPILastUpdate time.Time
	accountNameByIDAPILock       sync.Mutex

	rootAccountID string

	ReportsLock sync.Mutex
	ReportHash  string

	MetricMonthlyCosts *prometheus.CounterVec
	metricValues       map[string]float64
}

func readCSV(input io.Reader) ([]*awsBillingElement, error) {
	r := csv.NewReader(input)

	pos := map[string]int{
		"RecordType": -1,
	}

	elems := []*awsBillingElement{}

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		// get first line
		if pos["RecordType"] == -1 {
			for i, field := range record {
				pos[field] = i
			}
			continue
		}

		// skip if not a linked line item
		if record[pos["RecordType"]] != "LinkedLineItem" {
			continue
		}

		costs, err := strconv.ParseFloat(
			record[pos["TotalCost"]],
			64,
		)
		if err != nil {
			log.Warnf("Couldn't parse consts float: %s", err)
			continue
		}

		elems = append(elems, &awsBillingElement{
			ProjectID:   record[pos["LinkedAccountId"]],
			ServiceName: record[pos["ProductCode"]],
			Costs:       costs,
			Currency:    record[pos["CurrencyCode"]],
		})
	}
	return reduceElementsByFunc(elems, groupByProjectIDServiceCurrency), nil
}

func reduceElementsByFunc(elementsIn []*awsBillingElement, fnKey func(*awsBillingElement) string) []*awsBillingElement {
	keyMap := map[string]*awsBillingElement{}
	elementsOut := []*awsBillingElement{}

	for _, elem := range elementsIn {
		key := fnKey(elem)
		if groupElem, ok := keyMap[key]; !ok {
			e := &awsBillingElement{
				ProjectID:   elem.ProjectID,
				ProjectName: elem.ProjectName,
				ServiceName: elem.ServiceName,
				Costs:       elem.Costs,
				Currency:    elem.Currency,
			}
			elementsOut = append(elementsOut, e)
			keyMap[key] = e
		} else {
			groupElem.Costs = groupElem.Costs + elem.Costs
		}
	}
	return elementsOut
}

func groupByProjectIDServiceCurrency(e *awsBillingElement) string {
	return fmt.Sprintf(
		"%s-%s-%s",
		e.ProjectID,
		e.ServiceName,
		e.Currency,
	)
}

func NewAWSBilling(metric *prometheus.CounterVec, bucketName, region, rootAccountID, accountMapString, ownerTag, projectIDTag string) *AWSBilling {
	accountMap := map[AccountID]AccountName{}
	accountMapParts := strings.Split(accountMapString, ",")
	for _, mapping := range accountMapParts {
		mappingParts := strings.Split(mapping, "=")
		if len(mappingParts) != 2 {
			continue
		}
		accountMap[AccountID(mappingParts[0])] = AccountName(mappingParts[1])
	}

	for id, name := range accountMap {
		log.With("account_id", id).With("account_name", name).Debug("manual account mapping set up")
	}

	return &AWSBilling{
		MetricMonthlyCosts:      metric,
		BucketName:              bucketName,
		Region:                  region,
		OwnerTag:                ownerTag,
		ProjectIDTag:            projectIDTag,
		rootAccountID:           rootAccountID,
		metricValues:            map[string]float64{},
		accountNameByIDOverride: accountMap,
		time:                    &realClock{},
	}
}

func (a *AWSBilling) getAccountPath(ctx context.Context, svc *organizations.Organizations, ac *Account, accountMap map[AccountID]*Account) ([]string, error) {
	// we are at the root
	if ac.Type == AccountTypeOrganization {
		return []string{}, nil
	}

	// if no parent is known find one through API
	if ac.Parent == "" {
		resp, err := svc.ListParentsWithContext(ctx, &organizations.ListParentsInput{ChildId: aws.String(string(ac.ID))})
		if err != nil {
			return nil, fmt.Errorf("error listing parents of %s: %s", ac.ID, err)
		}
		if len(resp.Parents) == 0 {
			return []string{}, nil
		}
		if len(resp.Parents) != 1 {
			return nil, fmt.Errorf("expected only a single parent %+v", resp.Parents)
		}
		ac.Parent = AccountID(*resp.Parents[0].Id)
	}

	// check if parent is already existing
	parent, ok := accountMap[ac.Parent]
	if !ok {
		parent = &Account{ID: ac.Parent}
		if strings.HasPrefix(string(ac.Parent), "r-") {
			resp, err := svc.DescribeOrganizationWithContext(ctx, &organizations.DescribeOrganizationInput{})
			if err != nil {
				return nil, err
			}
			parent.Type = AccountTypeOrganization
			parent.Name = AccountName(strings.Split(*resp.Organization.MasterAccountEmail, "@")[1])
		} else if strings.HasPrefix(string(ac.Parent), "ou-") {
			parent.Type = AccountTypeOrganizationUnit
			resp, err := svc.DescribeOrganizationalUnitWithContext(ctx, &organizations.DescribeOrganizationalUnitInput{OrganizationalUnitId: aws.String(string(ac.Parent))})
			if err != nil {
				return nil, err
			}
			parent.ID = AccountID(*resp.OrganizationalUnit.Id)
			parent.Name = AccountName(*resp.OrganizationalUnit.Name)
			parent.Type = AccountTypeOrganizationUnit
		} else {
			return nil, fmt.Errorf("unkown parent id: %s", ac.Parent)
		}
		accountMap[parent.ID] = parent
	}

	result, err := a.getAccountPath(ctx, svc, parent, accountMap)
	if err != nil {
		return nil, err
	}

	return append(result, string(parent.Name)), nil

	/*
		if err := svc.ListParentsPages(ctx, &organizations.ListParentsInput{}, func(resp *organizations.ListParentsOutput, _ bool) bool {
			for _, parent := range resp.Parents{
			}
			return true
		}
		}); err != nil {
			log.Warnf("error finding parent for project %s: %s", ac.ID, err)
		}
	*/

}

func (a *AWSBilling) getAccountNameByIDAPI(ctx context.Context) (map[AccountID]*Account, error) {
	session, err := a.awsSession()
	if err != nil {
		return nil, err
	}
	svc := organizations.New(session, a.awsConfig())

	accountMap := make(map[AccountID]*Account)

	if err := svc.ListAccountsPagesWithContext(ctx, &organizations.ListAccountsInput{}, func(resp *organizations.ListAccountsOutput, _ bool) bool {
		for _, account := range resp.Accounts {
			ac := &Account{
				ID:   AccountID(*account.Id),
				Name: AccountName(*account.Name),
			}

			// resolve tags
			if err := svc.ListTagsForResourcePagesWithContext(ctx, &organizations.ListTagsForResourceInput{ResourceId: aws.String(*account.Id)}, func(resp *organizations.ListTagsForResourceOutput, _ bool) bool {
				for _, tag := range resp.Tags {
					if *tag.Key == a.ProjectIDTag {
						ac.Name = AccountName(*tag.Value)
					}
					if *tag.Key == a.OwnerTag {
						ac.Owner = AccountOwner(*tag.Value)
					}
				}
				return true
			}); err != nil {
				log.Warnf("error listing tags for project %s: %s", ac.ID, err)
			}

			// find position in the organization
			if path, err := a.getAccountPath(ctx, svc, ac, accountMap); err != nil {
				log.Warnf("error building account path for project %s: %s", ac.ID, err)
			} else {
				ac.Path = AccountPath(strings.Join(path, "/"))
			}
			accountMap[ac.ID] = ac
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("error listing project in organization: %s", err)
	}

	for key, value := range accountMap {
		if value.Type != AccountTypeProject {
			continue
		}
		log.With("account_id", key).
			With("account_name", value.Name).
			With("owner", value.Owner).
			With("path", value.Path).
			Debug("account mapping from organizations API")
	}

	return accountMap, nil
}

func (a *AWSBilling) AccountByID(id AccountID) *Account {
	ctx := context.Background()

	a.accountNameByIDAPILock.Lock()
	defer a.accountNameByIDAPILock.Unlock()

	// update cache of API based mapping
	if a.accountNameByIDAPI == nil || a.time.Now().Add(-time.Hour).After(a.accountNameByIDAPILastUpdate) {
		if m, err := a.getAccountNameByIDAPI(ctx); err != nil {
			log.Warnf("couldn't retrieve list of accounts: %s", err)
		} else {
			a.accountNameByIDAPI = m
			a.accountNameByIDAPILastUpdate = a.time.Now()
		}
	}

	var account *Account

	// check if we have an API based account name
	if val, ok := a.accountNameByIDAPI[id]; ok {
		account = val
	}

	// see if an override has been defined
	if val, ok := a.accountNameByIDOverride[id]; ok {
		if account == nil {
			account = &Account{ID: AccountID(id)}
		}
		account.Name = val
	}

	if account == nil {
		return &Account{
			ID:   AccountID(id),
			Name: AccountName(fmt.Sprintf("unknown-%s", id)),
		}
	}

	return account
}

func (a *AWSBilling) RootAccountID(ctx context.Context) (string, error) {
	if a.rootAccountID != "" {
		return a.rootAccountID, nil
	}

	session, err := a.awsSession()
	if err != nil {
		return "", err
	}
	svc := sts.New(session, a.awsConfig())

	ci, err := svc.GetCallerIdentityWithContext(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return *ci.Account, nil
}

func (a *AWSBilling) awsSession() (*session.Session, error) {
	return session.NewSession()
}

func (a *AWSBilling) awsConfig() *aws.Config {
	return &aws.Config{Region: aws.String(a.Region)}
}

func (a *AWSBilling) Query() error {
	ctx := context.Background()

	session, err := a.awsSession()
	if err != nil {
		return err
	}
	svc := s3.New(session, a.awsConfig())

	rootAccountID, err := a.RootAccountID(ctx)
	if err != nil {
		return fmt.Errorf("Error detecting root account ID: %s", err)
	}

	prefix := fmt.Sprintf("%s-aws-billing-csv-", rootAccountID)
	params := &s3.ListObjectsInput{
		Bucket: aws.String(a.BucketName),
		Prefix: aws.String(prefix),
	}

	var billingObject *s3.Object
	if err := svc.ListObjectsPagesWithContext(ctx, params, func(resp *s3.ListObjectsOutput, _ bool) bool {
		for _, object := range resp.Contents {
			key := *object.Key
			log.Debugf("found report '%s' for '%s'", key, key[len(prefix):len(key)-4])
			if billingObject == nil || strings.Compare(key, *billingObject.Key) > 0 {
				billingObject = object
			}
		}
		return true
	}); err != nil {
		return fmt.Errorf("Error listing AWS bucket: %s", err)
	}

	if billingObject == nil {
		return fmt.Errorf("No billing report for account '%s' found in bucket '%s'", rootAccountID, a.BucketName)
	}

	key := *billingObject.Key
	log.Debugf("use report '%s' for '%s' hash (%s)", key, key[len(prefix):len(key)-4], *billingObject.ETag)

	// lock from here on
	a.ReportsLock.Lock()
	defer a.ReportsLock.Unlock()

	if a.ReportHash == *billingObject.ETag {
		log.Debugf("report '%s' has already been parsed", key)
		return nil
	}
	// TODO: check hash

	billingObjectContent, err := svc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(a.BucketName),
		Key:    billingObject.Key,
	})
	if err != nil {
		return fmt.Errorf("Error download billing report '%s': %s", *billingObject.Key, err)
	}

	billingElements, err := readCSV(billingObjectContent.Body)
	if err != nil {
		return fmt.Errorf("Error parsing CSV billing report '%s': %s", *billingObject.Key, err)
	}
	err = billingObjectContent.Body.Close()
	if err != nil {
		return err
	}

	for _, elem := range billingElements {
		projectID := elem.ProjectID
		project := a.AccountByID(AccountID(projectID))
		elem.ProjectName = projectID

		m := a.MetricMonthlyCosts.WithLabelValues(
			"aws",
			elem.Currency,
			string(project.Name),
			elem.ServiceName,
			string(project.Path),
			string(project.Owner),
		)
		key := groupByProjectIDServiceCurrency(elem)
		if _, ok := a.metricValues[key]; !ok {
			a.metricValues[groupByProjectIDServiceCurrency(elem)] = 0
		}
		value := elem.Costs
		m.Add(value - a.metricValues[key])
		a.metricValues[key] = value
		log.Debugf("%+#v", elem)
	}
	a.ReportHash = *billingObject.ETag
	return nil
}

func (a *AWSBilling) Test() error {
	return a.Query()
}

func (a *AWSBilling) String() string {
	rootAccountID, err := a.RootAccountID(context.Background())
	if err != nil {
		rootAccountID = "unknown"
	}

	return fmt.Sprintf("AWS Billing on root account '%s' in bucket '%s'", rootAccountID, a.BucketName)
}
