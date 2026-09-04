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

package watch_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// settle is how long a test waits for the asynchronous informer machinery to
// reach the expected state.
const settle = 5 * time.Second

// crdGVR and bucketGVR are the resources these tests exercise.
//
//nolint:gochecknoglobals // immutable resource coordinates shared by the tests
var (
	crdGVR = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	bucketGVR = schema.GroupVersionResource{
		Group: "s3.aws.upbound.io", Version: "v1beta1", Resource: "buckets",
	}
)

// discardLogger builds a logger that writes nowhere.
func discardLogger() (logger *slog.Logger) {
	logger = slog.New(slog.DiscardHandler)
	return logger
}

// bucketCRD builds a CustomResourceDefinition for a managed resource, in the
// same shape a provider ships: both categories, a forProvider schema, an
// atProvider schema, and an Established condition.
func bucketCRD(categories []any, established bool) (definition *unstructured.Unstructured) {
	status := "True"
	if !established {
		status = "False"
	}

	definition = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "buckets.s3.aws.upbound.io"},
		"spec": map[string]any{
			"group": "s3.aws.upbound.io",
			"names": map[string]any{
				"kind":       "Bucket",
				"plural":     "buckets",
				"categories": categories,
			},
			"scope": "Cluster",
			"versions": []any{map[string]any{
				"name":    "v1beta1",
				"served":  true,
				"storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"spec": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"forProvider": map[string]any{
									"type":       "object",
									"properties": map[string]any{"region": map[string]any{"type": "string"}},
								},
							},
						},
						"status": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"atProvider": map[string]any{
									"type": "object",
									"properties": map[string]any{
										// Echoes the declared field back, as an
										// upjet-generated provider does, so the
										// kind is drift-comparable.
										"region": map[string]any{"type": "string"},
										"arn":    map[string]any{"type": "string"},
									},
								},
							},
						},
					},
				}},
			}},
		},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Established", "status": status, "reason": "InitialNamesAccepted"},
		}},
	}}

	return definition
}

// bucketObject builds one managed resource instance.
func bucketObject(name string) (object *unstructured.Unstructured) {
	object = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "s3.aws.upbound.io/v1beta1",
		"kind":       "Bucket",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		"status":     map[string]any{"atProvider": map[string]any{"arn": "arn:aws:s3:::" + name}},
	}}

	return object
}

// newFakeClient builds a dynamic fake client seeded with the supplied objects.
func newFakeClient(objects ...runtime.Object) (client *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()

	listKinds := map[schema.GroupVersionResource]string{
		crdGVR:    "CustomResourceDefinitionList",
		bucketGVR: "BucketList",
	}

	client = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)

	return client
}

// runManager starts a manager in the background, stopping it when the test
// ends.
func runManager(t *testing.T, client *dynamicfake.FakeDynamicClient) (manager *watch.Manager) {
	t.Helper()

	options := watch.Options{
		Resync: time.Minute,
		Matcher: discovery.NewMatcher(
			[]string{"crossplane"},
			[]string{"*.crossplane.io", "*.upbound.io"},
			nil, nil,
		),
	}

	ctx, cancel := context.WithCancel(context.Background())
	manager = watch.New(client, options, discardLogger())

	go func() {
		_ = manager.Run(ctx)
	}()

	t.Cleanup(cancel)

	return manager
}

// TestDiscoversAndWatchesMatchingKind is the end-to-end path: a provider's CRD
// exists, so its objects turn up in the snapshot without anything being
// configured per kind.
func TestDiscoversAndWatchesMatchingKind(t *testing.T) {
	t.Parallel()

	client := newFakeClient(
		bucketCRD([]any{"crossplane", "managed"}, true),
		bucketObject("logs"),
		bucketObject("backups"),
	)

	manager := runManager(t, client)

	require.Eventually(t, func() (done bool) {
		for _, snapshot := range manager.Snapshot() {
			if snapshot.Kind.GVK.Kind == "Bucket" && len(snapshot.Objects) == 2 {
				done = true
			}
		}

		return done
	}, settle, 20*time.Millisecond, "the Bucket kind and its objects must be discovered")

	snapshots := manager.Snapshot()
	require.Len(t, snapshots, 1)

	assert.True(t, snapshots[0].Kind.Managed, "a CRD with forProvider and atProvider is a managed resource")
	assert.True(t, snapshots[0].Kind.DriftComparable, "atProvider echoes region back, so drift is comparable")
	assert.NotNil(t, snapshots[0].Kind.ForProvider)
	assert.True(t, snapshots[0].Synced)
}

// TestIgnoresNonMatchingCRD proves an unrelated CRD does not get watched.
func TestIgnoresNonMatchingCRD(t *testing.T) {
	t.Parallel()

	unrelated := bucketCRD([]any{"cert-manager"}, true)
	err := unstructured.SetNestedField(unrelated.Object, "cert-manager.io", "spec", "group")
	require.NoError(t, err)

	client := newFakeClient(unrelated)
	manager := runManager(t, client)

	require.Eventually(t, manager.Ready, settle, 20*time.Millisecond)

	assert.Empty(t, manager.Snapshot(), "a CRD matching neither category nor group glob must be ignored")
}

// TestIgnoresUnestablishedCRD proves the exporter waits for the API server to
// accept a definition before informing on it, rather than racing the install
// and generating spurious watch errors.
func TestIgnoresUnestablishedCRD(t *testing.T) {
	t.Parallel()

	client := newFakeClient(bucketCRD([]any{"crossplane", "managed"}, false))
	manager := runManager(t, client)

	require.Eventually(t, manager.Ready, settle, 20*time.Millisecond)

	assert.Empty(t, manager.Snapshot(), "an unestablished CRD must not be watched")
}

// TestDeregistersOnCRDDelete proves a provider uninstall takes its kinds back
// out of the metrics.
func TestDeregistersOnCRDDelete(t *testing.T) {
	t.Parallel()

	client := newFakeClient(bucketCRD([]any{"crossplane", "managed"}, true), bucketObject("logs"))
	manager := runManager(t, client)

	require.Eventually(t, func() (done bool) {
		done = len(manager.Snapshot()) == 1
		return done
	}, settle, 20*time.Millisecond)

	err := client.Resource(crdGVR).Delete(context.Background(), "buckets.s3.aws.upbound.io", deleteOptions())
	require.NoError(t, err)

	require.Eventually(t, func() (done bool) {
		done = len(manager.Snapshot()) == 0
		return done
	}, settle, 20*time.Millisecond, "deleting the CRD must stop the kind being reported")
}

// TestReadyReflectsCRDSync pins the readiness contract: not ready until the
// CRD watch has listed, because a partial view is worse than none.
func TestReadyReflectsCRDSync(t *testing.T) {
	t.Parallel()

	client := newFakeClient(bucketCRD([]any{"crossplane"}, true))
	manager := runManager(t, client)

	require.Eventually(t, manager.Ready, settle, 20*time.Millisecond)
	assert.True(t, manager.Ready())
}

// TestReadyBeforeRunIsFalse proves readiness does not claim health the
// exporter cannot verify.
func TestReadyBeforeRunIsFalse(t *testing.T) {
	t.Parallel()

	manager := watch.New(newFakeClient(), watch.Options{Resync: time.Minute}, discardLogger())
	assert.False(t, manager.Ready())
	assert.Empty(t, manager.Snapshot())
}

// deleteOptions returns default delete options for the fake client.
func deleteOptions() (options metav1.DeleteOptions) {
	options = metav1.DeleteOptions{}
	return options
}
