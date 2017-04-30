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

const AppName = "gcp_billing_exporter"
const AppNameLong = "GCP Billing Exporter"

func main() {
	var (
		reportPrefix  = flag.String("billing.report-prefix", "my-billing", "Report name prefix.")
		bucketName    = flag.String("billing.bucket-name", "my-billing", "Bucket name that stores JSON billing reports.")
		showVersion   = flag.Bool("version", false, "Print version information.")
		listenAddress = flag.String("web.listen-address", ":9660", "Address on which to expose metrics and web interface.")
		metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print(AppName))
		os.Exit(0)
	}

	log.Infoln("Starting", AppName, version.Info())
	log.Infoln("Build context", version.BuildContext())

	b := NewGCPBilling()
	b.BucketName = *bucketName
	b.ReportPrefix = *reportPrefix

	if err := prometheus.Register(GCPBillingCollector{GCPBilling: b}); err != nil {
		log.Fatalf("Couldn't register collector: %s", err)
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
