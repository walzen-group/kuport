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
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/walzen-group/kuport/internal/agent"
	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
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
	showVersion bool
}

// parseFlags resolves flags against env fallbacks. getenv is injected so the
// resolution is testable without touching the process environment.
func parseFlags(args []string, getenv func(string) string) config {
	fs := flag.NewFlagSet("kuport-agent", flag.ExitOnError)
	nodeName := fs.String("node-name", getenv("NODE_NAME"),
		"this node's name; the DaemonSet sets it from spec.nodeName via the downward API")
	logLevel := fs.String("log-level", envOr(getenv, "LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	resync := fs.Duration("resync", durOr(getenv, "RESYNC", 5*time.Minute), "informer resync period, the level-driven safety net")
	healthAddr := fs.String("health-addr", envOr(getenv, "HEALTH_ADDR", ":8081"), "address the health and readiness probes bind to")
	showVersion := fs.Bool("version", false, "print the version and exit")
	_ = fs.Parse(args)

	return config{
		nodeName:    *nodeName,
		logLevel:    *logLevel,
		resync:      *resync,
		healthAddr:  *healthAddr,
		showVersion: *showVersion,
	}
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
		Cache:                  cache.Options{SyncPeriod: &cfg.resync},
		Metrics:                metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	handles, err := datapath.NewHandles()
	if err != nil {
		return fmt.Errorf("open host handles: %w", err)
	}
	dp := agent.HostDatapath{Handles: handles}

	r := &agent.Reconciler{
		Client:   mgr.GetClient(),
		NodeName: cfg.nodeName,
		DP:       dp,
		Host:     agent.NewHost(mgr.GetAPIReader(), handles.NL),
		Log:      log.WithName("reconciler"),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up reconciler: %w", err)
	}

	if err := mgr.Add(agent.TeardownRunnable{DP: dp, Log: log.WithName("shutdown")}); err != nil {
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
