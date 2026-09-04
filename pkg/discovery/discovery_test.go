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

package discovery_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
)

// loadCRD reads a CustomResourceDefinition from testdata. The fixtures are
// unmodified copies of real Crossplane CRDs — the core apiextensions and pkg
// definitions, plus a provider's managed resource and ProviderConfig — so the
// parser is exercised against what actually ships, not a hand-written idea of
// it.
func loadCRD(t *testing.T, name string) (object *unstructured.Unstructured) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	content := map[string]any{}
	err = yaml.Unmarshal(raw, &content)
	require.NoError(t, err)

	object = &unstructured.Unstructured{Object: content}

	return object
}

func TestMatcher(t *testing.T) {
	t.Parallel()

	matcher := discovery.NewMatcher(
		[]string{"crossplane", "composite", "claim"},
		[]string{"*.crossplane.io", "*.upbound.io"},
		nil,
		nil,
	)

	cases := []struct {
		name       string
		group      string
		kind       string
		categories []string
		expected   bool
	}{
		{
			name:       "managed resource matches on category",
			group:      "s3.aws.upbound.io",
			kind:       "Bucket",
			categories: []string{"crossplane", "managed", "aws"},
			expected:   true,
		},
		{
			name:       "core kind matches on category",
			group:      "apiextensions.crossplane.io",
			kind:       "Composition",
			categories: []string{"crossplane"},
			expected:   true,
		},
		{
			// ProviderConfig ships with no categories at all; the group glob is
			// the only thing that finds it.
			name:       "category-less kind matches on group glob",
			group:      "talos.crossplane.io",
			kind:       "ProviderConfig",
			categories: nil,
			expected:   true,
		},
		{
			name:       "category match is case-insensitive",
			group:      "example.io",
			kind:       "Thing",
			categories: []string{"Crossplane"},
			expected:   true,
		},
		{
			// crossplane-runtime appends "composite" to every CRD it generates
			// from an XRD. Those CRDs carry no "crossplane" category and live in
			// whatever API group the XRD author chose, so the category is the
			// only thing that finds them. Without it the composite layer - the
			// one a platform team actually works in - is invisible.
			name:       "composite resource matches on the composite category",
			group:      "platform.example.dev",
			kind:       "XEksCluster",
			categories: []string{"composite"},
			expected:   true,
		},
		{
			name:       "claim matches on the claim category",
			group:      "platform.example.dev",
			kind:       "EksCluster",
			categories: []string{"claim"},
			expected:   true,
		},
		{
			// Crossplane v2 namespaced managed resources live under
			// *.m.upbound.io and must still be matched by the group glob.
			name:       "v2 namespaced managed resource matches",
			group:      "s3.aws.m.upbound.io",
			kind:       "Bucket",
			categories: []string{"crossplane", "managed"},
			expected:   true,
		},
		{
			name:       "unrelated kind does not match",
			group:      "apps",
			kind:       "Deployment",
			categories: []string{"all"},
			expected:   false,
		},
		{
			name:       "unrelated CRD does not match",
			group:      "cert-manager.io",
			kind:       "Certificate",
			categories: []string{"cert-manager"},
			expected:   false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual := matcher.Matches(testCase.group, testCase.kind, testCase.categories)
			assert.Equal(t, testCase.expected, actual)
		})
	}
}

func TestMatcherExclusions(t *testing.T) {
	t.Parallel()

	matcher := discovery.NewMatcher(
		[]string{"crossplane"},
		[]string{"*.crossplane.io"},
		[]string{"ProviderConfigUsage"},
		[]string{"*.noisy.example.io"},
	)

	assert.False(t, matcher.Matches("talos.crossplane.io", "ProviderConfigUsage", []string{"crossplane"}),
		"kind exclusion must win over a category match")
	assert.False(t, matcher.Matches("talos.crossplane.io", "providerconfigusage", []string{"crossplane"}),
		"kind exclusion is case-insensitive")
	assert.False(t, matcher.Matches("a.noisy.example.io", "Thing", []string{"crossplane"}),
		"group exclusion must win over a category match")
	assert.True(t, matcher.Matches("talos.crossplane.io", "Configuration", []string{"crossplane"}),
		"unexcluded kinds still match")
}

// TestMatcherMalformedGlob pins the self-healing behaviour: one bad pattern
// must not blind the exporter to every other kind.
func TestMatcherMalformedGlob(t *testing.T) {
	t.Parallel()

	matcher := discovery.NewMatcher(nil, []string{"[", "*.crossplane.io"}, nil, nil)

	assert.True(t, matcher.Matches("talos.crossplane.io", "ProviderConfig", nil),
		"a malformed glob must be skipped, not abort matching")
}

func TestParseCRDManagedResource(t *testing.T) {
	t.Parallel()

	kind, usable := discovery.ParseCRD(loadCRD(t, "managed-resource.yaml"))
	require.True(t, usable)

	assert.Equal(t, "machine.talos.crossplane.io", kind.GVR.Group)
	assert.Equal(t, "configurations", kind.GVR.Resource)
	assert.Equal(t, "v1alpha1", kind.GVR.Version)
	assert.Equal(t, "Configuration", kind.GVK.Kind)
	assert.False(t, kind.Namespaced, "this managed resource is cluster scoped")
	assert.Contains(t, kind.Categories, "crossplane")
	assert.Contains(t, kind.Categories, "managed")

	require.True(t, kind.Managed, "a CRD with both forProvider and atProvider is a managed resource")

	// This is a real provider that reports only computed results: forProvider
	// declares clusterName, node and machineType while atProvider carries only
	// generatedTime, machineConfiguration and machineConfigurationHash. The two
	// halves share no field, so there is nothing to compare and drift must not
	// be reported - otherwise every object of this kind would read as
	// permanently drifted.
	assert.False(t, kind.DriftComparable,
		"a provider that echoes none of the declared configuration back is not drift-comparable")
	assert.Nil(t, kind.ForProvider)
}

func TestParseCRDNonManagedKinds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		file       string
		group      string
		kindName   string
		resource   string
		version    string
		categories []string
	}{
		{
			name:       "package Provider is not a managed resource",
			file:       "provider.yaml",
			group:      "pkg.crossplane.io",
			kindName:   "Provider",
			resource:   "providers",
			version:    "v1",
			categories: []string{"crossplane", "pkg"},
		},
		{
			name:       "Composition is not a managed resource",
			file:       "composition.yaml",
			group:      "apiextensions.crossplane.io",
			kindName:   "Composition",
			resource:   "compositions",
			version:    "v1",
			categories: []string{"crossplane"},
		},
		{
			name:     "ProviderConfig is not a managed resource and has no categories",
			file:     "providerconfig.yaml",
			group:    "talos.crossplane.io",
			kindName: "ProviderConfig",
			resource: "providerconfigs",
			version:  "v1alpha1",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			kind, usable := discovery.ParseCRD(loadCRD(t, testCase.file))
			require.True(t, usable)

			assert.Equal(t, testCase.group, kind.GVR.Group)
			assert.Equal(t, testCase.resource, kind.GVR.Resource)
			assert.Equal(t, testCase.version, kind.GVR.Version)
			assert.Equal(t, testCase.kindName, kind.GVK.Kind)
			assert.False(t, kind.Managed)
			assert.Nil(t, kind.ForProvider)

			for _, category := range testCase.categories {
				assert.Contains(t, kind.Categories, category)
			}

			if len(testCase.categories) == 0 {
				assert.Empty(t, kind.Categories)
			}
		})
	}
}

// TestDiscoveryCoversEveryFixture is the integration check that the two
// discovery signals together find every real Crossplane CRD in testdata,
// including the category-less ProviderConfig.
func TestDiscoveryCoversEveryFixture(t *testing.T) {
	t.Parallel()

	matcher := discovery.NewMatcher(
		[]string{"crossplane"},
		[]string{"*.crossplane.io", "*.upbound.io"},
		nil,
		nil,
	)

	fixtures := []string{"managed-resource.yaml", "provider.yaml", "composition.yaml", "providerconfig.yaml"}

	for _, fixture := range fixtures {
		kind, usable := discovery.ParseCRD(loadCRD(t, fixture))
		require.True(t, usable, fixture)

		matched := matcher.Matches(kind.GVR.Group, kind.GVK.Kind, kind.Categories)
		assert.True(t, matched, "%s must be discovered", fixture)
	}
}

func TestParseCRDUnusable(t *testing.T) {
	t.Parallel()

	empty := &unstructured.Unstructured{Object: map[string]any{}}
	_, usable := discovery.ParseCRD(empty)
	assert.False(t, usable, "a CRD with no names is not usable")

	noServedVersion := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"group": "example.io",
			"names": map[string]any{"kind": "Thing", "plural": "things"},
			"versions": []any{
				map[string]any{"name": "v1", "served": false, "storage": true},
			},
		},
	}}

	_, usable = discovery.ParseCRD(noServedVersion)
	assert.False(t, usable, "a CRD with no served version is not usable")
}

func TestPreferredVersionPrefersStorage(t *testing.T) {
	t.Parallel()

	object := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"group": "example.io",
			"names": map[string]any{"kind": "Thing", "plural": "things"},
			"versions": []any{
				map[string]any{"name": "v1alpha1", "served": true, "storage": false},
				map[string]any{"name": "v1beta1", "served": true, "storage": true},
			},
		},
	}}

	kind, usable := discovery.ParseCRD(object)
	require.True(t, usable)
	assert.Equal(t, "v1beta1", kind.GVR.Version, "the storage version is preferred")
}

func TestPreferredVersionFallsBackToServed(t *testing.T) {
	t.Parallel()

	object := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"group": "example.io",
			"names": map[string]any{"kind": "Thing", "plural": "things"},
			"versions": []any{
				map[string]any{"name": "v1alpha1", "served": true, "storage": false},
				map[string]any{"name": "v1beta1", "served": false, "storage": true},
			},
		},
	}}

	kind, usable := discovery.ParseCRD(object)
	require.True(t, usable)
	assert.Equal(t, "v1alpha1", kind.GVR.Version, "an unserved storage version cannot be watched")
}

func TestEstablished(t *testing.T) {
	t.Parallel()

	established := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "NamesAccepted", "status": "True"},
			map[string]any{"type": "Established", "status": "True"},
		}},
	}}
	assert.True(t, discovery.Established(established))

	notYet := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Established", "status": "False"},
		}},
	}}
	assert.False(t, discovery.Established(notYet))

	noStatus := &unstructured.Unstructured{Object: map[string]any{}}
	assert.False(t, discovery.Established(noStatus))
}
