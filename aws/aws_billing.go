package aws

import (
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

type AccountName string
type AccountID string

type AWSBilling struct {
	time Clock

	BucketName string
	Region     string

	// accountNameByIDOverride contains account name mappings specified
	// manually through CLI arguments (take precedence)
	accountNameByIDOverride map[AccountID]AccountName

	// accountNameByIDCachedAPI contains account name mappings specified
	accountNameByIDAPI           map[AccountID]AccountName
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

func NewAWSBilling(metric *prometheus.CounterVec, bucketName, region, rootAccountID, accountMapString string) *AWSBilling {
	accountMap := map[AccountID]AccountName{}
	accountMapParts := strings.Split(accountMapString, ",")
	for _, mapping := range accountMapParts {
		mappingParts := strings.Split(mapping, "=")
		if len(mappingParts) != 2 {
			continue
		}
		accountMap[AccountID(mappingParts[0])] = AccountName(mappingParts[1])
	}

	log.Infof("%+#v from '%s'", accountMap, accountMapString)

	return &AWSBilling{
		MetricMonthlyCosts:      metric,
		BucketName:              bucketName,
		Region:                  region,
		rootAccountID:           rootAccountID,
		metricValues:            map[string]float64{},
		accountNameByIDOverride: accountMap,
		time:                    &realClock{},
	}
}

func (a *AWSBilling) getAccountNameByIDAPI() (map[AccountID]AccountName, error) {
	svc := organizations.New(a.awsSession(), a.awsConfig())

	accountMap := map[AccountID]AccountName{}

	input := &organizations.ListAccountsInput{}
	for {
		resp, err := svc.ListAccounts(input)
		if err != nil {
			return nil, err
		}
		for _, account := range resp.Accounts {
			accountMap[AccountID(*account.Id)] = AccountName(*account.Name)
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		input.NextToken = resp.NextToken
	}

	log.Infof("%+#v from AWS api", accountMap)

	return accountMap, nil
}

func (a *AWSBilling) AccountNameByID(id AccountID) AccountName {

	// see if an override has been defined
	if val, ok := a.accountNameByIDOverride[id]; ok {
		return val
	}

	// update cache of API based mapping
	if a.accountNameByIDAPI == nil || a.time.Now().Add(-time.Hour).After(a.accountNameByIDAPILastUpdate) {
		if m, err := a.getAccountNameByIDAPI(); err != nil {
			log.Warnf("couldn't retrieve list of accounts: %s", err)
		} else {
			a.accountNameByIDAPI = m
			a.accountNameByIDAPILastUpdate = a.time.Now()
		}
	}

	// check if we have an API based account name
	if val, ok := a.accountNameByIDAPI[id]; ok {
		return val
	}

	return AccountName(fmt.Sprintf("unknown-%s", id))
}

func (a *AWSBilling) RootAccountID() string {
	if a.rootAccountID != "" {
		return a.rootAccountID
	}

	svc := sts.New(a.awsSession(), a.awsConfig())

	ci, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		log.Warnf("Error detecting account ID: %s", err)
		return ""
	}
	return *ci.Account
}

func (a *AWSBilling) awsSession() *session.Session {
	return session.New()
}

func (a *AWSBilling) awsConfig() *aws.Config {
	return &aws.Config{Region: aws.String(a.Region)}
}

func (a *AWSBilling) Query() error {
	svc := s3.New(a.awsSession(), a.awsConfig())

	prefix := fmt.Sprintf("%s-aws-billing-csv-", a.RootAccountID())
	params := &s3.ListObjectsInput{
		Bucket: aws.String(a.BucketName),
		Prefix: aws.String(prefix),
	}

	resp, err := svc.ListObjects(params)
	if err != nil {
		return fmt.Errorf("Error listing AWS bucket: %s", err)
	}

	var billingObject *s3.Object
	for _, object := range resp.Contents {
		key := *object.Key
		log.Debugf("found report '%s' for '%s'", key, key[len(prefix):len(key)-4])
		if billingObject == nil || strings.Compare(key, *billingObject.Key) > 0 {
			billingObject = object
		}
	}

	if billingObject == nil {
		return fmt.Errorf("No billing report for account '%s' found in bucket '%s'", a.RootAccountID(), a.BucketName)
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
		projectID = string(a.AccountNameByID(AccountID(projectID)))
		elem.ProjectName = projectID

		m := a.MetricMonthlyCosts.WithLabelValues(
			"aws",
			elem.Currency,
			projectID,
			elem.ServiceName,
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
	return fmt.Sprintf("AWS Billing on root account '%s' in bucket '%s'", a.RootAccountID(), a.BucketName)
}
