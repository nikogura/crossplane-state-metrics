// Copyright © 2026 Nik Ogura <nik.ogura@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command crossplane-state-metrics exports per-object state and configuration
// drift for the Crossplane objects in a Kubernetes cluster, in Prometheus
// format.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.opentelemetry.io/otel"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
	"github.com/nikogura/crossplane-state-metrics/pkg/observability"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// serviceName identifies this service in telemetry; otelScope is the
// instrumentation scope for its instruments.
const (
	serviceName = "crossplane-state-metrics"
	otelScope   = "github.com/nikogura/crossplane-state-metrics"
)

// serviceVersion is stamped into telemetry via
// -ldflags "-X main.serviceVersion=<version>"; "dev" is the local default.
//
//nolint:gochecknoglobals // build-time injected version string
var serviceVersion = "dev"

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		// The logger is not up yet, and a configuration error must be visible.
		_, _ = os.Stderr.WriteString("configuration error: " + err.Error() + "\n")
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	run(ctx, cfg, logger)
}

// run wires the exporter together and blocks until the context is cancelled.
func run(ctx context.Context, cfg config.Config, logger *slog.Logger) {
	obs, err := observability.Init(ctx, observability.Options{
		ServiceName:  serviceName,
		Version:      serviceVersion,
		MetricsPush:  cfg.MetricsPush,
		PushInterval: cfg.PushInterval,
	}, logger)
	if err != nil {
		logger.ErrorContext(ctx, "initializing observability", slog.String("error", err.Error()))
		os.Exit(1)
	}

	defer shutdownObservability(obs, logger)

	err = metrics.Init(otel.Meter(otelScope), serviceVersion)
	if err != nil {
		logger.ErrorContext(ctx, "initializing metric instruments", slog.String("error", err.Error()))
		os.Exit(1)
	}

	metrics.RegisterKubeClientMetrics()

	client, err := buildClient(cfg)
	if err != nil {
		logger.ErrorContext(ctx, "building Kubernetes client", slog.String("error", err.Error()))
		os.Exit(1)
	}

	manager := watch.New(client, watch.Options{
		Matcher:    discovery.NewMatcher(cfg.Categories, cfg.Groups, cfg.ExcludeKinds, cfg.ExcludeGroups),
		Namespaces: cfg.Namespaces,
		Resync:     cfg.Resync,
		TrimCache:  cfg.TrimCache,
	}, logger)

	stateCollector := collector.New(manager, cfg, logger)

	err = obs.StateRegistry.Register(stateCollector)
	if err != nil {
		logger.ErrorContext(ctx, "registering state collector", slog.String("error", err.Error()))
		os.Exit(1)
	}

	server := metrics.NewServer(cfg.MetricsAddr, obs.Gatherer(), logger)
	server.SetReadinessChecker(manager.Ready)

	// The admin server comes up before the watch does, so probes and /metrics
	// answer from the first moment the process is alive. Readiness reports
	// syncing until the CRD watch has listed.
	go serveMetrics(ctx, server, logger)

	logger.InfoContext(ctx, "crossplane-state-metrics starting",
		slog.String("version", serviceVersion),
		slog.Any("categories", cfg.Categories),
		slog.Any("groups", cfg.Groups),
		slog.Bool("drift", cfg.Drift),
		slog.String("drift_mode", cfg.DriftMode))

	runErr := manager.Run(ctx)
	if runErr != nil {
		logger.ErrorContext(ctx, "watch manager stopped", slog.String("error", runErr.Error()))
	}

	logger.InfoContext(ctx, "shutdown complete")
}

// serveMetrics runs the admin server, logging rather than exiting on failure.
func serveMetrics(ctx context.Context, server *metrics.Server, logger *slog.Logger) {
	err := server.Start(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "admin server stopped", slog.String("error", err.Error()))
	}
}

// shutdownObservability flushes telemetry on exit.
func shutdownObservability(obs *observability.Providers, logger *slog.Logger) {
	err := obs.Shutdown(context.Background())
	if err != nil {
		logger.Error("flushing telemetry on shutdown", slog.String("error", err.Error()))
	}
}

// buildClient creates the dynamic Kubernetes client, applying the configured
// client-side rate limit.
func buildClient(cfg config.Config) (client dynamic.Interface, err error) {
	var restConfig *rest.Config

	if cfg.Kubeconfig == "" {
		restConfig, err = rest.InClusterConfig()
	} else {
		restConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	}

	if err != nil {
		return client, err
	}

	restConfig.QPS = float32(cfg.KubeQPS)
	restConfig.Burst = cfg.KubeBurst
	restConfig.UserAgent = serviceName + "/" + serviceVersion

	client, err = dynamic.NewForConfig(restConfig)

	return client, err
}

// newLogger builds the structured JSON logger, wrapped so records emitted
// inside a span carry trace and span IDs for correlation.
func newLogger(level string) (logger *slog.Logger) {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)})
	logger = slog.New(observability.NewSlogHandler(handler))

	return logger
}

// parseLevel maps a configured level name onto an slog level. Validation has
// already rejected anything unrecognised, so the default is unreachable in
// practice and exists only to keep the switch total.
func parseLevel(level string) (parsed slog.Level) {
	switch level {
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}

	return parsed
}
