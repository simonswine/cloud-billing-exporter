// Sample storage-quickstart creates a Google Cloud Storage bucket.
package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"

	"github.com/jetstack-experimental/cloud-billing-exporter/gcp"
)

type GCPBillingCollector struct {
	GCPBilling *gcp.GCPBilling
}

func NewGCPBillingCollector(bucketName string, reportPrefix string) *GCPBillingCollector {
	g := gcp.NewGCPBilling()
	g.BucketName = bucketName
	g.ReportPrefix = reportPrefix
	g.MetricMonthlyCosts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName(Namespace, "billing", "monthly_costs"),
			Help: "Billed costs per calendar month.",
		},
		[]string{"cloud", "currency", "account", "service"},
	)

	return &GCPBillingCollector{GCPBilling: g}
}

func (g GCPBillingCollector) Describe(ch chan<- *prometheus.Desc) {
	g.GCPBilling.MetricMonthlyCosts.Describe(ch)
}

// Collect implements the prometheus.Collector interface.
func (g GCPBillingCollector) Collect(ch chan<- prometheus.Metric) {
	// TODO: do this only every hour, as billing is not updated that often by GCP
	err := g.GCPBilling.Query()
	if err != nil {
		log.Warn("not able to query: ", err)
	}
	g.GCPBilling.MetricMonthlyCosts.Collect(ch)
}
