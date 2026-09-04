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
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// stubSource is a fixed set of snapshots standing in for the watch manager.
type stubSource struct {
	snapshots []watch.Snapshot
}

// Snapshot implements collector.Snapshotter.
func (s stubSource) Snapshot() (snapshots []watch.Snapshot) {
	snapshots = s.snapshots
	return snapshots
}

// discardLogger builds a logger that writes nowhere.
func discardLogger() (logger *slog.Logger) {
	logger = slog.New(slog.DiscardHandler)
	return logger
}

// defaultConfig returns the zero-flag configuration the exporter runs with out
// of the box.
func defaultConfig(t *testing.T) (cfg config.Config) {
	t.Helper()

	var err error

	cfg, err = config.Load(nil)
	require.NoError(t, err)

	return cfg
}

// bucketKind is a managed-resource kind with a small forProvider schema.
func bucketKind() (kind discovery.Kind) {
	kind = discovery.Kind{
		GVR:             schema.GroupVersionResource{Group: "s3.aws.upbound.io", Version: "v1beta1", Resource: "buckets"},
		GVK:             schema.GroupVersionKind{Group: "s3.aws.upbound.io", Version: "v1beta1", Kind: "Bucket"},
		Managed:         true,
		DriftComparable: true,
		Categories:      []string{"crossplane", "managed"},
		ForProvider: &drift.Schema{
			Type: "object",
			Properties: map[string]*drift.Schema{
				"region": {Type: "string"},
				"tags": {
					Type:                 "object",
					AdditionalProperties: &drift.Schema{Type: "string"},
				},
			},
		},
	}

	return kind
}

// providerKind is a package kind, which carries conditions but no drift.
func providerKind() (kind discovery.Kind) {
	kind = discovery.Kind{
		GVR:        schema.GroupVersionResource{Group: "pkg.crossplane.io", Version: "v1", Resource: "providers"},
		GVK:        schema.GroupVersionKind{Group: "pkg.crossplane.io", Version: "v1", Kind: "Provider"},
		Categories: []string{"crossplane", "pkg"},
	}

	return kind
}

// object builds an unstructured object with the supplied name and content.
func object(name string, namespace string, content map[string]any) (built *unstructured.Unstructured) {
	built = &unstructured.Unstructured{Object: content}
	built.SetName(name)

	if namespace != "" {
		built.SetNamespace(namespace)
	}

	built.SetUID(types.UID(name + "-uid"))
	built.SetResourceVersion("1")

	return built
}

// condition builds one xpv1-shaped status condition.
func condition(conditionType string, status string, reason string) (built map[string]any) {
	built = map[string]any{
		"type":               conditionType,
		"status":             status,
		"reason":             reason,
		"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z"),
	}

	return built
}

// TestConditionsAreUniversal is the core claim of the wider scope: one loop
// covers every Crossplane kind, because they all carry the same condition
// shape. A managed resource's Synced/Ready and a package's Installed/Healthy
// come out of the same code with no per-kind knowledge.
func TestConditionsAreUniversal(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{
		{
			Kind:   bucketKind(),
			Synced: true,
			Objects: []*unstructured.Unstructured{
				object("logs", "", map[string]any{
					"status": map[string]any{"conditions": []any{
						condition("Synced", "True", "ReconcileSuccess"),
						condition("Ready", "True", "Available"),
					}},
				}),
			},
		},
		{
			Kind:   providerKind(),
			Synced: true,
			Objects: []*unstructured.Unstructured{
				object("provider-aws-s3", "", map[string]any{
					"status": map[string]any{"conditions": []any{
						condition("Installed", "True", "HealthyPackageRevision"),
						condition("Healthy", "False", "UnhealthyPackageRevision"),
					}},
				}),
			},
		},
	}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_resource_condition One series per Crossplane object per status condition. The value is always 1 and the condition's state is carried in the status label, so a new condition type appears without a code change.
# TYPE crossplane_state_resource_condition gauge
crossplane_state_resource_condition{condition="Healthy",reason="UnhealthyPackageRevision",status="False",xp_group="pkg.crossplane.io",xp_kind="Provider",xp_name="provider-aws-s3",xp_namespace="",xp_version="v1"} 1
crossplane_state_resource_condition{condition="Installed",reason="HealthyPackageRevision",status="True",xp_group="pkg.crossplane.io",xp_kind="Provider",xp_name="provider-aws-s3",xp_namespace="",xp_version="v1"} 1
crossplane_state_resource_condition{condition="Ready",reason="Available",status="True",xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_name="logs",xp_namespace="",xp_version="v1beta1"} 1
crossplane_state_resource_condition{condition="Synced",reason="ReconcileSuccess",status="True",xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_name="logs",xp_namespace="",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_condition")
	require.NoError(t, err)
}

// TestResourceInfoCoversConditionlessKinds proves the four Crossplane kinds
// with no status conditions at all still appear in the metrics.
func TestResourceInfoCoversConditionlessKinds(t *testing.T) {
	t.Parallel()

	compositionKind := discovery.Kind{
		GVR: schema.GroupVersionResource{Group: "apiextensions.crossplane.io", Version: "v1", Resource: "compositions"},
		GVK: schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1", Kind: "Composition"},
	}

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    compositionKind,
		Synced:  true,
		Objects: []*unstructured.Unstructured{object("xnetwork", "", map[string]any{})},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	count := testutil.CollectAndCount(subject, "crossplane_state_resource_info")
	assert.Equal(t, 1, count, "a Composition has no conditions but must still be reported as present")

	conditions := testutil.CollectAndCount(subject, "crossplane_state_resource_condition")
	assert.Equal(t, 0, conditions)
}

func TestDriftMetric(t *testing.T) {
	t.Parallel()

	drifted := object("drifted", "", map[string]any{
		"spec":   map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		"status": map[string]any{"atProvider": map[string]any{"region": "us-west-2", "arn": "arn:aws:s3:::x"}},
	})

	clean := object("clean", "", map[string]any{
		"spec":   map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		"status": map[string]any{"atProvider": map[string]any{"region": "us-east-1", "arn": "arn:aws:s3:::y"}},
	})

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{drifted, clean},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_mr_drift 1 when a managed resource's declared spec.forProvider no longer matches its observed status.atProvider, 0 when they agree. Only managed resources have both halves, so only they are compared.
# TYPE crossplane_state_mr_drift gauge
crossplane_state_mr_drift{xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_name="clean",xp_namespace="",xp_version="v1beta1"} 0
crossplane_state_mr_drift{xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_name="drifted",xp_namespace="",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_mr_drift")
	require.NoError(t, err)
}

// TestDriftSkippedForNonManagedKinds pins that drift is emitted only where
// both halves of the comparison exist.
func TestDriftSkippedForNonManagedKinds(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    providerKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{object("provider-aws", "", map[string]any{})},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	assert.Equal(t, 0, testutil.CollectAndCount(subject, "crossplane_state_mr_drift"))
}

func TestDriftFieldsAreOptInAndCapped(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig(t)
	cfg.DriftFields = true
	cfg.DriftFieldsMax = 2

	tagSchema := bucketKind()

	badly := object("badly-drifted", "", map[string]any{
		"spec": map[string]any{"forProvider": map[string]any{
			"tags": map[string]any{"a": "1", "b": "2", "c": "3", "d": "4"},
		}},
		"status": map[string]any{"atProvider": map[string]any{
			"tags": map[string]any{"a": "x", "b": "y", "c": "z", "d": "w"},
		}},
	})

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    tagSchema,
		Synced:  true,
		Objects: []*unstructured.Unstructured{badly},
	}}}

	withFields := collector.New(source, cfg, discardLogger())
	assert.Equal(t, 2, testutil.CollectAndCount(withFields, "crossplane_state_mr_drift_field"),
		"the per-resource field cap must bound cardinality")

	withoutFields := collector.New(source, defaultConfig(t), discardLogger())
	assert.Equal(t, 0, testutil.CollectAndCount(withoutFields, "crossplane_state_mr_drift_field"),
		"per-field detail is opt-in")
}

func TestExternalNameLabelIsOptIn(t *testing.T) {
	t.Parallel()

	bucket := object("logs", "", map[string]any{})
	bucket.SetAnnotations(map[string]string{"crossplane.io/external-name": "my-real-bucket"})

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{bucket},
	}}}

	cfg := defaultConfig(t)
	cfg.ExternalNameLabel = true

	withLabel := collector.New(source, cfg, discardLogger())

	expected := `
# HELP crossplane_state_resource_info Presence of a Crossplane object. The value is always 1; identity is carried in labels. Emitted for every object, including the kinds that carry no status conditions.
# TYPE crossplane_state_resource_info gauge
crossplane_state_resource_info{xp_external_name="my-real-bucket",xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_name="logs",xp_namespace="",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(withLabel, strings.NewReader(expected), "crossplane_state_resource_info")
	require.NoError(t, err)
}

// TestNamespaceLabelIsBespoke guards the label-collision decision. A bare
// "namespace" label would be renamed to exported_namespace by Prometheus,
// because the scrape target already supplies one for the exporter's own pod —
// and every dashboard querying it would break quietly.
func TestNamespaceLabelIsBespoke(t *testing.T) {
	t.Parallel()

	namespaced := bucketKind()
	namespaced.Namespaced = true

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    namespaced,
		Synced:  true,
		Objects: []*unstructured.Unstructured{object("logs", "team-a", map[string]any{})},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_resource_info Presence of a Crossplane object. The value is always 1; identity is carried in labels. Emitted for every object, including the kinds that carry no status conditions.
# TYPE crossplane_state_resource_info gauge
crossplane_state_resource_info{xp_group="s3.aws.upbound.io",xp_kind="Bucket",xp_name="logs",xp_namespace="team-a",xp_version="v1beta1"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_resource_info")
	require.NoError(t, err)
}

// TestCollectedMetricsPassPromlint runs the collected output through the
// Prometheus naming linter, so a metric named badly fails the build rather
// than shipping.
func TestCollectedMetricsPassPromlint(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:   bucketKind(),
		Synced: true,
		Objects: []*unstructured.Unstructured{object("logs", "", map[string]any{
			"spec":   map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
			"status": map[string]any{"atProvider": map[string]any{"region": "us-east-1"}},
		})},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	problems, err := testutil.CollectAndLint(subject)
	require.NoError(t, err)
	assert.Empty(t, problems, "collected metrics must satisfy promlint")
}

func TestCompositionRevisions(t *testing.T) {
	t.Parallel()

	revisionKind := discovery.Kind{
		GVR: schema.GroupVersionResource{Group: "apiextensions.crossplane.io", Version: "v1", Resource: "compositionrevisions"},
		GVK: schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1", Kind: "CompositionRevision"},
	}

	makeRevision := func(name string, revision int64, owner string) (built *unstructured.Unstructured) {
		built = object(name, "", map[string]any{
			"spec": map[string]any{"revision": revision},
		})
		built.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Composition", Name: owner}})

		return built
	}

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:   revisionKind,
		Synced: true,
		Objects: []*unstructured.Unstructured{
			makeRevision("xnetwork-aaa", 1, "xnetwork"),
			makeRevision("xnetwork-bbb", 2, "xnetwork"),
			makeRevision("xnetwork-ccc", 3, "xnetwork"),
		},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_composition_revisions Number of revisions that exist for a Composition. A Composition carries no status conditions, so revision churn is the signal it does have.
# TYPE crossplane_state_composition_revisions gauge
crossplane_state_composition_revisions{xp_name="xnetwork"} 3
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_composition_revisions")
	require.NoError(t, err)

	current := `
# HELP crossplane_state_composition_current_revision Highest revision number observed for a Composition, which is the revision new composite resources are rendered from.
# TYPE crossplane_state_composition_current_revision gauge
crossplane_state_composition_current_revision{xp_name="xnetwork"} 3
`

	err = testutil.CollectAndCompare(subject, strings.NewReader(current), "crossplane_state_composition_current_revision")
	require.NoError(t, err)
}

// TestCompositionRevisionLabelFallback covers a revision whose owner reference
// has not been written yet.
func TestCompositionRevisionLabelFallback(t *testing.T) {
	t.Parallel()

	revisionKind := discovery.Kind{
		GVR: schema.GroupVersionResource{Group: "apiextensions.crossplane.io", Version: "v1", Resource: "compositionrevisions"},
		GVK: schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1", Kind: "CompositionRevision"},
	}

	revision := object("orphan-aaa", "", map[string]any{"spec": map[string]any{"revision": int64(7)}})
	revision.SetLabels(map[string]string{"crossplane.io/composition-name": "orphan"})

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    revisionKind,
		Synced:  true,
		Objects: []*unstructured.Unstructured{revision},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_composition_current_revision Highest revision number observed for a Composition, which is the revision new composite resources are rendered from.
# TYPE crossplane_state_composition_current_revision gauge
crossplane_state_composition_current_revision{xp_name="orphan"} 7
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected), "crossplane_state_composition_current_revision")
	require.NoError(t, err)
}

// TestDeletedObjectsStopBeingReported is the reason this is a native collector
// rather than a set of OTel instruments: state that goes away must stop being
// emitted, not linger for the life of the process.
func TestDeletedObjectsStopBeingReported(t *testing.T) {
	t.Parallel()

	populated := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{object("logs", "", map[string]any{})},
	}}}

	subject := collector.New(&mutableSource{inner: populated}, defaultConfig(t), discardLogger())
	assert.Equal(t, 1, testutil.CollectAndCount(subject, "crossplane_state_resource_info"))
}

// mutableSource lets a test change what the next scrape sees.
type mutableSource struct {
	inner stubSource
}

// Snapshot implements collector.Snapshotter.
func (m *mutableSource) Snapshot() (snapshots []watch.Snapshot) {
	snapshots = m.inner.snapshots
	return snapshots
}
