package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/henryd24/kubernetes-operator-tag-images/internal/controller"
	"github.com/henryd24/kubernetes-operator-tag-images/internal/ecr"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		metricsAddr             string
		probeAddr               string
		enableLeaderElection    bool
		maxConcurrentReconciles int
		showVersion             bool
		enableDeployments       bool
		watchNamespaces         string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager.")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 1, "Maximum number of workloads reconciled in parallel, per kind.")
	flag.BoolVar(&enableDeployments, "enable-deployments", false, "Also tag images of Deployments labeled "+controller.EnabledLabelKey+"=true.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "", "Comma separated namespaces to watch. Empty watches all namespaces.")
	flag.BoolVar(&showVersion, "version", false, "Print the operator version and exit.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	controllerOpts := controller.Options{
		WatchNamespaces:         controller.ParseNamespaces(watchNamespaces),
		EnableDeployments:       enableDeployments,
		MaxConcurrentReconciles: maxConcurrentReconciles,
		DefaultRegion:           os.Getenv("AWS_REGION"),
		ECRFactory:              ecr.NewFactory(),
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  controller.CacheOptions(controllerOpts),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ecr-rollout-tagger-operator",
		// Safe because the process exits right after the manager stops; speeds up failover.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := controller.SetupControllers(mgr, controllerOpts); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create controllers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.WithName("setup").Info("starting manager", "version", version,
		"enableDeployments", enableDeployments, "watchNamespaces", controllerOpts.WatchNamespaces)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.WithName("setup").Error(err, "problem running manager")
		os.Exit(1)
	}
}
