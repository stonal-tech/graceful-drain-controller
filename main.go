package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	cli "github.com/urfave/cli/v3"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)

	return s
}()

// Config holds all controller configuration.
type Config struct {
	Port              int
	WebhookPort       int
	CertDir           string
	LogLevel          string
	EnabledAnnotation string
	RequeueInterval   time.Duration
	RolloutTimeout    time.Duration
}

func main() {
	app := &cli.Command{
		Name:  "graceful-drain-controller",
		Usage: "Kubernetes controller for zero-downtime eviction of singleton Deployments",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "port",
				Value:   8081,
				Usage:   "Port for health check probes",
				Sources: cli.EnvVars("PORT"),
			},
			&cli.IntFlag{
				Name:    "webhook-port",
				Value:   9443,
				Usage:   "Port for the webhook HTTPS server",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_WEBHOOK_PORT"),
			},
			&cli.StringFlag{
				Name:    "cert-dir",
				Value:   "",
				Usage:   "Directory containing TLS certs for the webhook server",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_CERT_DIR"),
			},
			&cli.StringFlag{
				Name:    "log-level",
				Value:   "info",
				Usage:   "Log level (debug, info, warn, error)",
				Sources: cli.EnvVars("GRACEFUL_DRAIN_LOG_LEVEL"),
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

	ctx := context.Background()
	if err := app.Run(ctx, os.Args); err != nil {
		slog.ErrorContext(ctx, "fatal error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	cfg := Config{
		Port:              cmd.Int("port"),
		WebhookPort:       cmd.Int("webhook-port"),
		CertDir:           cmd.String("cert-dir"),
		LogLevel:          cmd.String("log-level"),
		EnabledAnnotation: cmd.String("enabled-annotation"),
		RequeueInterval:   cmd.Duration("requeue-interval"),
		RolloutTimeout:    cmd.Duration("rollout-timeout"),
	}

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

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	// Use a separate handler for controller-runtime, capped at info level,
	// to suppress noisy cache/reflector debug messages.
	crLevel := max(level, slog.LevelInfo)
	crHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: crLevel})
	ctrl.SetLogger(logr.FromSlogHandler(crHandler))

	slog.InfoContext(ctx, "starting graceful-drain-controller",
		"port", cfg.Port,
		"webhookPort", cfg.WebhookPort,
		"enabledAnnotation", cfg.EnabledAnnotation,
		"requeueInterval", cfg.RequeueInterval.String(),
		"rolloutTimeout", cfg.RolloutTimeout.String(),
	)

	// Create controller-runtime manager.
	webhookOpts := webhook.Options{
		Port: cfg.WebhookPort,
	}
	if cfg.CertDir != "" {
		webhookOpts.CertDir = cfg.CertDir
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: fmt.Sprintf(":%d", cfg.Port),
		LeaderElection:         true,
		LeaderElectionID:       "graceful-drain-controller",
		WebhookServer:          webhook.NewServer(webhookOpts),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Register webhook handler.
	mgr.GetWebhookServer().Register("/validate-eviction", &webhook.Admission{
		Handler: &EvictionHandler{
			Client:            mgr.GetClient(),
			EnabledAnnotation: cfg.EnabledAnnotation,
			RolloutTimeout:    cfg.RolloutTimeout,
		},
	})

	// Register background reconciler.
	reconciler := &DeploymentReconciler{
		Client:          mgr.GetClient(),
		Recorder:        mgr.GetEventRecorder("graceful-drain-controller"),
		RolloutTimeout:  cfg.RolloutTimeout,
		RequeueInterval: cfg.RequeueInterval,
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

	slog.InfoContext(ctx, "starting manager")

	return mgr.Start(ctrl.SetupSignalHandler())
}
