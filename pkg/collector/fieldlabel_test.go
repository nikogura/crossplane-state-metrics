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

const regionEntry = "ecs.aws.m.upbound.io/Cluster:spec.forProvider.region"

const conditionHelp = `
# HELP crossplane_state_resource_condition One series per Crossplane object per status condition. The value is always 1 and the condition's state is carried in the status label, so a new condition type appears without a code change.
# TYPE crossplane_state_resource_condition gauge
`

// clusterKind is the namespaced ECS Cluster kind the feature was asked for.
func clusterKind() (kind discovery.Kind) {
	kind = discovery.Kind{
		GVR:        schema.GroupVersionResource{Group: "ecs.aws.m.upbound.io", Version: "v1beta1", Resource: "clusters"},
		GVK:        schema.GroupVersionKind{Group: "ecs.aws.m.upbound.io", Version: "v1beta1", Kind: "Cluster"},
		Namespaced: true,
		Managed:    true,
		Categories: []string{"crossplane", "managed"},
	}

	return kind
}

// cluster builds an ECS Cluster declaring a region and reporting Ready.
func cluster(name string, region any) (built *unstructured.Unstructured) {
	built = object(name, "prod", map[string]any{
		"spec":   map[string]any{"forProvider": map[string]any{"region": region}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Available"}}},
	})

	return built
}

// labelConfig returns a configuration carrying the given field label entries.
func labelConfig(t *testing.T, entries ...string) (cfg config.Config) {
	t.Helper()

	cfg = mustConfig(t, func(cfg *config.Config) {
		for _, entry := range entries {
			label, err := config.ParseFieldLabel(entry)
			require.NoError(t, err)

			cfg.FieldLabels = append(cfg.FieldLabels, label)
		}
	})

	return cfg
}

// TestFieldLabelOnConditionSeries is the acceptance case: the region read
// off the object is a label on its condition series, named after the leaf.
func TestFieldLabelOnConditionSeries(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:   clusterKind(),
		Synced: true,
		Objects: []*unstructured.Unstructured{
			cluster("qa-1", "us-west-1"),
			cluster("qa-2", "us-east-1"),
		},
	}}}

	subject := collector.New(source, labelConfig(t, regionEntry), discardLogger())

	expected := conditionHelp + `
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="qa-1",xp_namespace="prod",xp_region="us-west-1",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="qa-2",xp_namespace="prod",xp_region="us-east-1",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_condition")
	require.NoError(t, err)
}

// TestFieldLabelOnEveryObjectSeries: the label rides on every per-object
// metric, so a field value and its region are on the same series.
func TestFieldLabelOnEveryObjectSeries(t *testing.T) {
	t.Parallel()

	built := cluster("qa-1", "us-west-1")
	require.NoError(t, unstructured.SetNestedField(built.Object, int64(4), "status", "atProvider", "registeredContainerInstancesCount"))

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    clusterKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{built},
	}}}

	cfg := labelConfig(t, regionEntry)
	metric, err := config.ParseFieldMetric("ecs.aws.m.upbound.io/Cluster:status.atProvider.registeredContainerInstancesCount")
	require.NoError(t, err)
	cfg.FieldMetrics = append(cfg.FieldMetrics, metric)

	subject := collector.New(source, cfg, discardLogger())

	expected := `
# HELP crossplane_state_resource_info Presence of a Crossplane object. The value is always 1; identity is carried in labels. Emitted for every object, including the kinds that carry no status conditions.
# TYPE crossplane_state_resource_info gauge
crossplane_state_resource_info{xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="qa-1",xp_namespace="prod",xp_region="us-west-1",xp_version="v1beta1"} 1
# HELP crossplane_state_resource_field Value of one numeric field of a Crossplane object, named by the field label. Opt-in per kind and field through --field-metrics, and emitted only where the field is present and numeric.
# TYPE crossplane_state_resource_field gauge
crossplane_state_resource_field{field="status.atProvider.registeredContainerInstancesCount",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="qa-1",xp_namespace="prod",xp_region="us-west-1",xp_version="v1beta1"} 4
`

	err = testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_info", "crossplane_state_resource_field")
	require.NoError(t, err)
}

// TestFieldLabelIsOptIn: with nothing configured, no per-object series
// carries a field label.
func TestFieldLabelIsOptIn(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    clusterKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{cluster("qa-1", "us-west-1")},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := conditionHelp + `
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="qa-1",xp_namespace="prod",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_condition")
	require.NoError(t, err)
}

// TestFieldLabelEmptyWhereUnavailable: a missing field, a non-string value,
// and an object of a kind the label is not configured for all carry the label
// empty, which is the same as absent to Prometheus, and never a wrong value.
func TestFieldLabelEmptyWhereUnavailable(t *testing.T) {
	t.Parallel()

	clusterScoped := clusterKind()
	clusterScoped.GVK.Group = "ecs.aws.upbound.io"
	clusterScoped.Namespaced = false

	source := stubSource{snapshots: []watch.Snapshot{
		{
			Kind:   clusterKind(),
			Synced: true,
			Objects: []*unstructured.Unstructured{
				cluster("named", "us-west-1"),
				cluster("as-number", int64(1)),
				cluster("as-object", map[string]any{"name": "us-west-1"}),
				object("absent", "prod", map[string]any{
					"spec":   map[string]any{"forProvider": map[string]any{}},
					"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Available"}}},
				}),
			},
		},
		{
			Kind:    clusterScoped,
			Synced:  true,
			Objects: []*unstructured.Unstructured{cluster("other-group", "us-west-1")},
		},
	}}

	subject := collector.New(source, labelConfig(t, regionEntry), discardLogger())

	expected := conditionHelp + `
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="named",xp_namespace="prod",xp_region="us-west-1",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="as-number",xp_namespace="prod",xp_region="",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="as-object",xp_namespace="prod",xp_region="",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="absent",xp_namespace="prod",xp_region="",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.upbound.io",xp_kind="Cluster",xp_name="other-group",xp_namespace="prod",xp_region="",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_condition")
	require.NoError(t, err)
}

// TestFieldLabelSharedAcrossKinds: two kinds configured for a field with the
// same leaf share one label, and a kind with its own leaf adds a second that
// the first kinds carry empty.
func TestFieldLabelSharedAcrossKinds(t *testing.T) {
	t.Parallel()

	svc := service("api", int64(3))
	require.NoError(t, unstructured.SetNestedField(svc.Object, "us-east-1", "spec", "forProvider", "region"))
	require.NoError(t, unstructured.SetNestedField(svc.Object, "FARGATE", "spec", "forProvider", "launchType"))
	require.NoError(t, unstructured.SetNestedField(svc.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "Available"}}, "status", "conditions"))

	source := stubSource{snapshots: []watch.Snapshot{
		{Kind: clusterKind(), Synced: true, Objects: []*unstructured.Unstructured{cluster("qa-1", "us-west-1")}},
		{Kind: serviceKind(), Synced: true, Objects: []*unstructured.Unstructured{svc}},
	}}

	subject := collector.New(source, labelConfig(t,
		regionEntry,
		"ecs.aws.m.upbound.io/Service:spec.forProvider.region",
		"ecs.aws.m.upbound.io/Service:spec.forProvider.launchType",
	), discardLogger())

	expected := conditionHelp + `
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_launch_type="",xp_name="qa-1",xp_namespace="prod",xp_region="us-west-1",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="ecs.aws.m.upbound.io",xp_kind="Service",xp_launch_type="FARGATE",xp_name="api",xp_namespace="prod",xp_region="us-east-1",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_condition")
	require.NoError(t, err)
}

// TestFieldLabelWithOwnership: the field label sits beside the ownership
// labels, so a dashboard filtered on xp_composite can read the region
// straight off the same series.
func TestFieldLabelWithOwnership(t *testing.T) {
	t.Parallel()

	built := cluster("qa-1-cluster", "us-west-1")
	built.SetLabels(map[string]string{"crossplane.io/composite": "qa-1"})

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    clusterKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{built},
	}}}

	cfg := labelConfig(t, regionEntry)
	cfg.CompositeLabel = true

	subject := collector.New(source, cfg, discardLogger())

	expected := conditionHelp + `
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_claim="",xp_claim_namespace="",xp_composite="qa-1",xp_group="ecs.aws.m.upbound.io",xp_kind="Cluster",xp_name="qa-1-cluster",xp_namespace="prod",xp_region="us-west-1",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_condition")
	require.NoError(t, err)
}

// TestFieldLabelAddsNoSeries: a label widens series, it does not multiply
// them, so the count against --max-series is unchanged; and an aggregated
// kind, which has no per-object series, carries none.
func TestFieldLabelAddsNoSeries(t *testing.T) {
	t.Parallel()

	objects := make([]*unstructured.Unstructured, 0, 10)
	for index := range 10 {
		objects = append(objects, cluster("qa-"+strings.Repeat("x", index), "us-west-1"))
	}

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    clusterKind(),
		Synced:  true,
		Objects: objects,
	}}}

	labelled := collector.New(source, labelConfig(t, regionEntry), discardLogger())
	plain := collector.New(source, defaultConfig(t), discardLogger())

	assert.Equal(t, countSeries(t, plain), countSeries(t, labelled))

	aggregated := labelConfig(t, regionEntry)
	aggregated.AggregateThreshold = 5

	text := render(t, collector.New(source, aggregated, discardLogger()))
	assert.NotContains(t, text, "xp_region", "an aggregated kind has no per-object series to label")
}

// TestFieldLabelPassesPromlint keeps the derived label names inside
// Prometheus naming rules.
func TestFieldLabelPassesPromlint(t *testing.T) {
	t.Parallel()

	built := cluster("qa-1", "us-west-1")
	require.NoError(t, unstructured.SetNestedField(built.Object, "zone-a", "spec", "forProvider", "availabilityZone"))

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    clusterKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{built},
	}}}

	subject := collector.New(source, labelConfig(t, regionEntry, "ecs.aws.m.upbound.io/Cluster:spec.forProvider.availabilityZone"), discardLogger())

	problems, err := testutil.CollectAndLint(subject)
	require.NoError(t, err)
	assert.Empty(t, problems)
}

// TestReservedNamesCoverTheCollectorLabels ties the config package's reserved
// list to the labels this package actually emits, so adding an object label
// here without reserving it there fails.
func TestReservedNamesCoverTheCollectorLabels(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		collector.LabelGroup, collector.LabelVersion, collector.LabelKind, collector.LabelName, collector.LabelNamespace,
		collector.LabelExternalName, collector.LabelComposite, collector.LabelClaim, collector.LabelClaimNS,
	} {
		assert.True(t, config.ReservedLabelName(name), "%s is an object label and must be reserved", name)
	}
}
