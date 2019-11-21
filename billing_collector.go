package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"

	"github.com/simonswine/cloud-billing-exporter/aws"
	"github.com/simonswine/cloud-billing-exporter/gcp"
)

const AppName = "cloud_billing_exporter"
const AppNameLong = "Cloud Billing Exporter"
const Namespace = "cloud"

type cloudBillingCollector interface {
	Query() error
	Test() error
	String() string
}

type BillingCollector struct {
	awsBilling       *aws.AWSBilling
	AWSRegion        *string
	AWSBucketName    *string
	AWSRootAccountID *int
	AWSAccountMap    *string

	gcpBilling      *gcp.GCPBilling
	GCPReportPrefix *string
	GCPBucketName   *string
	GCPOwnerLabel   *string

	ShowVersion   *bool
	ListenAddress *string
	MetricsPath   *string

	collectors         []cloudBillingCollector
	metricMonthlyCosts *prometheus.CounterVec
}

func (b *BillingCollector) parseFlags() {
	b.GCPReportPrefix = flag.String("gcp-billing.report-prefix", "my-billing", "Report name prefix for GCP billing.")
	b.GCPBucketName = flag.String("gcp-billing.bucket-name", "", "Bucket name that stores GCP JSON billing reports.")
	b.GCPOwnerLabel = flag.String("gcp-billing.owner-label", "owner-base32", "Name of the owner label, which contains the owner in base32 encoding.")

	b.AWSRegion = flag.String("aws-billing.region", "eu-west-1", "Region name for AWS billing bucket.")
	b.AWSBucketName = flag.String("aws-billing.bucket-name", "", "Bucket name that stores AWS billing reports.")
	b.AWSRootAccountID = flag.Int("aws-billing.root-account-id", 0, "Root Account ID.")
	b.AWSAccountMap = flag.String("aws-billing.account-map", "", "Map account IDs to more readable names. Example: 1200000=acme-dev,120001=acme-prod")

	b.ShowVersion = flag.Bool("version", false, "Print version information.")
	b.ListenAddress = flag.String("web.listen-address", ":9660", "Address on which to expose metrics and web interface.")
	b.MetricsPath = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")

	flag.Parse()
}

func (b *BillingCollector) Run() {
	b.parseFlags()

	if *b.ShowVersion {
		fmt.Fprintln(os.Stdout, version.Print(AppName))
		os.Exit(0)
	}

	log.Infoln("Starting", AppName, version.Info())
	log.Infoln("Build context", version.BuildContext())

	b.metricMonthlyCosts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName(Namespace, "billing", "monthly_costs"),
			Help: "Billed costs per calendar month.",
		},
		[]string{"cloud", "currency", "account", "service", "path", "owner"},
	)

	if *b.AWSBucketName != "" {
		var rootAccountID string
		if *b.AWSRootAccountID != 0 {
			rootAccountID = fmt.Sprintf("%d", *b.AWSRootAccountID)
		}
		c := aws.NewAWSBilling(
			b.metricMonthlyCosts,
			*b.AWSBucketName,
			*b.AWSRegion,
			rootAccountID,
			*b.AWSAccountMap,
		)
		if err := c.Test(); err != nil {
			log.Error(err)
		} else {
			b.collectors = append(b.collectors, c)
		}
	}

	if *b.GCPBucketName != "" {
		c := gcp.NewGCPBilling(
			b.metricMonthlyCosts,
			*b.GCPBucketName,
			*b.GCPReportPrefix,
			*b.GCPOwnerLabel,
		)
		if err := c.Test(); err != nil {
			log.Error(err)
		} else {
			b.collectors = append(b.collectors, c)
		}
	}

	if len(b.collectors) == 0 {
		log.Fatal("no working cloud billing collectors found")
	}

	if err := prometheus.Register(b); err != nil {
		log.Fatalf("Couldn't register collector: %s", err)
	}

	handler := promhttp.HandlerFor(prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			ErrorLog:      log.NewErrorLogger(),
			ErrorHandling: promhttp.ContinueOnError,
		})
	http.Handle(*b.MetricsPath, handler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>` + AppNameLong + `</title></head>
			<body>
			<h1>` + AppNameLong + `</h1>
			<p><a href="` + *b.MetricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})

	log.Infoln("Listening on", *b.ListenAddress)
	err := http.ListenAndServe(*b.ListenAddress, nil)
	if err != nil {
		log.Fatal(err)
	}
}

func (b BillingCollector) Describe(ch chan<- *prometheus.Desc) {
	b.metricMonthlyCosts.Describe(ch)
}

func (b BillingCollector) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	for _, c := range b.collectors {
		wg.Add(1)
		go func(c cloudBillingCollector) {
			defer wg.Done()
			err := c.Query()
			if err != nil {
				log.Warnf("Error querying collector (%s): %s", c.String(), err)
			}
		}(c)
	}

	wg.Wait()
	b.metricMonthlyCosts.Collect(ch)
}

func main() {
	b := &BillingCollector{}
	b.Run()
}
