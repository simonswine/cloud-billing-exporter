// Sample storage-quickstart creates a Google Cloud Storage bucket.
package main

import (
//"github.com/prometheus/client_golang/prometheus"
//"github.com/prometheus/common/log"
)

type AWSBillingCollector struct {
	//AWSBilling *aws.AWSBilling
}

func NewAWSBillingCollector(bucketName string, reportPrefix string) *AWSBillingCollector {
	return &AWSBillingCollector{}
}

/*
func (g AWSBillingCollector) Describe(ch chan<- *prometheus.Desc) {
	g.AWSBilling.MetricMonthlyCosts.Describe(ch)
}

// Collect implements the prometheus.Collector interface.
func (g AWSBillingCollector) Collect(ch chan<- prometheus.Metric) {
	err := g.AWSBilling.Query()
	if err != nil {
		log.Warn("not able to query: ", err)
	}
	g.AWSBilling.MetricMonthlyCosts.Collect(ch)
}
*/
