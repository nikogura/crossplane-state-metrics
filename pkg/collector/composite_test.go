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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// composed builds a managed resource owned by a composite, labelled the way
// crossplane-runtime labels everything a composite creates.
func composed(name string, composite string) (object *unstructured.Unstructured) {
	object = &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		"status": map[string]any{"atProvider": map[string]any{"region": "us-east-1"}},
	}}
	object.SetName(name)
	object.SetUID(k8stypes.UID(name + "-uid"))
	object.SetResourceVersion("1")
	object.SetLabels(map[string]string{
		"crossplane.io/composite":       composite,
		"crossplane.io/claim-name":      "env",
		"crossplane.io/claim-namespace": "team-a",
	})

	return object
}

// withCondition attaches a status condition.
func withCondition(object *unstructured.Unstructured, status string) (returned *unstructured.Unstructured) {
	_ = unstructured.SetNestedSlice(object.Object, []any{
		condition("Ready", status, "Reconciled"),
	}, "status", "conditions")

	returned = object

	return returned
}

// compositeConfig returns the default configuration with the ownership labels
// switched on.
func compositeConfig(t *testing.T) (cfg config.Config) {
	t.Helper()

	cfg = defaultConfig(t)
	cfg.CompositeLabel = true

	return cfg
}

// TestCompositeLabelIsOptIn keeps the default surface unchanged, so an existing
// deployment's series do not silently gain labels.
func TestCompositeLabelIsOptIn(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{withCondition(composed("bucket-abc", "example-env"), "True")},
	}}}

	off := collector.New(source, defaultConfig(t), discardLogger())
	require.NotContains(t, render(t, off), "xp_composite")

	on := collector.New(source, compositeConfig(t), discardLogger())
	require.Contains(t, render(t, on), `xp_composite="example-env"`)
}

// TestOwnershipReachesHealthAndDrift is why the label goes on more than the
// info metric. Filtering a dashboard to one environment must not change which
// signals remain visible: if drift carried the dimension and conditions did
// not, an environment filter would quietly hide unhealthy resources.
func TestOwnershipReachesHealthAndDrift(t *testing.T) {
	t.Parallel()

	drifted := composed("bucket-xyz", "example-env")
	_ = unstructured.SetNestedMap(drifted.Object, map[string]any{"region": "eu-west-1"}, "status", "atProvider")

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{withCondition(drifted, "False")},
	}}}

	text := render(t, collector.New(source, compositeConfig(t), discardLogger()))

	for _, metric := range []string{
		"crossplane_state_resource_info",
		"crossplane_state_resource_condition",
		"crossplane_state_mr_drift",
	} {
		line := lineFor(text, metric)
		require.NotEmpty(t, line, "%s must be emitted", metric)
		assert.Contains(t, line, `xp_composite="example-env"`,
			"%s must carry the composite dimension", metric)
	}
}

// TestNameRegexMissesWhatCompositeCatches reproduces the failure this exists to
// fix.
//
// Composed resources get generated names, and only some happen to contain the
// environment token. A dashboard filtering on xp_name=~".*example-env.*" matches the
// one that does and silently drops the rest — so a broken resource sits outside
// the filter and the environment reads as healthy. The composite label is
// stamped by crossplane-runtime on all of them regardless of name.
func TestNameRegexMissesWhatCompositeCatches(t *testing.T) {
	t.Parallel()

	// Two resources of the same composite. Only one is named after it.
	named := withCondition(composed("example-env-bucket", "example-env"), "True")
	generated := withCondition(composed("logs-x7f2q", "example-env"), "False")

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{named, generated},
	}}}

	text := render(t, collector.New(source, compositeConfig(t), discardLogger()))

	matchedByName := 0
	matchedByComposite := 0

	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "crossplane_state_resource_condition{") {
			continue
		}

		if strings.Contains(line, `xp_name="example-env`) {
			matchedByName++
		}

		if strings.Contains(line, `xp_composite="example-env"`) {
			matchedByComposite++
		}
	}

	assert.Equal(t, 1, matchedByName,
		"a name regex finds only the resource that happens to be named after the environment")
	assert.Equal(t, 2, matchedByComposite,
		"the composite label finds every resource the composite owns, including the unhealthy one")
}

// TestUncomposedResourcesGetEmptyOwnership keeps a provider or an XRD, which no
// composite owns, from being given a misleading value.
func TestUncomposedResourcesGetEmptyOwnership(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    providerKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{withCondition(object("provider-aws", "", map[string]any{}), "True")},
	}}}

	text := render(t, collector.New(source, compositeConfig(t), discardLogger()))

	assert.Contains(t, text, `xp_composite=""`,
		"an object no composite owns reports empty, which is queryable as such")
}

// TestCompositeLabelAddsNoSeries pins the cardinality claim. The ownership
// labels are functions of the object, which its identity already determines, so
// they widen existing series rather than creating new ones.
func TestCompositeLabelAddsNoSeries(t *testing.T) {
	t.Parallel()

	objects := []*unstructured.Unstructured{
		withCondition(composed("a", "example-env"), "True"),
		withCondition(composed("b", "example-env"), "True"),
		withCondition(composed("c", "example-env-2"), "False"),
	}

	source := stubSource{snapshots: []watch.Snapshot{{Kind: bucketKind(), Synced: true, Objects: objects}}}

	for _, metric := range []string{
		"crossplane_state_resource_info",
		"crossplane_state_resource_condition",
		"crossplane_state_mr_drift",
	} {
		without := testutil.CollectAndCount(collector.New(source, defaultConfig(t), discardLogger()), metric)
		with := testutil.CollectAndCount(collector.New(source, compositeConfig(t), discardLogger()), metric)

		assert.Equal(t, without, with, "%s must gain a label, not series", metric)
	}
}

// render gathers a collector as Prometheus text exposition format.
func render(t *testing.T, subject *collector.Collector) (text string) {
	t.Helper()

	registry := newRegistry(t, subject)

	families, err := registry.Gather()
	require.NoError(t, err)

	builder := &strings.Builder{}

	for _, family := range families {
		_, err = expfmt.MetricFamilyToText(builder, family)
		require.NoError(t, err)
	}

	text = builder.String()

	return text
}

// lineFor returns the first sample line for a metric name.
func lineFor(text string, name string) (line string) {
	for _, candidate := range strings.Split(text, "\n") {
		if strings.HasPrefix(candidate, name+"{") {
			line = candidate
			return line
		}
	}

	return line
}

// newRegistry registers a collector into a fresh registry.
func newRegistry(t *testing.T, subject *collector.Collector) (registry *prometheus.Registry) {
	t.Helper()

	registry = prometheus.NewRegistry()
	require.NoError(t, registry.Register(subject))

	return registry
}
