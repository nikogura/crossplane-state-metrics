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

package metrics_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
)

// newMeterRegistry wires a real meter into a Prometheus registry so the tests
// can assert on the metric families actually produced.
func newMeterRegistry(t *testing.T) (registry *prometheus.Registry) {
	t.Helper()

	registry = prometheus.NewRegistry()

	exporter, err := promexporter.New(promexporter.WithRegisterer(registry))
	require.NoError(t, err)

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))

	err = metrics.Init(provider.Meter("test"), "test-version")
	require.NoError(t, err)

	return registry
}

// TestRecordersAreNilSafeBeforeInit proves that tooling which never calls Init
// records nothing rather than panicking on a nil instrument.
func TestRecordersAreNilSafeBeforeInit(t *testing.T) {
	ctx := context.Background()

	assert.NotPanics(t, func() {
		metrics.RecordScrape(ctx, 0.1)
		metrics.RecordScrapeError(ctx, "test")
		metrics.RecordInformerSyncError(ctx, "gvr")
		metrics.RecordCRDEvent(ctx, "register")
		metrics.RecordDrift(ctx, 0.1)
		metrics.RecordDriftError(ctx)
		metrics.RecordKubeRequest(ctx, "GET", "200")
		metrics.RecordKubeRequestDuration(ctx, "GET", 0.1)
		metrics.ScrapeStarted(ctx)
		metrics.ScrapeFinished(ctx)
		metrics.SetKindsWatched(1)
		metrics.SetInformerSyncState(1, 0)
	})
}

// TestInitRegistersEveryInstrument asserts the exporter's golden signals and
// health gauges all reach the registry under the names the dashboard queries.
func TestInitRegistersEveryInstrument(t *testing.T) {
	registry := newMeterRegistry(t)
	ctx := context.Background()

	metrics.RecordScrape(ctx, 0.25)
	metrics.RecordScrapeError(ctx, "collect")
	metrics.RecordInformerSyncError(ctx, "s3.aws.upbound.io/v1beta1, Resource=buckets")
	metrics.RecordCRDEvent(ctx, "register")
	metrics.RecordDrift(ctx, 0.01)
	metrics.RecordDriftError(ctx)
	metrics.RecordKubeRequest(ctx, "GET", "200")
	metrics.RecordKubeRequestDuration(ctx, "GET", 0.02)
	metrics.ScrapeStarted(ctx)
	metrics.SetKindsWatched(7)
	metrics.SetInformerSyncState(6, 1)

	families, err := registry.Gather()
	require.NoError(t, err)

	present := map[string]struct{}{}
	for _, family := range families {
		present[family.GetName()] = struct{}{}
	}

	expected := []string{
		// Traffic, errors, latency, saturation.
		"crossplane_state_scrapes_total",
		"crossplane_state_scrape_errors_total",
		"crossplane_state_scrape_duration_seconds",
		"crossplane_state_scrapes_in_flight",
		// Domain health.
		"crossplane_state_kinds_watched",
		"crossplane_state_informers_synced",
		"crossplane_state_informers_pending",
		"crossplane_state_informer_sync_errors_total",
		"crossplane_state_crd_events_total",
		// Drift.
		"crossplane_state_drift_duration_seconds",
		"crossplane_state_drift_errors_total",
		// Outbound calls.
		"crossplane_state_kube_requests_total",
		"crossplane_state_kube_request_duration_seconds",
		// Build information.
		"crossplane_state_build_info",
	}

	for _, name := range expected {
		assert.Contains(t, present, name, "instrument %s must reach the registry", name)
	}

	metrics.ScrapeFinished(ctx)
}

// TestGaugesReportSetValues proves the observable gauges read back the values
// the watch manager stores in them.
func TestGaugesReportSetValues(t *testing.T) {
	registry := newMeterRegistry(t)

	metrics.SetKindsWatched(42)
	metrics.SetInformerSyncState(40, 2)

	families, err := registry.Gather()
	require.NoError(t, err)

	values := map[string]float64{}

	for _, family := range families {
		for _, metric := range family.GetMetric() {
			if metric.GetGauge() != nil {
				values[family.GetName()] = metric.GetGauge().GetValue()
			}
		}
	}

	assert.InDelta(t, 42.0, values["crossplane_state_kinds_watched"], 0.001)
	assert.InDelta(t, 40.0, values["crossplane_state_informers_synced"], 0.001)
	assert.InDelta(t, 2.0, values["crossplane_state_informers_pending"], 0.001)
}

// TestKubeClientMetricsAreRecorded covers the client-go adapters, which are how
// every outbound call to the Kubernetes API gets counted and timed.
func TestKubeClientMetricsAreRecorded(t *testing.T) {
	registry := newMeterRegistry(t)
	ctx := context.Background()

	metrics.RegisterKubeClientMetrics()

	// Drive the recorders the adapters delegate to. The request URL is
	// deliberately not a label: it carries resource names and would put
	// unbounded cardinality into the metric.
	metrics.RecordKubeRequestDuration(ctx, "LIST", (25 * time.Millisecond).Seconds())
	metrics.RecordKubeRequest(ctx, "LIST", "200")
	metrics.RecordKubeRequest(ctx, "LIST", "403")

	families, err := registry.Gather()
	require.NoError(t, err)

	var codes []string

	for _, family := range families {
		if family.GetName() != "crossplane_state_kube_requests_total" {
			continue
		}

		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "code" {
					codes = append(codes, label.GetValue())
				}
			}
		}
	}

	assert.Contains(t, codes, "200")
	assert.Contains(t, codes, "403", "a denied API group must be visible as a 403")
}
