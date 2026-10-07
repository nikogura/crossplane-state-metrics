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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

const desiredCountEntry = "ecs.aws.m.upbound.io/Service:status.atProvider.desiredCount"

// serviceKind is a namespaced managed-resource kind with no comparable schema,
// which is the shape of the kind the feature was asked for.
func serviceKind() (kind discovery.Kind) {
	kind = discovery.Kind{
		GVR:        schema.GroupVersionResource{Group: "ecs.aws.m.upbound.io", Version: "v1beta1", Resource: "services"},
		GVK:        schema.GroupVersionKind{Group: "ecs.aws.m.upbound.io", Version: "v1beta1", Kind: "Service"},
		Namespaced: true,
		Managed:    true,
		Categories: []string{"crossplane", "managed"},
	}

	return kind
}

// service builds an ECS Service object reporting a desired count.
func service(name string, desired any) (built *unstructured.Unstructured) {
	built = object(name, "prod", map[string]any{
		"status": map[string]any{"atProvider": map[string]any{"desiredCount": desired}},
	})

	return built
}

// fieldConfig returns a configuration exporting the given field entries.
func fieldConfig(t *testing.T, entries ...string) (cfg config.Config) {
	t.Helper()

	cfg = mustConfig(t, func(cfg *config.Config) {
		for _, entry := range entries {
			metric, err := config.ParseFieldMetric(entry)
			require.NoError(t, err)

			cfg.FieldMetrics = append(cfg.FieldMetrics, metric)
		}
	})

	return cfg
}

// TestFieldMetricEmitsTheValue is the acceptance case: one series per object
// of the configured kind, carrying the object's own labels and the numeric
// value of the field.
func TestFieldMetricEmitsTheValue(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:   serviceKind(),
		Synced: true,
		Objects: []*unstructured.Unstructured{
			service("api", int64(3)),
			service("worker", float64(12)),
		},
	}}}

	subject := collector.New(source, fieldConfig(t, desiredCountEntry), discardLogger())

	expected := `
# HELP crossplane_state_resource_field Value of one numeric field of a Crossplane object, named by the field label. Opt-in per kind and field through --field-metrics, and emitted only where the field is present and numeric.
# TYPE crossplane_state_resource_field gauge
crossplane_state_resource_field{field="status.atProvider.desiredCount",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_name="api",xp_namespace="prod",xp_version="v1beta1"} 3
crossplane_state_resource_field{field="status.atProvider.desiredCount",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_name="worker",xp_namespace="prod",xp_version="v1beta1"} 12
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_field")
	require.NoError(t, err)
}

// TestFieldMetricIsOptIn: with nothing configured, the metric does not exist.
func TestFieldMetricIsOptIn(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    serviceKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{service("api", int64(3))},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	assert.Equal(t, 0, testutil.CollectAndCount(subject, "crossplane_state_resource_field"))
}

// TestFieldMetricOnlyWhereNumeric: a missing field, a non-numeric value, or an
// object of a different kind produces no series rather than a wrong one.
func TestFieldMetricOnlyWhereNumeric(t *testing.T) {
	t.Parallel()

	clusterScoped := serviceKind()
	clusterScoped.GVK.Group = "ecs.aws.upbound.io"
	clusterScoped.Namespaced = false

	source := stubSource{snapshots: []watch.Snapshot{
		{
			Kind:   serviceKind(),
			Synced: true,
			Objects: []*unstructured.Unstructured{
				service("counted", int64(3)),
				service("as-string", "3"),
				service("as-bool", true),
				object("absent", "prod", map[string]any{"status": map[string]any{"atProvider": map[string]any{}}}),
				object("no-status", "prod", map[string]any{}),
			},
		},
		{
			Kind:    clusterScoped,
			Synced:  true,
			Objects: []*unstructured.Unstructured{service("other-group", int64(5))},
		},
	}}

	subject := collector.New(source, fieldConfig(t, desiredCountEntry), discardLogger())

	expected := `
# HELP crossplane_state_resource_field Value of one numeric field of a Crossplane object, named by the field label. Opt-in per kind and field through --field-metrics, and emitted only where the field is present and numeric.
# TYPE crossplane_state_resource_field gauge
crossplane_state_resource_field{field="status.atProvider.desiredCount",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_name="counted",xp_namespace="prod",xp_version="v1beta1"} 3
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_field")
	require.NoError(t, err)
}

// TestFieldMetricSeveralFieldsPerKind: each configured field is its own series.
func TestFieldMetricSeveralFieldsPerKind(t *testing.T) {
	t.Parallel()

	svc := service("api", int64(3))
	require.NoError(t, unstructured.SetNestedField(svc.Object, int64(2), "spec", "forProvider", "desiredCount"))

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    serviceKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{svc},
	}}}

	subject := collector.New(source, fieldConfig(t,
		desiredCountEntry,
		"ecs.aws.m.upbound.io/Service:spec.forProvider.desiredCount",
	), discardLogger())

	expected := `
# HELP crossplane_state_resource_field Value of one numeric field of a Crossplane object, named by the field label. Opt-in per kind and field through --field-metrics, and emitted only where the field is present and numeric.
# TYPE crossplane_state_resource_field gauge
crossplane_state_resource_field{field="spec.forProvider.desiredCount",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_name="api",xp_namespace="prod",xp_version="v1beta1"} 2
crossplane_state_resource_field{field="status.atProvider.desiredCount",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_name="api",xp_namespace="prod",xp_version="v1beta1"} 3
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_field")
	require.NoError(t, err)
}

// TestFieldMetricCarriesOwnership: with --composite-label on, the field series
// carries the same ownership labels as the condition series, so it joins an
// environment-scoped dashboard with no further work.
func TestFieldMetricCarriesOwnership(t *testing.T) {
	t.Parallel()

	svc := service("api", int64(3))
	svc.SetLabels(map[string]string{"crossplane.io/composite": "sre-prod"})

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    serviceKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{svc},
	}}}

	cfg := fieldConfig(t, desiredCountEntry)
	cfg.CompositeLabel = true

	subject := collector.New(source, cfg, discardLogger())

	expected := `
# HELP crossplane_state_resource_field Value of one numeric field of a Crossplane object, named by the field label. Opt-in per kind and field through --field-metrics, and emitted only where the field is present and numeric.
# TYPE crossplane_state_resource_field gauge
crossplane_state_resource_field{field="status.atProvider.desiredCount",xp_claim="",xp_claim_namespace="",xp_composite="sre-prod",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_name="api",xp_namespace="prod",xp_version="v1beta1"} 3
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_field")
	require.NoError(t, err)
}

// TestFieldMetricCountsAgainstTheCap: field series draw on the same allowance
// as every other series, and an aggregated kind emits none, since per-object
// detail is exactly what aggregation gives up.
func TestFieldMetricCountsAgainstTheCap(t *testing.T) {
	t.Parallel()

	objects := make([]*unstructured.Unstructured, 0, 10)
	for index := range 10 {
		objects = append(objects, service("svc-"+strings.Repeat("x", index), int64(index)))
	}

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    serviceKind(),
		Synced:  true,
		Objects: objects,
	}}}

	unlimited := collector.New(source, fieldConfig(t, desiredCountEntry), discardLogger())
	withoutFields := collector.New(source, defaultConfig(t), discardLogger())

	assert.Equal(t, countSeries(t, withoutFields)+10, countSeries(t, unlimited),
		"one field series per object is added to the scrape's total")

	// countSeries includes the two constant series that are not budgeted.
	budgeted := countSeries(t, withoutFields) - 2

	capped := fieldConfig(t, desiredCountEntry)
	capped.MaxSeries = budgeted + 4

	cappedCollector := collector.New(source, capped, discardLogger())
	assert.Equal(t, budgeted+4+2, countSeries(t, cappedCollector), "field series stop at the cap like any other")

	truncated := `
# HELP crossplane_state_scrape_truncated 1 when the series cap was reached and the scrape was cut short. Alert on this: the metrics are incomplete, and which objects survived is stable but arbitrary.
# TYPE crossplane_state_scrape_truncated gauge
crossplane_state_scrape_truncated 1
`
	require.NoError(t, testutil.CollectAndCompare(cappedCollector, strings.NewReader(truncated), "crossplane_state_scrape_truncated"))

	aggregated := fieldConfig(t, desiredCountEntry)
	aggregated.AggregateThreshold = 5

	assert.Equal(t, 0, testutil.CollectAndCount(collector.New(source, aggregated, discardLogger()), "crossplane_state_resource_field"),
		"an aggregated kind reports no per-object field values")
}

// TestFieldMetricPassesPromlint keeps the new metric inside Prometheus naming
// rules alongside the rest.
func TestFieldMetricPassesPromlint(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    serviceKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{service("api", int64(3))},
	}}}

	subject := collector.New(source, fieldConfig(t, desiredCountEntry), discardLogger())

	problems, err := testutil.CollectAndLint(subject)
	require.NoError(t, err)
	assert.Empty(t, problems)
}
