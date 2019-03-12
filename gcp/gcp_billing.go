package gcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"golang.org/x/net/context"
	"google.golang.org/api/iterator"
)

const DateFormat = "2006-01-02"
const ReportsPerMonth = 32

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type gcpBillingMeasurements struct {
	Unit          string
	Sum           string
	MeasurementID string
}

type gcpBillingCost struct {
	Amount   string
	Currency string
	CreditID string
	Value    float64
}

type gcpBillingElement struct {
	ProjectName  string
	ProjectID    string
	ServiceName  string
	Measurements []gcpBillingMeasurements
	Cost         gcpBillingCost
	Credits      []gcpBillingCost
}

type gcpBillingReport struct {
	Elements []*gcpBillingElement
	Hash     []byte
}

type GCPBilling struct {
	clock        Clock
	BucketName   string
	ReportPrefix string

	ReportsLock        sync.Mutex
	Reports            [ReportsPerMonth]gcpBillingReport
	ReportsMonthPrefix string

	BigQueryProjectID string

	MetricMonthlyCosts *prometheus.CounterVec
	metricValues       map[string]float64
	resourcesMetadata  *resourcesMetadata
}

func NewGCPBillingBucket(metric *prometheus.CounterVec, bucketName, reportPrefix, ownerLabel string) *GCPBilling {
	return &GCPBilling{
		MetricMonthlyCosts: metric,
		BucketName:         bucketName,
		ReportPrefix:       reportPrefix,
		resourcesMetadata:  newResourcesMetadata().WithOwnerLabel(ownerLabel),
		clock:              realClock{},
		metricValues:       map[string]float64{},
	}
}

func NewGCPBillingBigQuery(metric *prometheus.CounterVec, bigQueryProjectID string) *GCPBilling {
	return &GCPBilling{
		MetricMonthlyCosts: metric,
		BigQueryProjectID:  bigQueryProjectID,
		clock:              realClock{},
		metricValues:       map[string]float64{},
	}
}

func (g *GCPBilling) filterLastTwoMonths() []string {
	now := g.clock.Now()
	currentYear, currentMonth, _ := now.Date()

	lastMonth := currentMonth - 1
	lastMonthsYear := currentYear

	if currentMonth == time.January {
		lastMonth = time.December
		lastMonthsYear = currentYear - 1
	}

	return []string{
		fmt.Sprintf("%s-%04d-%02d-", g.ReportPrefix, currentYear, currentMonth),
		fmt.Sprintf("%s-%04d-%02d-", g.ReportPrefix, lastMonthsYear, lastMonth),
	}
}

// simplify service key
func (e *gcpBillingElement) GetServiceName() string {
	if e.ServiceName != "" {
		return e.ServiceName
	}

	if len(e.Measurements) != 1 {
		return "misc"
	}

	service := e.Measurements[0].MeasurementID
	parts := strings.Split(service, "/")
	if len(parts) >= 3 && parts[1] == "services" {
		return parts[2]
	}

	return service
}

func (e *gcpBillingElement) GetValue() float64 {
	if e.Cost.Amount != "" {
		value, err := strconv.ParseFloat(e.Cost.Amount, 64)
		if err != nil {
			log.Warnf("failed to convert '%s' to float: %v", e.Cost.Amount, err)
		} else {
			return value
		}
	}

	return e.Cost.Value
}

func reduceElementsByFunc(elementsIn []*gcpBillingElement, fnKey func(*gcpBillingElement) string) []*gcpBillingElement {
	keyMap := map[string]*gcpBillingElement{}
	elementsOut := []*gcpBillingElement{}

	for _, elem := range elementsIn {
		key := fnKey(elem)
		if groupElem, ok := keyMap[key]; !ok {
			e := &gcpBillingElement{
				ProjectID:   elem.ProjectID,
				ProjectName: elem.ProjectName,
				ServiceName: elem.GetServiceName(),
				Cost: gcpBillingCost{
					Currency: elem.Cost.Currency,
					Value:    elem.GetValue(),
				},
			}
			elementsOut = append(elementsOut, e)
			keyMap[key] = e
		} else {
			groupElem.Cost.Value = groupElem.Cost.Value + elem.GetValue()
		}
	}
	return elementsOut
}

func groupByProjectIDServiceCurrency(e *gcpBillingElement) string {
	return fmt.Sprintf(
		"%s-%s-%s",
		e.ProjectID,
		e.GetServiceName(),
		e.Cost.Currency,
	)
}

func reduceElementsByProjectIDServiceCurrency(elementsIn []*gcpBillingElement) []*gcpBillingElement {
	return reduceElementsByFunc(elementsIn, groupByProjectIDServiceCurrency)
}

func (g *GCPBilling) getReportFile(ctx context.Context, bucket *storage.BucketHandle, objectAttrs *storage.ObjectAttrs) {
	lengthName := len(objectAttrs.Name)
	if lengthName < 8 {
		log.Warnf("invalid report filename: %s", objectAttrs.Name)
		return
	}

	i, err := strconv.Atoi(objectAttrs.Name[lengthName-7 : lengthName-5])
	if err != nil {
		log.Warnf("invalid report filename '%s': %s", objectAttrs.Name, err)
		return
	}
	i = i - 1

	if reflect.DeepEqual(g.Reports[i].Hash, objectAttrs.MD5) {
		log.Debugf("report '%s' already parsed in cache", objectAttrs.Name)
		return
	}

	reader, err := bucket.Object(objectAttrs.Name).NewReader(ctx)
	if err != nil {
		log.Warnf("failed to read report '%s': %v", objectAttrs.Name, err)
		return
	}
	defer reader.Close()
	err = json.NewDecoder(reader).Decode(&g.Reports[i].Elements)
	if err != nil {
		log.Warnf("failed to parse report JSON '%s': %v", objectAttrs.Name, err)
		return
	}

	g.Reports[i].Elements = reduceElementsByProjectIDServiceCurrency(g.Reports[i].Elements)
	g.Reports[i].Hash = objectAttrs.MD5

	for _, elem := range g.Reports[i].Elements {
		log.With(
			"currency",
			elem.Cost.Currency,
		).With(
			"services",
			elem.GetServiceName(),
		).With(
			"account",
			elem.ProjectID,
		).With(
			"costs",
			elem.GetValue(),
		).Debug(
			objectAttrs.Name,
		)
	}
}

func (g *GCPBilling) GetReports(ctx context.Context) error {

	// create a GCS client.
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create client: %v", err)
	}

	bucket := client.Bucket(g.BucketName)

	var it *storage.ObjectIterator
	var bucketAttrs *storage.ObjectAttrs
	var prefix string
	for _, prefix = range g.filterLastTwoMonths() {
		log.Debugf("looking for reports in bucket '%s' with prefix '%s'", g.BucketName, prefix)
		it = bucket.Objects(ctx, &storage.Query{Prefix: prefix})
		bucketAttrs, err = it.Next()
		if err == iterator.Done {
			bucketAttrs = nil
			continue
		}
		if err != nil {
			return fmt.Errorf("Failed to list objects: %v", err)
		}
		break
	}

	if bucketAttrs == nil {
		log.Warnf("No reports of this or last month found in bucket '%s' with prefix '%s'", g.BucketName, g.ReportPrefix)
		return nil
	}

	if g.ReportsMonthPrefix != prefix {
		log.Debugf("reports prefix changed -> clear cache (old: %s, new: %s)", g.ReportsMonthPrefix, prefix)
		g.ReportsMonthPrefix = prefix
		g.Reports = [ReportsPerMonth]gcpBillingReport{}
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func(attr *storage.ObjectAttrs) {
		defer wg.Done()
		g.getReportFile(ctx, bucket, attr)
	}(bucketAttrs)

	// list objects
	for {
		bucketAttrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return fmt.Errorf("Failed to list objects: %v", err)
		}
		wg.Add(1)
		go func(attr *storage.ObjectAttrs) {
			defer wg.Done()
			g.getReportFile(ctx, bucket, attr)
		}(bucketAttrs)
	}

	wg.Wait()
	return nil
}

func (g *GCPBilling) Test() error {
	return g.Query()
}

func (g *GCPBilling) Query() error {

	g.BigQueryProjectID = "jetstack-gke"
	ctx := context.Background()
	client, err := bigquery.NewClient(ctx, g.BigQueryProjectID)
	if err != nil {
		return err
	}

	q := client.Query(`
SELECT
  project.id,
  invoice.month,
  currency,
  service.description,
  SUM(cost) as cost
FROM ` + "`jetstack-gke.jetstack_billing.gcp_billing_export_v1_00C1A5_231D52_42D4E3`" + `
GROUP BY project.id, invoice.month, currency, service.description
ORDER BY cost DESC
;
`)
	it, err := q.Read(ctx)
	if err != nil {
		return err
	}

	for {
		var values []bigquery.Value
		err := it.Next(&values)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		fmt.Println(values)
	}

	return nil
}

func (g *GCPBilling) OldQuery() error {
	ctx := context.Background()

	// lock from here on
	g.ReportsLock.Lock()
	defer g.ReportsLock.Unlock()

	// update from GCS buckets
	err := g.GetReports(ctx)
	if err != nil {
		return err
	}

	// update metadata if neccessary
	if err := g.resourcesMetadata.update(ctx); err != nil {
		log.Warnf("error updating resource metadata: %s", err)
	}

	// gather all costs
	elems := []*gcpBillingElement{}
	for _, report := range g.Reports {
		elems = append(elems, report.Elements...)
	}

	// group them
	elems = reduceElementsByProjectIDServiceCurrency(elems)

	// write them into the metrics
	for _, elem := range elems {
		var owner, path string
		metadata := g.resourcesMetadata.projectByID(elem.ProjectID)
		if metadata != nil {
			owner = metadata.owner
			path = strings.Join(g.resourcesMetadata.path(metadata), "/")
		}

		m := g.MetricMonthlyCosts.WithLabelValues(
			"gcp",
			elem.Cost.Currency,
			elem.ProjectID,
			elem.GetServiceName(),
			path,
			owner,
		)
		key := groupByProjectIDServiceCurrency(elem)
		if _, ok := g.metricValues[key]; !ok {
			g.metricValues[groupByProjectIDServiceCurrency(elem)] = 0
		}
		value := elem.GetValue()
		delta := value - g.metricValues[key]
		if delta < 0 {
			log.With("project", elem.ProjectID).With("service_name", elem.GetServiceName()).Warnf("costs are falling by: '%f'", delta)
			continue
		}
		m.Add(delta)
		g.metricValues[key] = value
	}

	return nil
}

func (g *GCPBilling) String() string {
	return fmt.Sprintf("GCP Billing in bucket '%s'", g.BucketName)
}
