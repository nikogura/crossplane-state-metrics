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

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
	"github.com/nikogura/crossplane-state-metrics/pkg/observability"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// dashboardPath is the checked-in Grafana dashboard this test validates.
const dashboardPath = "../../dashboards/crossplane-state-metrics.json"

// metricNamePattern finds this exporter's metric names inside PromQL.
var metricNamePattern = regexp.MustCompile(`crossplane_state_[a-z0-9_]+`)

// histogramSuffixes are the series a histogram instrument expands into. A
// dashboard querying crossplane_state_scrape_duration_seconds_bucket is
// referencing the crossplane_state_scrape_duration_seconds instrument.
//
//nolint:gochecknoglobals // an immutable suffix list
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

// stubSource supplies one object of each interesting shape so every metric
// family the collector can emit is actually emitted.
type stubSource struct{}

// Snapshot implements collector.Snapshotter.
func (stubSource) Snapshot() (snapshots []watch.Snapshot) {
	managed := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"forProvider": map[string]any{"region": "us-east-1"}},
		"status": map[string]any{
			"atProvider": map[string]any{"region": "us-west-2"},
			"conditions": []any{
				map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess"},
				map[string]any{"type": "Ready", "status": "False", "reason": "Unavailable"},
			},
		},
	}}
	managed.SetName("bucket")
	managed.SetUID("bucket-uid")
	managed.SetResourceVersion("1")

	// Two more objects of a different kind, so one kind stays below the
	// aggregation threshold and reports per object while the other crosses it
	// and rolls up. Both families of metric are then emitted, and the dashboard
	// can be checked against all of them.
	crowded := managed.DeepCopy()
	crowded.SetName("instance-b")
	crowded.SetUID("instance-b-uid")

	crowdedTwin := managed.DeepCopy()
	crowdedTwin.SetName("instance-c")
	crowdedTwin.SetUID("instance-c-uid")

	revision := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"revision": int64(3)},
	}}
	revision.SetName("xnetwork-aaa")
	revision.SetUID("rev-uid")
	revision.SetResourceVersion("1")
	revision.SetLabels(map[string]string{"crossplane.io/composition-name": "xnetwork"})

	definition := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"state": "Inactive",
			"group": "dynamodb.aws.upbound.io",
			"names": map[string]any{"kind": "Table"},
		},
	}}
	definition.SetName("tables.dynamodb.aws.upbound.io")
	definition.SetUID("mrd-uid")
	definition.SetResourceVersion("1")

	snapshots = []watch.Snapshot{
		{
			Kind: discovery.Kind{
				GVR:             schema.GroupVersionResource{Group: "s3.aws.upbound.io", Version: "v1beta1", Resource: "buckets"},
				GVK:             schema.GroupVersionKind{Group: "s3.aws.upbound.io", Version: "v1beta1", Kind: "Bucket"},
				Managed:         true,
				DriftComparable: true,
				ForProvider: &drift.Schema{Type: "object", Properties: map[string]*drift.Schema{
					"region": {Type: "string"},
				}},
			},
			Synced:  true,
			Objects: []*unstructured.Unstructured{managed},
		},
		{
			Kind: discovery.Kind{
				GVR:             schema.GroupVersionResource{Group: "rds.aws.upbound.io", Version: "v1beta1", Resource: "instances"},
				GVK:             schema.GroupVersionKind{Group: "rds.aws.upbound.io", Version: "v1beta1", Kind: "Instance"},
				Managed:         true,
				DriftComparable: true,
				ForProvider: &drift.Schema{Type: "object", Properties: map[string]*drift.Schema{
					"region": {Type: "string"},
				}},
			},
			Synced:  true,
			Objects: []*unstructured.Unstructured{crowded, crowdedTwin},
		},
		{
			Kind: discovery.Kind{
				GVR: schema.GroupVersionResource{Group: "apiextensions.crossplane.io", Version: "v1", Resource: "compositionrevisions"},
				GVK: schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1", Kind: "CompositionRevision"},
			},
			Synced:  true,
			Objects: []*unstructured.Unstructured{revision},
		},
		{
			Kind: discovery.Kind{
				GVR: schema.GroupVersionResource{Group: "apiextensions.crossplane.io", Version: "v1alpha1", Resource: "managedresourcedefinitions"},
				GVK: schema.GroupVersionKind{Group: "apiextensions.crossplane.io", Version: "v1alpha1", Kind: "ManagedResourceDefinition"},
			},
			Synced:  true,
			Objects: []*unstructured.Unstructured{definition},
		},
	}

	return snapshots
}

// emittedMetricNames boots the real observability stack and collector, then
// returns every metric family name a scrape would actually produce.
func emittedMetricNames(t *testing.T) (names map[string]struct{}) {
	t.Helper()

	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)

	providers, err := observability.Init(ctx, observability.Options{
		ServiceName: serviceName,
		Version:     "test",
	}, logger)
	require.NoError(t, err)

	err = metrics.Init(otel.Meter(otelScope), "test")
	require.NoError(t, err)

	// Aggregation is exercised alongside the per-object path: the threshold of
	// one means any kind with more than a single object rolls up, so both
	// families of metric are emitted and the dashboard can be checked against
	// all of them.
	cfg, err := config.Load([]string{
		"--drift-fields=true",
		"--external-name-label=true",
		"--aggregate-threshold=1",
	})
	require.NoError(t, err)

	stateCollector := collector.New(stubSource{}, cfg, logger)

	err = providers.StateRegistry.Register(stateCollector)
	require.NoError(t, err)

	// Exercise the instruments that only record on activity, so their families
	// exist by the time the registry is gathered.
	metrics.RecordScrape(ctx, 0.1)
	metrics.RecordScrapeError(ctx, "test")
	metrics.RecordInformerSyncError(ctx, "test/v1, Resource=things")
	metrics.RecordCRDEvent(ctx, "register")
	metrics.RecordDrift(ctx, 0.01)
	metrics.RecordDriftError(ctx)
	metrics.RecordKubeRequest(ctx, "GET", "200")
	metrics.RecordKubeRequestDuration(ctx, "GET", 0.01)
	metrics.ScrapeStarted(ctx)
	metrics.ScrapeFinished(ctx)
	metrics.SetKindsWatched(2)
	metrics.SetInformerSyncState(2, 0)

	families, err := providers.Gatherer().Gather()
	require.NoError(t, err)

	names = make(map[string]struct{}, len(families))
	for _, family := range families {
		names[family.GetName()] = struct{}{}
	}

	return names
}

// dashboardMetricReferences extracts every crossplane_state_* name the
// dashboard's queries mention.
func dashboardMetricReferences(t *testing.T) (referenced []string) {
	t.Helper()

	raw, err := os.ReadFile(dashboardPath)
	require.NoError(t, err)

	var dashboard any

	err = json.Unmarshal(raw, &dashboard)
	require.NoError(t, err)

	unique := map[string]struct{}{}

	for _, expr := range collectExpressions(dashboard) {
		for _, name := range metricNamePattern.FindAllString(expr, -1) {
			unique[name] = struct{}{}
		}
	}

	for name := range unique {
		referenced = append(referenced, name)
	}

	sort.Strings(referenced)

	return referenced
}

// collectExpressions walks the decoded dashboard for query strings.
func collectExpressions(node any) (expressions []string) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			text, isText := value.(string)
			if isText && (key == "expr" || key == "query") {
				expressions = append(expressions, text)
				continue
			}

			expressions = append(expressions, collectExpressions(value)...)
		}
	case []any:
		for _, item := range typed {
			expressions = append(expressions, collectExpressions(item)...)
		}
	}

	return expressions
}

// TestDashboardReferencesOnlyEmittedMetrics is the guard against the classic
// silent defect: a dashboard panel querying a metric name that the code does
// not emit renders an empty graph forever, and nobody notices until an
// incident. Renaming an instrument without updating the dashboard fails here.
func TestDashboardReferencesOnlyEmittedMetrics(t *testing.T) {
	emitted := emittedMetricNames(t)
	referenced := dashboardMetricReferences(t)

	require.NotEmpty(t, referenced, "the dashboard must query this exporter's metrics")

	var missing []string

	for _, name := range referenced {
		if isEmitted(name, emitted) {
			continue
		}

		missing = append(missing, name)
	}

	assert.Empty(t, missing, "dashboard references metrics the exporter never emits: %v", missing)
}

// isEmitted reports whether a referenced name maps to an emitted family,
// allowing for the series a histogram expands into.
func isEmitted(name string, emitted map[string]struct{}) (found bool) {
	_, found = emitted[name]
	if found {
		return found
	}

	for _, suffix := range histogramSuffixes {
		base := strings.TrimSuffix(name, suffix)
		if base == name {
			continue
		}

		_, found = emitted[base]
		if found {
			return found
		}
	}

	return found
}

// TestDashboardUsesTemplatedDatasources keeps the dashboard portable: a
// hardcoded datasource UID imports into exactly one Grafana and silently
// breaks in every other.
func TestDashboardUsesTemplatedDatasources(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(dashboardPath)
	require.NoError(t, err)

	var dashboard map[string]any

	err = json.Unmarshal(raw, &dashboard)
	require.NoError(t, err)

	for _, uid := range collectDatasourceUIDs(dashboard) {
		assert.True(t, strings.HasPrefix(uid, "${"),
			"datasource uid %q must be a template variable, not a hardcoded UID", uid)
	}
}

// collectDatasourceUIDs gathers every datasource uid referenced by a panel.
func collectDatasourceUIDs(node any) (uids []string) {
	switch typed := node.(type) {
	case map[string]any:
		source, hasSource := typed["datasource"].(map[string]any)
		if hasSource {
			uid, hasUID := source["uid"].(string)
			if hasUID {
				uids = append(uids, uid)
			}
		}

		for key, value := range typed {
			if key == "datasource" {
				continue
			}

			uids = append(uids, collectDatasourceUIDs(value)...)
		}
	case []any:
		for _, item := range typed {
			uids = append(uids, collectDatasourceUIDs(item)...)
		}
	}

	return uids
}
