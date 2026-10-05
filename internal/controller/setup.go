package controller

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Options configures which workloads the operator handles.
type Options struct {
	// WatchNamespaces restricts the operator to these namespaces. Empty means all.
	WatchNamespaces []string
	// EnableDeployments also handles Deployments labeled ecr-tagger.io/enabled=true.
	EnableDeployments       bool
	MaxConcurrentReconciles int
	DefaultRegion           string
	ECRFactory              taggerFactory
}

// ParseNamespaces splits a comma separated namespace list, ignoring blanks.
func ParseNamespaces(value string) []string {
	var namespaces []string
	for _, ns := range strings.Split(value, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces
}

// CacheOptions limits what the manager caches: only the watched namespaces and,
// for Deployments, only the opted-in ones (filtered by the API server).
func CacheOptions(opts Options) cache.Options {
	cacheOpts := cache.Options{}
	if len(opts.WatchNamespaces) > 0 {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range opts.WatchNamespaces {
			cacheOpts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	if opts.EnableDeployments {
		deployment := &unstructured.Unstructured{}
		deployment.SetGroupVersionKind(deploymentGVK)
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			deployment: {Label: labels.SelectorFromSet(labels.Set{EnabledLabelKey: "true"})},
		}
	}
	return cacheOpts
}

// SetupControllers registers the Rollout controller and, when enabled, the Deployment controller.
func SetupControllers(mgr ctrl.Manager, opts Options) error {
	kinds := []WorkloadKind{RolloutKind{}}
	if opts.EnableDeployments {
		kinds = append(kinds, DeploymentKind{})
	}

	for _, kind := range kinds {
		reconciler := &WorkloadReconciler{
			Client:                  mgr.GetClient(),
			APIReader:               mgr.GetAPIReader(),
			Scheme:                  mgr.GetScheme(),
			ECRFactory:              opts.ECRFactory,
			Recorder:                mgr.GetEventRecorderFor("rollout-ecr-tagger"),
			OperatorName:            "rollout-ecr-tagger",
			DefaultRegion:           opts.DefaultRegion,
			Kind:                    kind,
			MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
		}
		if err := reconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("create %s controller: %w", kind.GVK().Kind, err)
		}
	}
	return nil
}
