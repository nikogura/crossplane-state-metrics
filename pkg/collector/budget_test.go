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

package collector_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// fleet builds n managed resources, each carrying two conditions.
func fleet(count int) (snapshots []watch.Snapshot) {
	objects := make([]*unstructured.Unstructured, 0, count)

	for index := range count {
		object := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
			"status": map[string]any{
				"atProvider": map[string]any{"region": "us-east-1"},
				"conditions": []any{
					condition("Synced", "True", "ReconcileSuccess"),
					condition("Ready", "True", "Available"),
				},
			},
		}}
		object.SetName(fmt.Sprintf("bucket-%04d", index))
		object.SetUID(types.UID(fmt.Sprintf("uid-%04d", index)))
		object.SetResourceVersion("1")
		objects = append(objects, object)
	}

	snapshots = []watch.Snapshot{{Kind: bucketKind(), Synced: true, Objects: objects}}

	return snapshots
}

// countSeries gathers a collector and totals the series it produced.
func countSeries(t *testing.T, subject prometheus.Collector) (total int) {
	t.Helper()

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(subject))

	families, err := registry.Gather()
	require.NoError(t, err)

	for _, family := range families {
		total += len(family.GetMetric())
	}

	return total
}

// TestSeriesPerObject pins the cardinality arithmetic the documentation quotes.
// If this number changes, the sizing guidance in the README is wrong.
func TestSeriesPerObject(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.MaxSeries = 0

	subject := collector.New(stubSource{snapshots: fleet(100)}, cfg, discardLogger())

	// 100 objects x (1 info + 2 conditions + 1 drift) + 2 scrape-accounting series.
	assert.Equal(t, 402, countSeries(t, subject),
		"four series per managed resource, plus the truncation and emitted-count gauges")
}

// TestSeriesCapTruncates proves the backstop stops emission and says so, rather
// than quietly overwhelming a shared Prometheus.
func TestSeriesCapTruncates(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.MaxSeries = 50

	subject := collector.New(stubSource{snapshots: fleet(100)}, cfg, discardLogger())

	total := countSeries(t, subject)
	assert.LessOrEqual(t, total, 52, "emission must stop at the cap")

	expected := `
# HELP crossplane_state_scrape_truncated 1 when the series cap was reached and the scrape was cut short. Alert on this: the metrics are incomplete, and which objects survived is stable but arbitrary.
# TYPE crossplane_state_scrape_truncated gauge
crossplane_state_scrape_truncated 1
`
	require.NoError(t, testutil.CollectAndCompare(subject, strings.NewReader(expected),
		"crossplane_state_scrape_truncated"))
}

// TestSeriesCapNotHit keeps the truncation gauge honest when there is headroom.
func TestSeriesCapNotHit(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.MaxSeries = 10000

	subject := collector.New(stubSource{snapshots: fleet(10)}, cfg, discardLogger())

	expected := `
# HELP crossplane_state_scrape_truncated 1 when the series cap was reached and the scrape was cut short. Alert on this: the metrics are incomplete, and which objects survived is stable but arbitrary.
# TYPE crossplane_state_scrape_truncated gauge
crossplane_state_scrape_truncated 0
`
	require.NoError(t, testutil.CollectAndCompare(subject, strings.NewReader(expected),
		"crossplane_state_scrape_truncated"))
}

// TestTruncationIsDeterministic is the property that makes the cap safe to use.
// If the surviving set varied between scrapes, every churned series would
// generate staleness markers and the resulting TSDB churn would be worse than
// the cardinality the cap was meant to prevent.
func TestTruncationIsDeterministic(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.MaxSeries = 37

	source := stubSource{snapshots: fleet(200)}
	subject := collector.New(source, cfg, discardLogger())

	render := func() (text string) {
		registry := prometheus.NewRegistry()
		require.NoError(t, registry.Register(collector.New(source, cfg, discardLogger())))

		families, err := registry.Gather()
		require.NoError(t, err)

		builder := &strings.Builder{}
		for _, family := range families {
			builder.WriteString(family.String())
		}

		text = builder.String()

		return text
	}

	first := render()
	for range 5 {
		assert.Equal(t, first, render(), "the same fleet must truncate to the same set every scrape")
	}

	_ = subject
}

// TestAggregateThreshold trades per-object detail for a fixed cost per kind.
func TestAggregateThreshold(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.MaxSeries = 0
	cfg.AggregateThreshold = 50

	perObject := collector.New(stubSource{snapshots: fleet(200)}, mustConfig(t, func(c *config.Config) {
		c.MaxSeries = 0
	}), discardLogger())

	aggregated := collector.New(stubSource{snapshots: fleet(200)}, cfg, discardLogger())

	assert.Equal(t, 802, countSeries(t, perObject), "200 objects x 4 series, plus 2 accounting series")

	// 1 kind_aggregated + 1 resource_count + 2 condition_count (Synced/True,
	// Ready/True) + 1 drift_count + 2 accounting = 7, for any object count.
	assert.Equal(t, 7, countSeries(t, aggregated),
		"an aggregated kind costs a fixed handful of series regardless of object count")

	// Per-object series are gone, and the marker says so.
	assert.Equal(t, 0, testutil.CollectAndCount(aggregated, "crossplane_state_resource_info"))
	assert.Equal(t, 1, testutil.CollectAndCount(aggregated, "crossplane_state_kind_aggregated"))

	expected := `
# HELP crossplane_state_resource_count Number of objects of an aggregated kind.
# TYPE crossplane_state_resource_count gauge
crossplane_state_resource_count{xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_version="v1beta1"} 200
`
	require.NoError(t, testutil.CollectAndCompare(aggregated, strings.NewReader(expected),
		"crossplane_state_resource_count"))
}

// TestAggregateBelowThresholdStaysPerObject proves the rollup only engages for
// kinds that have actually grown large.
func TestAggregateBelowThresholdStaysPerObject(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.MaxSeries = 0
	cfg.AggregateThreshold = 500

	subject := collector.New(stubSource{snapshots: fleet(10)}, cfg, discardLogger())

	assert.Equal(t, 10, testutil.CollectAndCount(subject, "crossplane_state_resource_info"))
	assert.Equal(t, 0, testutil.CollectAndCount(subject, "crossplane_state_kind_aggregated"))
}

// mustConfig returns the default config with an override applied.
func mustConfig(t *testing.T, apply func(cfg *config.Config)) (cfg config.Config) {
	t.Helper()

	cfg = defaultConfig(t)
	apply(&cfg)

	return cfg
}
