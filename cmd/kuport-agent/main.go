// Command kuport-agent is the per-node daemon. One runs on every node in the
// DaemonSet; each watches the same objects, computes its own node's desired
// state, and applies it. There is no central controller and no leader election:
// the agents agree because the reconcile is deterministic, not because they
// coordinate.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/walzen-group/kuport/internal/agent"
	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
	"github.com/walzen-group/kuport/internal/metrics"
)

// version is injected at build time with -X main.version=<v>. It defaults to
// dev so a plain `go run` still answers --version.
var version = "dev"

func main() {
	cfg := parseFlags(os.Args[1:], os.Getenv)
	if cfg.showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "kuport-agent:", err)
		os.Exit(1)
	}
}

// config is the resolved runtime configuration, each field a flag with an env
// fallback so the DaemonSet may set either.
type config struct {
	nodeName    string
	logLevel    string
	resync      time.Duration
	healthAddr  string
	linkPort    uint16
	metrics     metrics.BindConfig
	showVersion bool
}

// parseFlags resolves flags against env fallbacks. getenv is injected so the
// resolution is testable without touching the process environment.
func parseFlags(args []string, getenv func(string) string) config {
	fs := flag.NewFlagSet("kuport-agent", flag.ExitOnError)
	nodeName := fs.String("node-name", getenv("NODE_NAME"),
		"this node's name; the DaemonSet sets it from spec.nodeName via the downward API")
	logLevel := fs.String("log-level", envOr(getenv, "LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	resync := fs.Duration("resync", durOr(getenv, "RESYNC", 5*time.Minute), "informer resync period, and how long an unchanged datapath goes before the agent applies it again")
	healthAddr := fs.String("health-addr", envOr(getenv, "HEALTH_ADDR", ":8081"), "address the health and readiness probes bind to")
	linkPort := fs.Uint("link-port", uintOr(getenv, "LINK_PORT", 4790),
		"UDP port of every return link; must differ from the CNI's tunnel port, 8472 for Cilium")
	metricsBind := fs.String("metrics-bind", getenv("METRICS_BIND"),
		"where /metrics listens: empty for the node's InternalIP (HOST_IP), an interface name such as wt0, or off")
	metricsPort := fs.Uint("metrics-port", uintOr(getenv, "METRICS_PORT", 9790), "TCP port of /metrics")
	showVersion := fs.Bool("version", false, "print the version and exit")
	_ = fs.Parse(args)

	return config{
		nodeName:   *nodeName,
		logLevel:   *logLevel,
		resync:     *resync,
		healthAddr: *healthAddr,
		linkPort:   uint16(*linkPort),
		metrics: metrics.BindConfig{
			Bind:   *metricsBind,
			HostIP: getenv("HOST_IP"),
			Port:   uint16(*metricsPort),
		},
		showVersion: *showVersion,
	}
}

// uintOr parses a port from the environment, falling back to def when unset,
// unparseable or outside 1-65535.
func uintOr(getenv func(string) string, key string, def uint) uint {
	if v := getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil && n > 0 {
			return uint(n)
		}
	}
	return def
}

// envOr returns the env value when set, else the default.
func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// durOr parses a duration from the environment, falling back to def when unset
// or unparseable.
func durOr(getenv func(string) string, key string, def time.Duration) time.Duration {
	if v := getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// run builds the manager, wires the reconciler and the health server, and blocks
// until a signal cancels the context.
func run(cfg config) error {
	if cfg.nodeName == "" {
		return fmt.Errorf("--node-name (or NODE_NAME) is required")
	}

	log := newLogger(cfg.logLevel)
	ctrl.SetLogger(log)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		v1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("build scheme: %w", err)
		}
	}

	// Leader election is deliberately left at its default (off): every agent
	// reconciles its own node independently, so electing one would be wrong.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: cfg.healthAddr,
		Cache: cache.Options{
			SyncPeriod: &cfg.resync,
			// Every agent caches every pod for the Egress reconcile; the
			// transform keeps only the fields it reads.
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {Transform: func(o any) (any, error) {
					if p, ok := o.(*corev1.Pod); ok {
						return agent.PodFields(p), nil
					}
					return o, nil
				}},
			},
		},
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	handles, err := datapath.NewHandles()
	if err != nil {
		return fmt.Errorf("open host handles: %w", err)
	}
	// The real handles' nftables connection also lists rules with counters.
	reader, _ := handles.NFT.(datapath.CounterReader)
	counters := datapath.NewCounters(reader)
	dp := agent.HostDatapath{Handles: handles, Counters: counters}

	r := &agent.Reconciler{
		Client:    mgr.GetClient(),
		NodeName:  cfg.nodeName,
		DP:        dp,
		Host:      agent.NewHost(mgr.GetAPIReader(), handles.NL),
		LinkPort:  cfg.linkPort,
		Repair:    cfg.resync,
		Log:       log.WithName("reconciler"),
		Totals:    counters,
		Conntrack: datapath.HostConntrack,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up reconciler: %w", err)
	}

	// /metrics serves controller-runtime's registry, the controller's own
	// series included, from listeners on the InternalIP or a named interface.
	// controller-runtime's metrics server stays off: it binds one address.
	if err := metrics.Register(ctrlmetrics.Registry, metrics.New(r)); err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	if err := mgr.Add(&metrics.Server{
		Config:  cfg.metrics,
		Lookup:  func(name string) ([]netip.Addr, error) { return datapath.InterfaceAddrs(handles.NL, name) },
		Handler: mux,
		Log:     log.WithName("metrics"),
	}); err != nil {
		return fmt.Errorf("add metrics server: %w", err)
	}

	if err := mgr.Add(agent.TeardownRunnable{DP: dp, Keep: r.KeepState, Log: log.WithName("shutdown")}); err != nil {
		return fmt.Errorf("add teardown runnable: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("reconciled", func(*http.Request) error {
		if r.Ready() {
			return nil
		}
		return fmt.Errorf("first reconcile not yet complete")
	}); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	log.Info("starting kuport-agent", "version", version, "node", cfg.nodeName, "healthAddr", cfg.healthAddr)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("manager exited: %w", err)
	}
	return nil
}

// newLogger builds an slog-backed logr logger at the requested level. debug maps
// to logr V(1), which is where no-op reconcile passes are logged.
func newLogger(level string) logr.Logger {
	lvl := slog.LevelInfo
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return logr.FromSlogHandler(h)
}
