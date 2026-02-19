package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	cli "github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
}

// Config holds all controller configuration.
type Config struct {
	Port              int
	LogLevel          string
	DrainTaints       []DrainTaint
	EnabledAnnotation string
	RequeueInterval   time.Duration
	RolloutTimeout    time.Duration
}

// parseDrainTaints parses a comma-separated string of "key:effect" pairs.
func parseDrainTaints(raw string) ([]DrainTaint, error) {
	var taints []DrainTaint
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid drain taint format %q, expected key:effect", entry)
		}
		taints = append(taints, DrainTaint{Key: parts[0], Effect: parts[1]})
	}
	return taints, nil
}

func main() {
	app := &cli.Command{
		Name:  "graceful-drain-controller",
		Usage: "Kubernetes controller for zero-downtime node drains of singleton Deployments",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "port",
				Value:   8081,
				Usage:   "Port for health check probes",
				Sources: cli.EnvVars("PORT"),
			},
			&cli.StringFlag{
				Name:    "log-level",
				Value:   "info",
				Usage:   "Log level (debug, info, warn, error)",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_LOG_LEVEL"),
			},
			&cli.StringFlag{
				Name:    "drain-taint",
				Value:   "karpenter.sh/disrupted:NoSchedule,ToBeDeletedByClusterAutoscaler:NoSchedule,node.kubernetes.io/unschedulable:NoSchedule",
				Usage:   "Comma-separated drain taints as key:effect pairs",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_DRAIN_TAINTS"),
			},
			&cli.StringFlag{
				Name:    "enabled-annotation",
				Value:   "",
				Usage:   "If set, only handle Deployments with this annotation set to 'true'",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_ENABLED_ANNOTATION"),
			},
			&cli.DurationFlag{
				Name:    "requeue-interval",
				Value:   5 * time.Second,
				Usage:   "Interval between requeue checks during rollout",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_REQUEUE_INTERVAL"),
			},
			&cli.DurationFlag{
				Name:    "rollout-timeout",
				Value:   5 * time.Minute,
				Usage:   "Maximum time to wait for rollout completion",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_ROLLOUT_TIMEOUT"),
			},
		},
		Action: run,
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	cfg := Config{
		Port:              int(cmd.Int("port")),
		LogLevel:          cmd.String("log-level"),
		EnabledAnnotation: cmd.String("enabled-annotation"),
		RequeueInterval:   cmd.Duration("requeue-interval"),
		RolloutTimeout:    cmd.Duration("rollout-timeout"),
	}

	// Parse drain taints.
	taints, err := parseDrainTaints(cmd.String("drain-taint"))
	if err != nil {
		return fmt.Errorf("parse drain taints: %w", err)
	}
	cfg.DrainTaints = taints

	// Set up structured logging.
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	slog.Info("starting graceful-drain-controller",
		"port", cfg.Port,
		"drainTaints", fmt.Sprintf("%+v", cfg.DrainTaints),
		"enabledAnnotation", cfg.EnabledAnnotation,
		"requeueInterval", cfg.RequeueInterval,
		"rolloutTimeout", cfg.RolloutTimeout,
	)

	// Create controller-runtime manager.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: fmt.Sprintf(":%d", cfg.Port),
		LeaderElection:         true,
		LeaderElectionID:       "graceful-drain-controller",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Index pods by spec.nodeName for efficient listing.
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		pod, ok := o.(*corev1.Pod)
		if !ok || pod.Spec.NodeName == "" {
			return nil
		}
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return fmt.Errorf("index pods by nodeName: %w", err)
	}

	// Register the reconciler.
	reconciler := &NodeReconciler{
		Client:            mgr.GetClient(),
		Recorder:          mgr.GetEventRecorderFor("graceful-drain-controller"),
		DrainTaints:       cfg.DrainTaints,
		EnabledAnnotation: cfg.EnabledAnnotation,
		RequeueInterval:   cfg.RequeueInterval,
		RolloutTimeout:    cfg.RolloutTimeout,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup controller: %w", err)
	}

	// Health probes.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	slog.Info("starting manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}
