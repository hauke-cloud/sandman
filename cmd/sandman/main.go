// Command sandman turns Kubernetes nodes on and off: Wake-on-LAN to start a
// machine, a privileged pod to stop one, and a REST API to ask for either.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
	"github.com/hauke-cloud/sandman/internal/config"
	"github.com/hauke-cloud/sandman/internal/crd"
	"github.com/hauke-cloud/sandman/internal/metrics"
	"github.com/hauke-cloud/sandman/internal/power"
	"github.com/hauke-cloud/sandman/internal/server"
	"github.com/hauke-cloud/sandman/internal/version"
	"github.com/hauke-cloud/sandman/internal/wol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// flag.ErrHelp means -h was passed; the flag package already printed
		// the usage, so exiting quietly is the whole response.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "sandman:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args, os.Stderr)
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)
	// controller-runtime logs through logr; bridging it onto the same handler
	// keeps the controller's log and sandman's own log in one format.
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	logger.Info("starting sandman",
		"version", version.Version(),
		"commit", version.Commit(),
		"listen_address", cfg.ListenAddress,
		"namespace", cfg.Namespace,
		"leader_election", cfg.LeaderElection,
		"install_crds", cfg.InstallCRDs,
		"read_only", cfg.ReadOnly,
		"api_authenticated", cfg.APIToken != "")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register core types: %w", err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register apiextensions types: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register sandman types: %w", err)
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubernetes configuration: %w", err)
	}

	ctx := ctrl.SetupSignalHandler()

	// The CRD is applied before the manager exists, with a direct client. An
	// informer for a type the API server does not serve yet fails outright
	// rather than waiting for it, so this has to finish first.
	if cfg.InstallCRDs {
		bootstrap, err := client.New(restConfig, client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("build bootstrap client: %w", err)
		}
		if err := crd.Install(ctx, bootstrap, logger); err != nil {
			return err
		}
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		// sandman serves its own metrics alongside the API, so the manager's
		// metrics server is off and its registry is gathered from there.
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          cfg.LeaderElection,
		LeaderElectionID:        cfg.LeaderElectionID,
		LeaderElectionNamespace: cfg.Namespace,
		// Releasing the lease on the way out means a rolling update hands over
		// in seconds instead of waiting for the lease to expire.
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// Watching every pod in the cluster to find the handful
				// sandman creates would cost more memory than the rest of the
				// controller put together. Drains read pods straight from the
				// API server instead.
				&corev1.Pod{}: {
					Label: shutdownPodSelector(),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	registry := metrics.NewRegistry()
	instruments := metrics.New(registry)
	registry.MustRegister(metrics.NewDeviceCollector(func(ctx context.Context) ([]v1alpha1.SandmanDevice, error) {
		var devices v1alpha1.SandmanDeviceList
		if err := mgr.GetClient().List(ctx, &devices); err != nil {
			return nil, err
		}
		return devices.Items, nil
	}, logger))

	waker, err := wol.NewSender(cfg.WOLSourceAddress)
	if err != nil {
		return err
	}

	reconciler := &power.Reconciler{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Waker:     waker,
		Namespace: cfg.Namespace,
		Metrics:   instruments,
		Recorder:  mgr.GetEventRecorder("sandman"),
		Logger:    logger,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register controller: %w", err)
	}

	// Readiness has to be answered by every replica, not only the leader --
	// a standby that never reports ready is a standby the Service never sends
	// a request to.
	var cacheReady atomic.Bool
	if err := mgr.Add(everyReplica(func(ctx context.Context) error {
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("cache did not sync")
		}
		cacheReady.Store(true)
		logger.Info("device cache synced")
		<-ctx.Done()
		return nil
	})); err != nil {
		return fmt.Errorf("register cache readiness: %w", err)
	}

	api := server.New(&server.Options{
		Address:     cfg.ListenAddress,
		MetricsPath: cfg.MetricsPath,
		// Both registries are served from one endpoint: sandman's own metrics
		// and controller-runtime's, which carry the reconcile and workqueue
		// series any controller is worth watching by.
		Gatherer: gatherers(registry),
		Client:   mgr.GetClient(),
		Ready:    cacheReady.Load,
		Token:    cfg.APIToken,
		ReadOnly: cfg.ReadOnly,
		Metrics:  instruments,
		Logger:   logger,
	})
	if err := mgr.Add(api); err != nil {
		return fmt.Errorf("register api server: %w", err)
	}

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// everyReplica marks a runnable as not needing leader election. The manager
// assumes the opposite for a bare function, which would leave a standby
// replica's cache-readiness never running -- and so its /readyz never passing.
type everyReplica func(context.Context) error

// Start implements manager.Runnable.
func (r everyReplica) Start(ctx context.Context) error { return r(ctx) }

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (r everyReplica) NeedLeaderElection() bool { return false }

// shutdownPodSelector narrows the pod cache to the pods sandman creates.
func shutdownPodSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{"app.kubernetes.io/managed-by": "sandman"})
}

// gatherers joins sandman's registry with controller-runtime's, so one scrape
// carries both.
func gatherers(own prometheus.Gatherer) prometheus.Gatherer {
	return prometheus.Gatherers{own, ctrlmetrics.Registry}
}
