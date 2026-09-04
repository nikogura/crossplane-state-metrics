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

package collector

import (
	"context"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// budget tracks how many series remain in a scrape's allowance.
//
// Truncation is deterministic on purpose. If the objects that get dropped
// varied between scrapes, series would appear and vanish at random, every
// churned series would generate staleness markers, and the resulting TSDB churn
// would be worse than the cardinality the cap was meant to prevent. Kinds are
// walked in a stable order and objects sorted within them, so the same fleet
// truncates to the same set every time.
type budget struct {
	remaining int
	limited   bool
	truncated bool
	emitted   int
}

// newBudget creates a budget. A limit of zero means unlimited.
func newBudget(limit int) (allowance *budget) {
	allowance = &budget{remaining: limit, limited: limit > 0}
	return allowance
}

// take reserves n series, reporting whether the allowance covered them.
func (b *budget) take(count int) (allowed bool) {
	if !b.limited {
		b.emitted += count
		allowed = true

		return allowed
	}

	if b.remaining < count {
		b.truncated = true
		return allowed
	}

	b.remaining -= count
	b.emitted += count
	allowed = true

	return allowed
}

// orderedSnapshots returns the snapshots in a stable order, sorting each kind's
// objects when a series cap is in force. Sorting is skipped entirely when it is
// not, because it is pure cost on the uncapped path.
func orderedSnapshots(snapshots []watch.Snapshot, sortObjects bool) (ordered []watch.Snapshot) {
	ordered = make([]watch.Snapshot, len(snapshots))
	copy(ordered, snapshots)

	sort.SliceStable(ordered, func(i int, j int) (less bool) {
		less = ordered[i].Kind.GVK.String() < ordered[j].Kind.GVK.String()
		return less
	})

	if !sortObjects {
		return ordered
	}

	for index := range ordered {
		objects := make([]*unstructured.Unstructured, len(ordered[index].Objects))
		copy(objects, ordered[index].Objects)

		sort.SliceStable(objects, func(i int, j int) (less bool) {
			left, right := objects[i], objects[j]
			if left.GetNamespace() != right.GetNamespace() {
				less = left.GetNamespace() < right.GetNamespace()
				return less
			}

			less = left.GetName() < right.GetName()

			return less
		})

		ordered[index].Objects = objects
	}

	return ordered
}

// aggregate is the per-kind rollup emitted in place of per-object series.
type aggregate struct {
	objects    int
	drifted    int
	comparable bool
	conditions map[conditionKey]int
}

// conditionKey identifies one condition type and state within a kind.
type conditionKey struct {
	condition string
	status    string
}

// shouldAggregate reports whether a kind has grown past the point where
// per-object detail is worth its cardinality.
func (c *Collector) shouldAggregate(snapshot watch.Snapshot) (rollup bool) {
	if c.options.AggregateThreshold <= 0 {
		return rollup
	}

	rollup = len(snapshot.Objects) > c.options.AggregateThreshold

	return rollup
}

// emitAggregate rolls a whole kind up into counts.
//
// A kind that is numerous and individually uninteresting costs four series per
// object; the same kind in aggregate costs one series per condition state plus
// two. The trade is real and worth naming: per-object drilldown for that kind
// is gone, and crossplane_state_kind_aggregated marks it so the absence is
// visible rather than looking like the objects do not exist.
func (c *Collector) emitAggregate(ctx context.Context, ch chan<- prometheus.Metric, snapshot watch.Snapshot, allowance *budget) {
	rollup := aggregate{
		objects:    len(snapshot.Objects),
		comparable: snapshot.Kind.Managed && snapshot.Kind.DriftComparable,
		conditions: map[conditionKey]int{},
	}

	for _, object := range snapshot.Objects {
		countConditions(object, rollup.conditions)

		if c.options.Drift && rollup.comparable {
			result, ok := c.driftFor(ctx, snapshot, object)
			if ok && result.Drifted {
				rollup.drifted++
			}
		}
	}

	group, version, kind := snapshot.Kind.GVK.Group, snapshot.Kind.GVK.Version, snapshot.Kind.GVK.Kind

	if allowance.take(2) {
		ch <- prometheus.MustNewConstMetric(c.kindAggregated, prometheus.GaugeValue, 1, group, version, kind)
		ch <- prometheus.MustNewConstMetric(c.resourceCount, prometheus.GaugeValue,
			float64(rollup.objects), group, version, kind)
	}

	for key, count := range rollup.conditions {
		if !allowance.take(1) {
			return
		}

		ch <- prometheus.MustNewConstMetric(c.conditionCount, prometheus.GaugeValue,
			float64(count), group, version, kind, key.condition, key.status)
	}

	if rollup.comparable && c.options.Drift && allowance.take(1) {
		ch <- prometheus.MustNewConstMetric(c.driftCount, prometheus.GaugeValue,
			float64(rollup.drifted), group, version, kind)
	}
}

// countConditions tallies one object's conditions into the rollup.
func countConditions(object *unstructured.Unstructured, into map[conditionKey]int) {
	conditions, found, condErr := unstructured.NestedSlice(object.Object, "status", "conditions")
	if condErr != nil || !found {
		return
	}

	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		conditionType, _, _ := unstructured.NestedString(condition, "type")
		if conditionType == "" {
			continue
		}

		conditionStatus, _, _ := unstructured.NestedString(condition, "status")

		// The reason is deliberately dropped in aggregate: it is the highest
		// cardinality of the three condition labels and rolling it up is the
		// point of aggregating at all.
		into[conditionKey{condition: conditionType, status: conditionStatus}]++
	}
}
