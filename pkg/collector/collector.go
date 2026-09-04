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

// Package collector turns the informer caches into Prometheus metrics.
//
// It is a native prometheus.Collector rather than a set of OpenTelemetry
// instruments, deliberately. OTel instruments accumulate: once an attribute
// set has been recorded it is exported for the life of the process. That is
// correct for the exporter's own counters and wrong for object state — a
// deleted managed resource would keep reporting Ready forever. A collector
// that walks the caches on each scrape emits exactly the objects that exist
// at that moment, and objects that go away simply stop being emitted.
package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// Label names.
//
// Every label describing the Crossplane object carries an xp_ prefix. The rule
// is worth stating once and then never thinking about again: anything
// xp_-prefixed came from this exporter; anything unprefixed came from your
// scrape configuration.
//
// The prefix is not decoration. In a multi-cluster Prometheus or Thanos setup,
// external labels such as cluster and environment are stamped on at ingestion
// and silently overwrite any same-named label a target emits. And a bare
// "namespace" label resolves to the *exporter pod's* namespace via the scrape
// target, colliding with the resource's own namespace — Prometheus renames the
// emitted one to exported_namespace and operators write queries against the
// wrong one for months.
const (
	LabelGroup        = "xp_group"
	LabelVersion      = "xp_version"
	LabelKind         = "xp_kind"
	LabelName         = "xp_name"
	LabelNamespace    = "xp_namespace"
	LabelExternalName = "xp_external_name"
	LabelCondition    = "condition"
	LabelStatus       = "status"
	LabelReason       = "reason"
	LabelField        = "field"
)

// externalNameAnnotation is where Crossplane records the identifier a managed
// resource has at the provider.
const externalNameAnnotation = "crossplane.io/external-name"

// Snapshotter supplies the current contents of the informer caches. The
// collector takes the narrow interface rather than the concrete watch manager,
// so metric output can be exercised against fixed inputs without a cluster.
type Snapshotter interface {
	// Snapshot returns every watched kind together with its cached objects.
	Snapshot() (snapshots []watch.Snapshot)
}

// Collector emits Crossplane object state from the informer caches.
type Collector struct {
	source  Snapshotter
	options config.Config
	logger  *slog.Logger

	resourceInfo      *prometheus.Desc
	resourceCondition *prometheus.Desc
	drift             *prometheus.Desc
	driftField        *prometheus.Desc
	compositionCount  *prometheus.Desc
	compositionActive *prometheus.Desc
	definitionActive  *prometheus.Desc
	kindAggregated    *prometheus.Desc
	resourceCount     *prometheus.Desc
	conditionCount    *prometheus.Desc
	driftCount        *prometheus.Desc
	scrapeTruncated   *prometheus.Desc
	seriesEmitted     *prometheus.Desc

	// driftCache memoises drift results by object UID and resourceVersion.
	// Drift can only change when the object changes, so recomputing it for
	// every managed resource on every scrape is wasted work at any real fleet
	// size. The cache is pruned to the live object set on each scrape, so it
	// cannot outgrow the cluster.
	driftMutex sync.Mutex
	driftCache map[types.UID]cachedDrift
}

// cachedDrift is one memoised drift result.
type cachedDrift struct {
	resourceVersion string
	result          drift.Result
}

// identityLabels are the labels identifying one Crossplane object.
//
//nolint:gochecknoglobals // an immutable label-name list shared by several descriptors
var identityLabels = []string{LabelGroup, LabelVersion, LabelKind, LabelName, LabelNamespace}

// kindLabels identify a kind rather than an individual object, for the
// aggregate rollups.
//
//nolint:gochecknoglobals // an immutable label-name list shared by several descriptors
var kindLabels = []string{LabelGroup, LabelVersion, LabelKind}

// New builds a Collector reading from the supplied snapshot source.
func New(source Snapshotter, options config.Config, logger *slog.Logger) (collector *Collector) {
	infoLabels := identityLabels
	if options.ExternalNameLabel {
		infoLabels = append(append([]string{}, identityLabels...), LabelExternalName)
	}

	collector = &Collector{
		source:     source,
		options:    options,
		logger:     logger,
		driftCache: make(map[types.UID]cachedDrift),

		resourceInfo: prometheus.NewDesc(
			"crossplane_state_resource_info",
			"Presence of a Crossplane object. The value is always 1; identity is carried in labels. Emitted for every object, including the kinds that carry no status conditions.",
			infoLabels, nil),

		resourceCondition: prometheus.NewDesc(
			"crossplane_state_resource_condition",
			"One series per Crossplane object per status condition. The value is always 1 and the condition's state is carried in the status label, so a new condition type appears without a code change.",
			append(append([]string{}, identityLabels...), LabelCondition, LabelStatus, LabelReason), nil),

		drift: prometheus.NewDesc(
			"crossplane_state_mr_drift",
			"1 when a managed resource's declared spec.forProvider no longer matches its observed status.atProvider, 0 when they agree. Only managed resources have both halves, so only they are compared.",
			identityLabels, nil),

		driftField: prometheus.NewDesc(
			"crossplane_state_mr_drift_field",
			"One series per differing field path on a drifted managed resource. Opt-in, and capped per resource, because it multiplies cardinality by field count.",
			append(append([]string{}, identityLabels...), LabelField), nil),

		compositionCount: prometheus.NewDesc(
			"crossplane_state_composition_revisions",
			"Number of revisions that exist for a Composition. A Composition carries no status conditions, so revision churn is the signal it does have.",
			[]string{LabelName}, nil),

		compositionActive: prometheus.NewDesc(
			"crossplane_state_composition_current_revision",
			"Highest revision number observed for a Composition, which is the revision new composite resources are rendered from.",
			[]string{LabelName}, nil),

		definitionActive: prometheus.NewDesc(
			"crossplane_state_managed_resource_definition_active",
			"1 when a Crossplane v2 ManagedResourceDefinition is Active, 0 when Inactive. An inactive definition means the underlying CRD is never created, so that managed resource kind does not exist in the cluster and produces no metrics at all.",
			[]string{LabelName, LabelDefinesGroup, LabelDefinesKind}, nil),

		kindAggregated: prometheus.NewDesc(
			"crossplane_state_kind_aggregated",
			"1 for a kind reported in aggregate rather than per object, because it exceeded --aggregate-threshold. Per-object series for this kind are deliberately absent, not missing.",
			kindLabels, nil),

		resourceCount: prometheus.NewDesc(
			"crossplane_state_resource_count",
			"Number of objects of an aggregated kind.",
			kindLabels, nil),

		conditionCount: prometheus.NewDesc(
			"crossplane_state_condition_count",
			"Number of objects of an aggregated kind in a given condition state. The reason label is dropped here; rolling it up is the point of aggregating.",
			append(append([]string{}, kindLabels...), LabelCondition, LabelStatus), nil),

		driftCount: prometheus.NewDesc(
			"crossplane_state_drift_count",
			"Number of drifted managed resources of an aggregated kind.",
			kindLabels, nil),

		scrapeTruncated: prometheus.NewDesc(
			"crossplane_state_scrape_truncated",
			"1 when the series cap was reached and the scrape was cut short. Alert on this: the metrics are incomplete, and which objects survived is stable but arbitrary.",
			nil, nil),

		seriesEmitted: prometheus.NewDesc(
			"crossplane_state_series_emitted",
			"Series emitted by the state collector in the last scrape, for sizing against --max-series.",
			nil, nil),
	}

	return collector
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.resourceInfo
	ch <- c.resourceCondition
	ch <- c.drift
	ch <- c.driftField
	ch <- c.compositionCount
	ch <- c.compositionActive
	ch <- c.definitionActive
	ch <- c.kindAggregated
	ch <- c.resourceCount
	ch <- c.conditionCount
	ch <- c.driftCount
	ch <- c.scrapeTruncated
	ch <- c.seriesEmitted
}

// Collect implements prometheus.Collector, walking every watched kind's cache
// and emitting the current state.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	started := time.Now()

	metrics.ScrapeStarted(ctx)
	defer metrics.ScrapeFinished(ctx)

	snapshots := c.source.Snapshot()
	allowance := newBudget(c.options.MaxSeries)

	live := make(map[types.UID]struct{})

	var revisions []*unstructured.Unstructured

	var definitions []*unstructured.Unstructured

	var driftSeconds float64

	for _, snapshot := range orderedSnapshots(snapshots, allowance.limited) {
		if isCompositionRevision(snapshot) {
			revisions = append(revisions, snapshot.Objects...)
		}

		if isManagedResourceDefinition(snapshot) {
			definitions = append(definitions, snapshot.Objects...)
		}

		// UIDs are recorded for every object regardless of whether its series
		// were emitted, so a truncated scrape does not evict the drift cache
		// for objects it simply did not reach.
		for _, object := range snapshot.Objects {
			live[object.GetUID()] = struct{}{}
		}

		if c.shouldAggregate(snapshot) {
			c.emitAggregate(ctx, ch, snapshot, allowance)
			continue
		}

		for _, object := range snapshot.Objects {
			c.emitInfo(ch, snapshot, object, allowance)
			c.emitConditions(ch, snapshot, object, allowance)
		}

		driftSeconds += c.emitDrift(ctx, ch, snapshot, allowance)
	}

	c.emitCompositions(ch, revisions, allowance)
	c.emitActivation(ch, definitions, allowance)
	c.pruneDriftCache(live)

	truncated := float64(0)
	if allowance.truncated {
		truncated = 1

		metrics.RecordScrapeError(ctx, "series_cap_reached")
		c.logger.WarnContext(ctx, "series cap reached; scrape truncated",
			slog.Int("max_series", c.options.MaxSeries),
			slog.Int("emitted", allowance.emitted))
	}

	ch <- prometheus.MustNewConstMetric(c.scrapeTruncated, prometheus.GaugeValue, truncated)
	ch <- prometheus.MustNewConstMetric(c.seriesEmitted, prometheus.GaugeValue, float64(allowance.emitted))

	if c.options.Drift {
		metrics.RecordDrift(ctx, driftSeconds)
	}

	metrics.RecordScrape(ctx, time.Since(started).Seconds())
}

// emitInfo emits the presence metric for one object.
func (c *Collector) emitInfo(ch chan<- prometheus.Metric, snapshot watch.Snapshot, object *unstructured.Unstructured, allowance *budget) {
	if !allowance.take(1) {
		return
	}

	labels := identityValues(snapshot, object)

	if c.options.ExternalNameLabel {
		labels = append(labels, object.GetAnnotations()[externalNameAnnotation])
	}

	ch <- prometheus.MustNewConstMetric(c.resourceInfo, prometheus.GaugeValue, 1, labels...)
}

// emitConditions emits one series per status condition.
//
// Every Crossplane object — managed resources, composites, claims, package
// revisions, definitions and operations alike — carries the identical
// condition shape, so this one loop covers all of them and needs no per-kind
// knowledge. Synced/Ready, Installed/Healthy, Established/Offered and
// Succeeded all fall out of it.
func (c *Collector) emitConditions(ch chan<- prometheus.Metric, snapshot watch.Snapshot, object *unstructured.Unstructured, allowance *budget) {
	conditions, found, condErr := unstructured.NestedSlice(object.Object, "status", "conditions")
	if condErr != nil || !found {
		return
	}

	identity := identityValues(snapshot, object)

	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		conditionType, _, _ := unstructured.NestedString(condition, "type")
		if conditionType == "" {
			continue
		}

		if !allowance.take(1) {
			return
		}

		conditionStatus, _, _ := unstructured.NestedString(condition, "status")
		conditionReason, _, _ := unstructured.NestedString(condition, "reason")

		labels := append(append([]string{}, identity...), conditionType, conditionStatus, conditionReason)

		ch <- prometheus.MustNewConstMetric(c.resourceCondition, prometheus.GaugeValue, 1, labels...)
	}
}

// identityValues builds the label values identifying one object.
func identityValues(snapshot watch.Snapshot, object *unstructured.Unstructured) (values []string) {
	values = []string{
		snapshot.Kind.GVK.Group,
		snapshot.Kind.GVK.Version,
		snapshot.Kind.GVK.Kind,
		object.GetName(),
		object.GetNamespace(),
	}

	return values
}
