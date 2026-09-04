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

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
)

// sampleObject builds an object with everything the transform might strip.
func sampleObject() (object *unstructured.Unstructured) {
	object = &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "logs",
			"managedFields": []any{
				map[string]any{"manager": "crossplane", "operation": "Apply"},
			},
			"annotations": map[string]any{"crossplane.io/external-name": "real-bucket"},
		},
		"spec": map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		"status": map[string]any{
			"atProvider": map[string]any{"arn": "arn:aws:s3:::logs", "region": "us-east-1"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "Available"},
			},
		},
	}}

	return object
}

// transformed runs the transform for a kind and returns the stored object.
func transformed(t *testing.T, kind discovery.Kind) (object *unstructured.Unstructured) {
	t.Helper()

	result, err := trimTransform(kind)(sampleObject())
	require.NoError(t, err)

	var ok bool

	object, ok = result.(*unstructured.Unstructured)
	require.True(t, ok)

	return object
}

// TestTrimKeepsWhatIsRead covers a drift-comparable managed resource: both
// halves of the comparison must survive, and so must the conditions and the
// external-name annotation.
func TestTrimKeepsWhatIsRead(t *testing.T) {
	t.Parallel()

	kind := discovery.Kind{
		GVK:             schema.GroupVersionKind{Group: "s3.aws.upbound.io", Kind: "Bucket"},
		Managed:         true,
		DriftComparable: true,
	}

	object := transformed(t, kind)

	_, found, err := unstructured.NestedMap(object.Object, "spec", "forProvider")
	require.NoError(t, err)
	assert.True(t, found, "the declared half of the drift comparison must survive")

	_, found, err = unstructured.NestedMap(object.Object, "status", "atProvider")
	require.NoError(t, err)
	assert.True(t, found, "the observed half of the drift comparison must survive")

	_, found, err = unstructured.NestedSlice(object.Object, "status", "conditions")
	require.NoError(t, err)
	assert.True(t, found, "conditions are read for every kind")

	assert.Equal(t, "real-bucket", object.GetAnnotations()["crossplane.io/external-name"])

	// managedFields is never read and is routinely the largest part of a
	// Crossplane object.
	_, found, err = unstructured.NestedSlice(object.Object, "metadata", "managedFields")
	require.NoError(t, err)
	assert.False(t, found, "managedFields must always be dropped")
}

// TestTrimDropsSpecForNonComparableKinds covers everything that is not a
// drift-comparable managed resource: composites, packages, XRDs, Compositions.
// Their spec is never read, and neither is atProvider.
func TestTrimDropsSpecForNonComparableKinds(t *testing.T) {
	t.Parallel()

	kind := discovery.Kind{GVK: schema.GroupVersionKind{Group: "pkg.crossplane.io", Kind: "Provider"}}

	object := transformed(t, kind)

	_, hasSpec := object.Object["spec"]
	assert.False(t, hasSpec, "spec is dead weight for a kind that is never drift-compared")

	_, found, err := unstructured.NestedMap(object.Object, "status", "atProvider")
	require.NoError(t, err)
	assert.False(t, found)

	_, found, err = unstructured.NestedSlice(object.Object, "status", "conditions")
	require.NoError(t, err)
	assert.True(t, found, "conditions must survive on every kind")
}

// TestTrimKeepsSpecForActivationAndRevisions pins the two non-managed kinds
// whose spec the collector does read. Dropping their spec would silently break
// the activation gauge and the composition revision metrics.
func TestTrimKeepsSpecForActivationAndRevisions(t *testing.T) {
	t.Parallel()

	for _, kindName := range []string{"ManagedResourceDefinition", "CompositionRevision"} {
		t.Run(kindName, func(t *testing.T) {
			t.Parallel()

			kind := discovery.Kind{
				GVK: schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Kind: kindName},
			}

			object := transformed(t, kind)

			_, hasSpec := object.Object["spec"]
			assert.True(t, hasSpec, "%s has spec fields the collector reads", kindName)
		})
	}
}

// TestTrimPassesThroughUnknownInput keeps tombstones and anything unexpected
// intact rather than dropping them on the floor.
func TestTrimPassesThroughUnknownInput(t *testing.T) {
	t.Parallel()

	kind := discovery.Kind{GVK: schema.GroupVersionKind{Group: "pkg.crossplane.io", Kind: "Provider"}}

	result, err := trimTransform(kind)("not an object")
	require.NoError(t, err)
	assert.Equal(t, "not an object", result)
}
