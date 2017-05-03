package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

const AppName = "cloud_billing_exporter"
const AppNameLong = "Cloud Billing Exporter"
const Namespace = "cloud"

func main() {
	var (
		gcpReportPrefix = flag.String("gcp-billing.report-prefix", "my-billing", "Report name prefix for GCP billing.")
		gcpBucketName   = flag.String("gcp-billing.bucket-name", "my-billing", "Bucket name that stores GCP JSON billing reports.")
		awsRegion       = flag.String("aws-billing.region", "eu-west-1", "Region name for AWS billing bucket.")
		awsBucketName   = flag.String("aws-billing.bucket-name", "my-billing", "Bucket name that stores AWS billing reports.")
		showVersion     = flag.Bool("version", false, "Print version information.")
		listenAddress   = flag.String("web.listen-address", ":9660", "Address on which to expose metrics and web interface.")
		metricsPath     = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print(AppName))
		os.Exit(0)
	}

	log.Infoln("Starting", AppName, version.Info())
	log.Infoln("Build context", version.BuildContext())

	gcpCollector := NewGCPBillingCollector(*gcpBucketName, *gcpReportPrefix)

	_ = NewAWSBillingCollector(*awsRegion, *awsBucketName)

	if err := prometheus.Register(gcpCollector); err != nil {
		log.Errorf("Couldn't register collector: %s", err)
	}

	handler := promhttp.HandlerFor(prometheus.DefaultGatherer,
		promhttp.HandlerOpts{
			ErrorLog:      log.NewErrorLogger(),
			ErrorHandling: promhttp.ContinueOnError,
		})
	http.Handle(*metricsPath, prometheus.InstrumentHandler("prometheus", handler))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>` + AppNameLong + `</title></head>
			<body>
			<h1>` + AppNameLong + `</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})

	log.Infoln("Listening on", *listenAddress)
	err := http.ListenAndServe(*listenAddress, nil)
	if err != nil {
		log.Fatal(err)
	}
}
