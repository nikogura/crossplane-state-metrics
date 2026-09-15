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

//go:build integration

// Package integration exercises the exporter against a real Kubernetes API
// server.
//
// The unit tests use a fake dynamic client, which proves the logic but not the
// wiring: it cannot show that CRD discovery survives a real watch, that
// informers actually sync against a real API server, or that the metrics an
// operator scrapes reflect objects that really exist. These tests start a real
// kube-apiserver and etcd through envtest, install real Crossplane CRDs into
// it, create real objects, and scrape the real registry.
//
// Build tag: integration. Run with `make integration-test`.
package integration

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// settle bounds how long a test waits for the informer machinery to observe a
// change made through the API server.
const settle = 30 * time.Second

// Resource coordinates used by these tests.
//
//nolint:gochecknoglobals // immutable resource coordinates shared by the tests
var (
	bucketGVR = schema.GroupVersionResource{
		Group: "s3.aws.upbound.io", Version: "v1beta1", Resource: "buckets",
	}
	providerGVR = schema.GroupVersionResource{
		Group: "pkg.crossplane.io", Version: "v1", Resource: "providers",
	}
	crdGVR = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	namespacedBucketGVR = schema.GroupVersionResource{
		Group: "s3.aws.m.upbound.io", Version: "v1beta1", Resource: "buckets",
	}
	compositeGVR = schema.GroupVersionResource{
		Group: "platform.example.dev", Version: "v1alpha1", Resource: "xeksclusters",
	}
	namespaceGVR  = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	definitionGVR = schema.GroupVersionResource{
		Group: "apiextensions.crossplane.io", Version: "v1alpha1", Resource: "managedresourcedefinitions",
	}
)

// harness is a running API server with the exporter watching it.
type harness struct {
	client   dynamic.Interface
	registry *prometheus.Registry
	manager  *watch.Manager
}

// start brings up envtest with the supplied CRD directories, starts the watch
// manager against it, and registers the collector.
func start(t *testing.T, cfg config.Config, crdDirs ...string) (running *harness) {
	t.Helper()

	environment := &envtest.Environment{
		CRDDirectoryPaths:     crdDirs,
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err := environment.Start()
	require.NoError(t, err, "starting envtest; run `make envtest-assets` if the binaries are missing")

	t.Cleanup(func() {
		_ = environment.Stop()
	})

	client, err := dynamic.NewForConfig(restConfig)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	manager := watch.New(client, watch.Options{
		Matcher: discovery.NewMatcher(cfg.Categories, cfg.Groups, cfg.ExcludeKinds, cfg.ExcludeGroups),
		Resync:  time.Minute,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = manager.Run(ctx)
	}()

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(collector.New(manager, cfg, logger)))

	require.Eventually(t, manager.Ready, settle, 100*time.Millisecond,
		"the CRD watch must complete its initial list")

	running = &harness{client: client, registry: registry, manager: manager}

	return running
}

// defaultConfig returns the zero-flag configuration with field detail enabled.
func defaultConfig(t *testing.T) (cfg config.Config) {
	t.Helper()

	var err error

	cfg, err = config.Load([]string{"--drift-fields=true"})
	require.NoError(t, err)

	return cfg
}

// crossplaneCRDs is the directory of real, unmodified Crossplane CRDs.
func crossplaneCRDs() (path string) {
	path = filepath.Join("..", "..", "pkg", "discovery", "testdata")
	return path
}

// localCRDs is this package's own fixture directory.
func localCRDs() (path string) {
	path = "testdata"
	return path
}

// gather renders the current metric families in Prometheus text exposition
// format: exactly the bytes an operator's Prometheus would scrape. Asserting
// against the protobuf debug rendering instead would test a representation
// nobody ever sees.
func (h *harness) gather(t *testing.T) (text string) {
	t.Helper()

	families, err := h.registry.Gather()
	require.NoError(t, err)

	rendered := &strings.Builder{}

	for _, family := range families {
		_, err = expfmt.MetricFamilyToText(rendered, family)
		require.NoError(t, err)
	}

	text = rendered.String()

	return text
}

// count returns how many series exist for a metric name.
func (h *harness) count(t *testing.T, name string) (found int) {
	t.Helper()

	var err error

	found, err = testutil.GatherAndCount(h.registry, name)
	require.NoError(t, err)

	return found
}

// TestDiscoversRealCrossplaneCRDs proves the central claim against a real API
// server: kinds are found by category and group glob, with nothing enumerated
// per kind.
func TestDiscoversRealCrossplaneCRDs(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())

	require.Eventually(t, func() (done bool) {
		kinds := map[string]bool{}
		for _, snapshot := range running.manager.Snapshot() {
			kinds[snapshot.Kind.GVK.Kind] = true
		}

		done = kinds["Bucket"] && kinds["Provider"] && kinds["Composition"] && kinds["ProviderConfig"]

		return done
	}, settle, 200*time.Millisecond,
		"category and group-glob discovery must find managed resources, packages, compositions and the category-less ProviderConfig")
}

// TestConditionsFromRealObjects writes a real status subresource and checks the
// metric an operator would scrape.
func TestConditionsFromRealObjects(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	bucket := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "s3.aws.upbound.io/v1beta1",
		"kind":       "Bucket",
		"metadata":   map[string]any{"name": "logs"},
		"spec":       map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
	}}

	created, err := running.client.Resource(bucketGVR).Create(ctx, bucket, metav1.CreateOptions{})
	require.NoError(t, err)

	err = unstructured.SetNestedSlice(created.Object, []any{
		map[string]any{
			"type": "Synced", "status": "True", "reason": "ReconcileSuccess",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		},
		map[string]any{
			"type": "Ready", "status": "False", "reason": "Unavailable",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		},
	}, "status", "conditions")
	require.NoError(t, err)

	err = unstructured.SetNestedMap(created.Object, map[string]any{"region": "us-east-1"}, "status", "atProvider")
	require.NoError(t, err)

	_, err = running.client.Resource(bucketGVR).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (done bool) {
		text := running.gather(t)
		done = strings.Contains(text, `condition="Ready"`) && strings.Contains(text, `status="False"`)

		return done
	}, settle, 200*time.Millisecond, "the Ready=False condition must reach the metrics")

	text := running.gather(t)
	require.Contains(t, text, `xp_kind="Bucket"`)
	require.Contains(t, text, `xp_name="logs"`)
	require.Contains(t, text, `reason="Unavailable"`)
}

// TestDriftAgainstRealObject drives the whole drift path through a real API
// server: declared spec, observed status, pruning, comparison, metric.
func TestDriftAgainstRealObject(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	create := func(name string, region string) (object *unstructured.Unstructured) {
		object = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "s3.aws.upbound.io/v1beta1",
			"kind":       "Bucket",
			"metadata":   map[string]any{"name": name},
			"spec": map[string]any{"forProvider": map[string]any{
				"region": region,
				"tags":   map[string]any{"Environment": "prod"},
			}},
		}}

		created, createErr := running.client.Resource(bucketGVR).Create(ctx, object, metav1.CreateOptions{})
		require.NoError(t, createErr)

		object = created

		return object
	}

	setObserved := func(object *unstructured.Unstructured, observed map[string]any) {
		err := unstructured.SetNestedMap(object.Object, observed, "status", "atProvider")
		require.NoError(t, err)

		_, err = running.client.Resource(bucketGVR).UpdateStatus(ctx, object, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	// Matches, plus provider-computed fields and a provider-injected tag that
	// must NOT register as drift.
	clean := create("clean", "us-east-1")
	setObserved(clean, map[string]any{
		"region": "us-east-1",
		"tags":   map[string]any{"Environment": "prod", "aws:createdBy": "someone"},
		"arn":    "arn:aws:s3:::clean",
		"id":     "clean-1234",
	})

	// Someone changed the region out of band.
	drifted := create("drifted", "us-east-1")
	setObserved(drifted, map[string]any{
		"region": "eu-west-1",
		"tags":   map[string]any{"Environment": "prod"},
		"arn":    "arn:aws:s3:::drifted",
	})

	require.Eventually(t, func() (done bool) {
		text := running.gather(t)
		done = strings.Contains(text, `xp_name="drifted"`) && strings.Contains(text, `xp_name="clean"`)

		return done
	}, settle, 200*time.Millisecond)

	var text string

	require.Eventually(t, func() (done bool) {
		problems, err := testutil.GatherAndLint(running.registry)
		require.NoError(t, err)
		require.Empty(t, problems)

		text = running.gather(t)

		// The drifted bucket reads 1 and the clean one reads 0.
		done = strings.Contains(text, "crossplane_state_mr_drift") &&
			driftValue(text, "drifted") == 1 &&
			driftValue(text, "clean") == 0

		return done
	}, settle, 200*time.Millisecond,
		"provider-computed fields and injected tags must not count as drift, but a changed region must")

	// Assert against the state the wait verified, rather than re-reading and
	// hoping it still holds.
	require.Contains(t, text, `field="region"`, "the differing field path must be reported")
}

// driftValue extracts the crossplane_state_mr_drift value for one object from
// the rendered metric text.
func driftValue(text string, name string) (value int) {
	value = -1

	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "crossplane_state_mr_drift{") ||
			!strings.Contains(line, `xp_name="`+name+`"`) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		parsed, convErr := strconv.Atoi(fields[len(fields)-1])
		if convErr != nil {
			continue
		}

		value = parsed

		return value
	}

	return value
}

// TestNonComparableKindEmitsNoDrift is the regression test for the false
// positive that real CRDs exposed: provider-talos's Configuration declares
// clusterName, node and machineType while its atProvider carries only
// generatedTime and machineConfiguration. The halves share no field, so the
// kind is not drift-comparable and must emit no drift series at all - rather
// than reporting every object of that kind permanently drifted.
func TestNonComparableKindEmitsNoDrift(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs())
	ctx := context.Background()

	configGVR := schema.GroupVersionResource{
		Group: "machine.talos.crossplane.io", Version: "v1alpha1", Resource: "configurations",
	}

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "machine.talos.crossplane.io/v1alpha1",
		"kind":       "Configuration",
		"metadata":   map[string]any{"name": "node-1"},
		"spec": map[string]any{"forProvider": map[string]any{
			"clusterEndpoint":   "https://192.0.2.1:6443",
			"clusterName":       "prod",
			"machineType":       "controlplane",
			"node":              "192.0.2.10",
			"machineSecretsRef": map[string]any{"name": "secrets", "namespace": "default"},
		}},
	}}

	_, err := running.client.Resource(configGVR).Create(ctx, object, metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (done bool) {
		done = strings.Contains(running.gather(t), `xp_name="node-1"`)

		return done
	}, settle, 200*time.Millisecond, "the object must be observed")

	// Present in the metrics, but with no drift series.
	require.Zero(t, running.count(t, "crossplane_state_mr_drift"),
		"a kind whose provider echoes nothing back must emit no drift series")
	require.NotZero(t, running.count(t, "crossplane_state_resource_info"))
}

// TestDiscoversCRDInstalledAtRuntime is the headline claim: installing a
// provider mid-flight is picked up with no restart and no configuration change.
func TestDiscoversCRDInstalledAtRuntime(t *testing.T) {
	// Start with only the core CRDs; the Bucket kind does not exist yet.
	running := start(t, defaultConfig(t), crossplaneCRDs())
	ctx := context.Background()

	require.Never(t, func() (present bool) {
		for _, snapshot := range running.manager.Snapshot() {
			if snapshot.Kind.GVK.Kind == "Bucket" {
				present = true
			}
		}

		return present
	}, 2*time.Second, 200*time.Millisecond, "Bucket must not exist before its CRD is installed")

	raw, err := os.ReadFile(filepath.Join("testdata", "bucket-crd.yaml"))
	require.NoError(t, err)

	definition := &unstructured.Unstructured{}
	require.NoError(t, decodeYAML(raw, definition))

	_, err = running.client.Resource(crdGVR).Create(ctx, definition, metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (found bool) {
		for _, snapshot := range running.manager.Snapshot() {
			if snapshot.Kind.GVK.Kind == "Bucket" {
				found = true
			}
		}

		return found
	}, settle, 200*time.Millisecond,
		"a CRD installed while running must be discovered without a restart")

	// And the new kind actually serves metrics.
	bucket := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "s3.aws.upbound.io/v1beta1",
		"kind":       "Bucket",
		"metadata":   map[string]any{"name": "late"},
		"spec":       map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
	}}

	_, err = running.client.Resource(bucketGVR).Create(ctx, bucket, metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (done bool) {
		done = strings.Contains(running.gather(t), `xp_name="late"`)

		return done
	}, settle, 200*time.Millisecond, "objects of the newly discovered kind must be reported")
}

// TestDeletedObjectsStopBeingReported is why the state metrics are a native
// collector rather than OTel instruments: state that goes away must stop being
// emitted, not linger for the life of the process.
func TestDeletedObjectsStopBeingReported(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	provider := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "pkg.crossplane.io/v1",
		"kind":       "Provider",
		"metadata":   map[string]any{"name": "provider-aws-s3"},
		"spec":       map[string]any{"package": "xpkg.upbound.io/upbound/provider-aws-s3:v1"},
	}}

	_, err := running.client.Resource(providerGVR).Create(ctx, provider, metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (done bool) {
		done = strings.Contains(running.gather(t), `xp_name="provider-aws-s3"`)

		return done
	}, settle, 200*time.Millisecond)

	err = running.client.Resource(providerGVR).Delete(ctx, "provider-aws-s3", metav1.DeleteOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (gone bool) {
		gone = !strings.Contains(running.gather(t), `xp_name="provider-aws-s3"`)

		return gone
	}, settle, 200*time.Millisecond,
		"a deleted object must stop being emitted on the very next scrape")
}

// decodeYAML parses a single YAML document into an unstructured object.
func decodeYAML(raw []byte, into *unstructured.Unstructured) (err error) {
	content := map[string]any{}

	err = yaml.Unmarshal(raw, &content)
	if err != nil {
		return err
	}

	into.Object = content

	return err
}

// TestDiscoversCompositeResources is the regression test for a gap that the
// defaults alone would have left wide open.
//
// crossplane-runtime appends the "composite" category to every CRD it generates
// from an XRD, and does NOT add "crossplane". Those CRDs also live in whatever
// API group the XRD author chose, which matches no upbound or crossplane.io
// glob. Discovering by the crossplane category and group globs alone therefore
// misses composite resources entirely - the layer a platform team actually
// works in, and the middle of the provider health -> composite -> managed
// resource chain the dashboard draws.
func TestDiscoversCompositeResources(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	require.Eventually(t, func() (found bool) {
		for _, snapshot := range running.manager.Snapshot() {
			if snapshot.Kind.GVK.Kind == "XEksCluster" {
				found = true
			}
		}

		return found
	}, settle, 200*time.Millisecond,
		"a composite CRD must be discovered by its composite category despite a non-Crossplane API group")

	composite := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "platform.example.dev/v1alpha1",
		"kind":       "XEksCluster",
		"metadata":   map[string]any{"name": "prod-cluster"},
		"spec":       map[string]any{"parameters": map[string]any{"region": "us-east-1"}},
	}}

	created, err := running.client.Resource(compositeGVR).Create(ctx, composite, metav1.CreateOptions{})
	require.NoError(t, err)

	err = unstructured.SetNestedSlice(created.Object, []any{
		map[string]any{
			"type": "Ready", "status": "False", "reason": "Creating",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		},
	}, "status", "conditions")
	require.NoError(t, err)

	_, err = running.client.Resource(compositeGVR).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() (done bool) {
		text := running.gather(t)
		done = strings.Contains(text, `xp_kind="XEksCluster"`) &&
			strings.Contains(text, `xp_name="prod-cluster"`) &&
			strings.Contains(text, `reason="Creating"`)

		return done
	}, settle, 200*time.Millisecond, "composite conditions must reach the metrics")

	// A composite has no status.atProvider, so it is never drift-compared.
	require.Zero(t, running.count(t, "crossplane_state_mr_drift"),
		"composites have no observed state to compare against")
}

// TestNamespacedManagedResource covers Crossplane v2 namespaced managed
// resources, which live under *.m.upbound.io and are namespace-scoped.
//
// The object's own namespace must appear as xp_namespace. A bare "namespace"
// label would be overwritten at scrape time by the exporter pod's namespace,
// which is why every identity label carries the xp_ prefix.
func TestNamespacedManagedResource(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	for _, name := range []string{"team-a", "team-b"} {
		namespace := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": name},
		}}

		_, err := running.client.Resource(namespaceGVR).Create(ctx, namespace, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	for _, namespace := range []string{"team-a", "team-b"} {
		bucket := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "s3.aws.m.upbound.io/v1beta1",
			"kind":       "Bucket",
			"metadata":   map[string]any{"name": "data", "namespace": namespace},
			"spec":       map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		}}

		created, err := running.client.Resource(namespacedBucketGVR).Namespace(namespace).
			Create(ctx, bucket, metav1.CreateOptions{})
		require.NoError(t, err)

		// team-b's bucket has drifted; team-a's has not.
		observed := "us-east-1"
		if namespace == "team-b" {
			observed = "eu-west-1"
		}

		err = unstructured.SetNestedMap(created.Object,
			map[string]any{"region": observed, "arn": "arn:aws:s3:::" + namespace},
			"status", "atProvider")
		require.NoError(t, err)

		_, err = running.client.Resource(namespacedBucketGVR).Namespace(namespace).
			UpdateStatus(ctx, created, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	// Wait on the drift verdicts, not merely on the objects appearing. An
	// object is in resource_info as soon as it is created, while its drift
	// verdict needs the separate status update to have landed and been
	// observed — so waiting for the namespace to show up would race the
	// assertions below.
	var text string

	require.Eventually(t, func() (done bool) {
		text = running.gather(t)
		done = namespacedDriftValue(text, "team-a") == 0 &&
			namespacedDriftValue(text, "team-b") == 1

		return done
	}, settle, 200*time.Millisecond,
		"both namespaced objects must have their observed state recorded before comparing")

	require.Contains(t, text, `xp_namespace="team-a"`,
		"each namespaced object must report its own namespace, not the exporter's")
	require.Contains(t, text, `xp_namespace="team-b"`)

	// Two objects share the name "data" and differ only by namespace, so the
	// namespace label is what keeps them apart.
	require.NotContains(t, text, "exported_namespace",
		"a bare namespace label would have been renamed by the scrape")

	require.Equal(t, 1, namespacedDriftValue(text, "team-b"), "team-b has drifted")
	require.Equal(t, 0, namespacedDriftValue(text, "team-a"), "team-a has not")
}

// namespacedDriftValue reads the drift value for the object in one namespace.
func namespacedDriftValue(text string, namespace string) (value int) {
	value = -1

	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "crossplane_state_mr_drift{") ||
			!strings.Contains(line, `xp_namespace="`+namespace+`"`) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		parsed, convErr := strconv.Atoi(fields[len(fields)-1])
		if convErr != nil {
			continue
		}

		value = parsed

		return value
	}

	return value
}

// TestManagedResourceDefinitionActivation covers the Crossplane v2 activation
// model against a real API server, including the server-side default.
//
// A definition created without spec.state is defaulted to Inactive by the API
// server itself, so this also proves the exporter reads the effective value
// rather than the submitted one.
func TestManagedResourceDefinitionActivation(t *testing.T) {
	running := start(t, defaultConfig(t), crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	create := func(name string, group string, kind string, state string) {
		spec := map[string]any{
			"group": group,
			"names": map[string]any{"kind": kind, "plural": strings.ToLower(kind) + "s"},
		}

		if state != "" {
			spec["state"] = state
		}

		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.crossplane.io/v1alpha1",
			"kind":       "ManagedResourceDefinition",
			"metadata":   map[string]any{"name": name},
			"spec":       spec,
		}}

		_, err := running.client.Resource(definitionGVR).Create(ctx, object, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	create("buckets.s3.aws.upbound.io", "s3.aws.upbound.io", "Bucket", "Active")
	create("tables.dynamodb.aws.upbound.io", "dynamodb.aws.upbound.io", "Table", "Inactive")
	create("queues.sqs.aws.upbound.io", "sqs.aws.upbound.io", "Queue", "")

	require.Eventually(t, func() (done bool) {
		done = running.count(t, "crossplane_state_managed_resource_definition_active") == 3

		return done
	}, settle, 200*time.Millisecond, "every definition must be reported")

	text := running.gather(t)

	require.Equal(t, 1, definitionState(text, "buckets.s3.aws.upbound.io"))
	require.Equal(t, 0, definitionState(text, "tables.dynamodb.aws.upbound.io"))
	require.Equal(t, 0, definitionState(text, "queues.sqs.aws.upbound.io"),
		"an unset state is defaulted to Inactive by the API server and must read 0")

	require.Contains(t, text, `defines_kind="Bucket"`)
	require.Contains(t, text, `defines_group="s3.aws.upbound.io"`)
}

// definitionState reads the activation gauge for one definition.
func definitionState(text string, name string) (value int) {
	value = -1

	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "crossplane_state_managed_resource_definition_active{") ||
			!strings.Contains(line, `xp_name="`+name+`"`) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		parsed, convErr := strconv.Atoi(fields[len(fields)-1])
		if convErr != nil {
			continue
		}

		value = parsed

		return value
	}

	return value
}

// TestCompositeLabelFromRealObjects proves the environment dimension survives a
// real API server round trip, and that filtering on it finds what a name regex
// misses.
//
// Composed resources get generated names. A dashboard scoped to an environment
// by xp_name regex matches only the ones whose names happen to carry the
// environment token, and silently drops the rest — so a broken resource sits
// outside the filter and the environment reads as healthy. The composite label
// is stamped by crossplane-runtime on all of them regardless of name.
func TestCompositeLabelFromRealObjects(t *testing.T) {
	cfg := defaultConfig(t)
	cfg.CompositeLabel = true

	running := start(t, cfg, crossplaneCRDs(), localCRDs())
	ctx := context.Background()

	create := func(name string, healthy string) {
		bucket := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "s3.aws.upbound.io/v1beta1",
			"kind":       "Bucket",
			"metadata": map[string]any{
				"name": name,
				"labels": map[string]any{
					"crossplane.io/composite":       "example-env",
					"crossplane.io/claim-name":      "environment",
					"crossplane.io/claim-namespace": "team-a",
				},
			},
			"spec": map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		}}

		created, err := running.client.Resource(bucketGVR).Create(ctx, bucket, metav1.CreateOptions{})
		require.NoError(t, err)

		err = unstructured.SetNestedSlice(created.Object, []any{
			map[string]any{
				"type": "Ready", "status": healthy, "reason": "Reconciled",
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			},
		}, "status", "conditions")
		require.NoError(t, err)

		_, err = running.client.Resource(bucketGVR).UpdateStatus(ctx, created, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	// One named after the environment, one with a generated name. Both belong
	// to the same composite; only the generated one is unhealthy.
	create("example-env-logs", "True")
	create("data-x7f2q", "False")

	// Wait on the series the assertions actually read.
	//
	// An object appears in resource_info as soon as it is created, but its
	// condition series only exists once the separate status update has landed
	// and been observed. Waiting for the name to show up anywhere would
	// therefore pass while one object still had no condition, and the counts
	// below would race.
	var text string

	require.Eventually(t, func() (done bool) {
		text = running.gather(t)
		done = countConditionSeries(text, `xp_composite="example-env"`) == 2

		return done
	}, settle, 200*time.Millisecond,
		"both resources must have their conditions observed before counting")

	require.Contains(t, text, `xp_composite="example-env"`)
	require.Contains(t, text, `xp_claim="environment"`)
	require.Contains(t, text, `xp_claim_namespace="team-a"`)

	byName := countConditionSeries(text, `xp_name="example-env`)
	byComposite := countConditionSeries(text, `xp_composite="example-env"`)
	unhealthyFound := false

	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "crossplane_state_resource_condition{") {
			continue
		}

		if strings.Contains(line, `xp_composite="example-env"`) && strings.Contains(line, `status="False"`) {
			unhealthyFound = true
		}
	}

	assert.Equal(t, 1, byName,
		"a name regex finds only the resource named after the environment")
	assert.Equal(t, 2, byComposite,
		"the composite label finds every resource the composite owns")
	assert.True(t, unhealthyFound,
		"including the unhealthy one a name filter would have hidden")
}

// countConditionSeries counts resource_condition samples whose label set
// contains the supplied fragment.
func countConditionSeries(text string, fragment string) (count int) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "crossplane_state_resource_condition{") {
			continue
		}

		if strings.Contains(line, fragment) {
			count++
		}
	}

	return count
}
