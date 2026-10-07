package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	resultSuccess = "success"
	resultFailure = "failure"
	// resultImmutable counts tags that could not be updated because the
	// repository has tag immutability enabled.
	resultImmutable = "immutable"
)

var tagOperationsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "ecr_tagger_tag_operations_total",
		Help: "Number of ECR tagging attempts, by workload kind, namespace and result.",
	},
	[]string{"kind", "namespace", "result"},
)

func init() {
	metrics.Registry.MustRegister(tagOperationsTotal)
}
