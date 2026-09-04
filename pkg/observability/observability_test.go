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

package observability_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nikogura/crossplane-state-metrics/pkg/observability"
)

// discardLogger builds a logger that writes nowhere.
func discardLogger() (logger *slog.Logger) {
	logger = slog.New(slog.DiscardHandler)
	return logger
}

// TestInitBuildsBothRegistries is the regression test for a startup failure
// that would take the whole exporter down: the OTel resource is a merge of the
// SDK default and this service's attributes, and if the semconv package the
// code imports does not match the schema URL resource.Default uses, the merge
// fails and Init returns an error before anything serves.
func TestInitBuildsBothRegistries(t *testing.T) {
	providers, err := observability.Init(context.Background(), observability.Options{
		ServiceName: "crossplane-state-metrics",
		Version:     "test",
	}, discardLogger())
	require.NoError(t, err, "a semconv/SDK schema mismatch surfaces here")
	require.NotNil(t, providers)

	assert.NotNil(t, providers.Registry)
	assert.NotNil(t, providers.StateRegistry)
	assert.NotSame(t, providers.Registry, providers.StateRegistry,
		"the registries must be distinct so an OTLP push does not double-count the OTel instruments")

	assert.NotNil(t, providers.Gatherer(), "one scrape must serve both registries")

	err = providers.Shutdown(context.Background())
	assert.NoError(t, err)
}

// TestScrapingIsTheDefault pins the stance: nothing is exported anywhere until
// Prometheus asks.
func TestScrapingIsTheDefault(t *testing.T) {
	providers, err := observability.Init(context.Background(), observability.Options{
		ServiceName: "crossplane-state-metrics",
		Version:     "test",
	}, discardLogger())
	require.NoError(t, err)

	assert.False(t, providers.MetricsPushActive(), "metrics push must be off unless asked for")
	assert.False(t, providers.TracingActive(), "tracing must be a no-op with no OTLP endpoint set")

	_ = providers.Shutdown(context.Background())
}

// TestMetricsPushWithoutEndpointFails proves a misconfigured push is a loud
// startup error, not a silent no-op that leaves an operator believing metrics
// are being delivered.
func TestMetricsPushWithoutEndpointFails(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	_, err := observability.Init(context.Background(), observability.Options{
		ServiceName:  "crossplane-state-metrics",
		Version:      "test",
		MetricsPush:  true,
		PushInterval: time.Minute,
	}, discardLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no OTLP endpoint is set")
}

// TestMetricsPushIsAdditive proves enabling push does not cost the scrape
// endpoint: both registries are still built and still served.
func TestMetricsPushIsAdditive(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://localhost:4318")

	providers, err := observability.Init(context.Background(), observability.Options{
		ServiceName:  "crossplane-state-metrics",
		Version:      "test",
		MetricsPush:  true,
		PushInterval: time.Minute,
	}, discardLogger())
	require.NoError(t, err)

	assert.True(t, providers.MetricsPushActive())
	assert.NotNil(t, providers.Registry, "the scrape endpoint keeps working with push on")
	assert.NotNil(t, providers.StateRegistry)

	_ = providers.Shutdown(context.Background())
}

func TestTracingActivatesWithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318")

	providers, err := observability.Init(context.Background(), observability.Options{
		ServiceName: "crossplane-state-metrics",
		Version:     "test",
	}, discardLogger())
	require.NoError(t, err)

	assert.True(t, providers.TracingActive())

	_ = providers.Shutdown(context.Background())
}
