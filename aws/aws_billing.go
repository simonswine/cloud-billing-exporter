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
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

type Clock interface {
	Now() time.Time
}

type awsBillingElement struct {
	ProjectName string
	ProjectID   string
	ServiceName string
	Costs       float64
	Currency    string
}

type AWSBilling struct {
	time Clock

	BucketName string
	Region     string
	AccountMap map[string]string

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
	accountMap := map[string]string{}
	accountMapParts := strings.Split(accountMapString, ",")
	for _, mapping := range accountMapParts {
		mappingParts := strings.Split(mapping, "=")
		if len(mappingParts) != 2 {
			continue
		}
		accountMap[mappingParts[0]] = mappingParts[1]
	}

	log.Infof("%+#v from '%s'", accountMap, accountMapString)

	return &AWSBilling{
		MetricMonthlyCosts: metric,
		BucketName:         bucketName,
		Region:             region,
		rootAccountID:      rootAccountID,
		metricValues:       map[string]float64{},
		AccountMap:         accountMap,
	}
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
		if val, ok := a.AccountMap[projectID]; ok {
			projectID = val
			elem.ProjectName = val
		}

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
