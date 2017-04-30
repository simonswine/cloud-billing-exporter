// Sample storage-quickstart creates a Google Cloud Storage bucket.
package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

type GCPBillingCollector struct {
	GCPBilling *GCPBilling
}

const Namespace = "cloud_provider"

func (g GCPBillingCollector) Describe(ch chan<- *prometheus.Desc) {
	g.GCPBilling.metricMonthlyCosts.Describe(ch)
}

// Collect implements the prometheus.Collector interface.
func (g GCPBillingCollector) Collect(ch chan<- prometheus.Metric) {
	err := g.GCPBilling.Query()
	if err != nil {
		log.Warn("not able to query: ", err)
	}
	g.GCPBilling.metricMonthlyCosts.Collect(ch)
}
