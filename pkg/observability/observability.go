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

// Package observability wires the OpenTelemetry signals for the exporter.
//
// Metrics are SCRAPED, not pushed. The OTel meter provider is fed by a
// Prometheus *pull* reader that registers instruments into a
// prometheus.Registry; nothing leaves the process until Prometheus issues a
// GET against /metrics. That is the default and needs no configuration.
//
// Two registries are kept, and the scrape endpoint gathers both:
//
//   - Registry holds the exporter's own OTel instruments.
//   - StateRegistry holds the native collector that walks the informer caches
//     and emits per-object Crossplane state on each scrape.
//
// They are separate so the optional OTLP metrics push can bridge the state
// registry into a periodic reader without re-collecting the OTel instruments
// that reader already gathers natively — one registry would double-count every
// self-observability series. Push is strictly additive: enabling it does not
// change, disable, or degrade the scrape endpoint.
//
// Tracing exports over OTLP when an endpoint is configured and is a cheap
// no-op otherwise, so the exporter behaves identically without a collector.
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Options configures observability setup.
type Options struct {
	// ServiceName and Version populate the OTel resource.
	ServiceName string
	Version     string

	// MetricsPush additionally exports metrics over OTLP on an interval. It is
	// off by default: Prometheus scrapes, and the /metrics endpoint is the
	// supported path. Turn it on only for an environment that cannot scrape —
	// a collector-only pipeline, or a network where nothing can reach the pod.
	MetricsPush bool

	// PushInterval is how often the OTLP metrics reader exports. Ignored when
	// MetricsPush is false.
	PushInterval time.Duration
}

// Providers bundles the configured OTel providers and the registries the
// metrics endpoint serves. Call Shutdown on graceful exit.
type Providers struct {
	// Registry holds the exporter's own OTel instruments.
	Registry *prometheus.Registry

	// StateRegistry holds the native per-object Crossplane state collector.
	StateRegistry *prometheus.Registry

	shutdownFuncs []func(ctx context.Context) (err error)
	tracingActive bool
	pushActive    bool
}

// Init configures the global OTel meter and tracer providers and the text-map
// propagator, returning the registries to serve and a Providers handle for
// shutdown.
func Init(ctx context.Context, opts Options, logger *slog.Logger) (providers *Providers, err error) {
	var res *resource.Resource

	res, err = buildResource(opts.ServiceName, opts.Version)
	if err != nil {
		err = fmt.Errorf("building otel resource: %w", err)
		return providers, err
	}

	registry := prometheus.NewRegistry()
	stateRegistry := prometheus.NewRegistry()

	providers = &Providers{Registry: registry, StateRegistry: stateRegistry}

	var meterProvider *sdkmetric.MeterProvider

	meterProvider, err = buildMeterProvider(ctx, registry, stateRegistry, res, opts, providers, logger)
	if err != nil {
		err = fmt.Errorf("building meter provider: %w", err)
		return providers, err
	}

	otel.SetMeterProvider(meterProvider)

	var tracerProvider *sdktrace.TracerProvider

	tracerProvider, providers.tracingActive, err = buildTracerProvider(ctx, res, logger)
	if err != nil {
		err = fmt.Errorf("building tracer provider: %w", err)
		return providers, err
	}

	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	providers.shutdownFuncs = append(providers.shutdownFuncs, meterProvider.Shutdown, tracerProvider.Shutdown)

	logger.InfoContext(ctx, "observability initialized",
		slog.String("service", opts.ServiceName),
		slog.String("version", opts.Version),
		slog.Bool("tracing_active", providers.tracingActive),
		slog.Bool("metrics_push_active", providers.pushActive))

	return providers, err
}

// Gatherer returns the combined gatherer the scrape endpoint serves: the
// exporter's own instruments plus the Crossplane state metrics.
func (p *Providers) Gatherer() (gatherer prometheus.Gatherer) {
	gatherer = prometheus.Gatherers{p.Registry, p.StateRegistry}
	return gatherer
}

// TracingActive reports whether an OTLP trace exporter was configured.
func (p *Providers) TracingActive() (active bool) {
	active = p.tracingActive
	return active
}

// MetricsPushActive reports whether the optional OTLP metrics push is running.
func (p *Providers) MetricsPushActive() (active bool) {
	active = p.pushActive
	return active
}

// Shutdown flushes and stops every configured provider, returning the first
// error encountered after attempting them all.
func (p *Providers) Shutdown(ctx context.Context) (err error) {
	for _, shutdown := range p.shutdownFuncs {
		shutdownErr := shutdown(ctx)
		if shutdownErr != nil && err == nil {
			err = shutdownErr
		}
	}

	return err
}

// buildResource assembles the OTel resource describing this service.
func buildResource(serviceName string, version string) (res *resource.Resource, err error) {
	res, err = resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)

	return res, err
}

// buildMeterProvider creates the meter provider. The Prometheus pull reader is
// always installed; the OTLP periodic reader is added only when push is
// explicitly requested.
func buildMeterProvider(ctx context.Context, registry *prometheus.Registry, stateRegistry *prometheus.Registry, res *resource.Resource, opts Options, providers *Providers, logger *slog.Logger) (provider *sdkmetric.MeterProvider, err error) {
	pullReader, newErr := promexporter.New(
		promexporter.WithRegisterer(registry),
		// Map instrument names one-to-one to metric names, so dashboards and
		// alerts reference exactly the names declared in code.
		promexporter.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
		promexporter.WithoutScopeInfo(),
	)
	if newErr != nil {
		err = fmt.Errorf("creating prometheus exporter: %w", newErr)
		return provider, err
	}

	providerOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(pullReader),
		sdkmetric.WithView(secondsHistogramView("crossplane_state_scrape_duration_seconds",
			[]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30})),
		sdkmetric.WithView(secondsHistogramView("crossplane_state_drift_duration_seconds",
			[]float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5})),
		sdkmetric.WithView(secondsHistogramView("crossplane_state_kube_request_duration_seconds",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})),
	}

	if opts.MetricsPush {
		var pushReader sdkmetric.Reader

		pushReader, err = buildPushReader(ctx, stateRegistry, opts, logger)
		if err != nil {
			return provider, err
		}

		providerOpts = append(providerOpts, sdkmetric.WithReader(pushReader))
		providers.pushActive = true
	}

	provider = sdkmetric.NewMeterProvider(providerOpts...)

	return provider, err
}

// buildPushReader creates the optional OTLP metrics reader. The state registry
// is bridged in as a producer so a pushing deployment reports the same series
// a scraping one would, rather than only the exporter's own telemetry.
func buildPushReader(ctx context.Context, stateRegistry *prometheus.Registry, opts Options, logger *slog.Logger) (reader sdkmetric.Reader, err error) {
	endpoint := metricsOTLPEndpoint()
	if endpoint == "" {
		err = errors.New("metrics push was requested but no OTLP endpoint is set; " +
			"set OTEL_EXPORTER_OTLP_METRICS_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT, or disable push and let Prometheus scrape /metrics")

		return reader, err
	}

	exporter, newErr := otlpmetrichttp.New(ctx)
	if newErr != nil {
		err = fmt.Errorf("creating OTLP metrics exporter: %w", newErr)
		return reader, err
	}

	reader = sdkmetric.NewPeriodicReader(exporter,
		sdkmetric.WithInterval(opts.PushInterval),
		sdkmetric.WithProducer(prombridge.NewMetricProducer(prombridge.WithGatherer(stateRegistry))),
	)

	logger.InfoContext(ctx, "OTLP metrics push enabled; the scrape endpoint remains authoritative",
		slog.String("endpoint", endpoint),
		slog.Duration("interval", opts.PushInterval))

	return reader, err
}

// secondsHistogramView pins explicit bucket boundaries on a named histogram.
func secondsHistogramView(instrument string, boundaries []float64) (view sdkmetric.View) {
	view = sdkmetric.NewView(
		sdkmetric.Instrument{Name: instrument},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
			Boundaries: boundaries,
		}},
	)

	return view
}

// buildTracerProvider creates a tracer provider that exports over OTLP/HTTP
// when an endpoint is configured, and is otherwise a cheap no-op.
func buildTracerProvider(ctx context.Context, res *resource.Resource, logger *slog.Logger) (provider *sdktrace.TracerProvider, active bool, err error) {
	endpoint := tracesOTLPEndpoint()
	if endpoint == "" {
		provider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.NeverSample()),
		)

		logger.InfoContext(ctx, "OTLP trace endpoint unset, tracing runs no-op")

		return provider, active, err
	}

	exporter, newErr := otlptracehttp.New(ctx)
	if newErr != nil {
		err = fmt.Errorf("creating OTLP trace exporter: %w", newErr)
		return provider, active, err
	}

	provider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	active = true

	logger.InfoContext(ctx, "OTLP tracing enabled", slog.String("endpoint", endpoint))

	return provider, active, err
}

// tracesOTLPEndpoint returns the configured OTLP traces endpoint, honoring the
// signal-specific variable ahead of the general one.
func tracesOTLPEndpoint() (endpoint string) {
	endpoint = envFirst("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	return endpoint
}

// metricsOTLPEndpoint returns the configured OTLP metrics endpoint, honoring
// the signal-specific variable ahead of the general one.
func metricsOTLPEndpoint() (endpoint string) {
	endpoint = envFirst("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	return endpoint
}

// envFirst returns the first environment variable of those named that is set.
func envFirst(keys ...string) (value string) {
	for _, key := range keys {
		value = os.Getenv(key)
		if value != "" {
			return value
		}
	}

	return value
}
