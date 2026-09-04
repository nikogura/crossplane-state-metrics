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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// emitDrift compares every managed resource of one kind and emits the drift
// gauge, returning how long the comparison took so the caller can record the
// whole scrape's drift cost as one observation.
//
// Only managed resources are compared, and only those whose provider echoes
// some of the declared configuration back. A composite resource has no
// status.atProvider to compare against; answering "does this composite still
// match what its Composition would render?" means re-running the composition
// function pipeline, which is a different program from this one. A managed
// resource whose atProvider shares no field with its forProvider has nothing
// to compare either, and is skipped rather than reported permanently drifted.
func (c *Collector) emitDrift(ctx context.Context, ch chan<- prometheus.Metric, snapshot watch.Snapshot, allowance *budget) (elapsed float64) {
	if !c.options.Drift || !snapshot.Kind.Managed || !snapshot.Kind.DriftComparable {
		return elapsed
	}

	started := time.Now()

	for _, object := range snapshot.Objects {
		result, ok := c.driftFor(ctx, snapshot, object)
		if !ok {
			continue
		}

		value := float64(0)
		if result.Drifted {
			value = 1
		}

		if !allowance.take(1) {
			break
		}

		identity := identityValues(snapshot, object)

		ch <- prometheus.MustNewConstMetric(c.drift, prometheus.GaugeValue, value, identity...)

		c.emitDriftFields(ch, identity, result, allowance)
	}

	elapsed = time.Since(started).Seconds()

	return elapsed
}

// emitDriftFields emits the opt-in per-field detail, capped so one badly
// drifted resource cannot blow up the series count on its own.
func (c *Collector) emitDriftFields(ch chan<- prometheus.Metric, identity []string, result drift.Result, allowance *budget) {
	if !c.options.DriftFields || !result.Drifted {
		return
	}

	for index, field := range result.Fields {
		if index >= c.options.DriftFieldsMax {
			return
		}

		if !allowance.take(1) {
			return
		}

		labels := append(append([]string{}, identity...), field)

		ch <- prometheus.MustNewConstMetric(c.driftField, prometheus.GaugeValue, 1, labels...)
	}
}

// driftFor returns the drift result for one object, computing it only when the
// object has changed since the cached result was taken.
func (c *Collector) driftFor(ctx context.Context, snapshot watch.Snapshot, object *unstructured.Unstructured) (result drift.Result, ok bool) {
	uid := object.GetUID()
	resourceVersion := object.GetResourceVersion()

	cached, hit := c.lookupDrift(uid)
	if hit && cached.resourceVersion == resourceVersion {
		result = cached.result
		ok = true

		return result, ok
	}

	forProvider, _, forErr := unstructured.NestedMap(object.Object, "spec", "forProvider")
	if forErr != nil {
		metrics.RecordDriftError(ctx)
		return result, ok
	}

	atProvider, _, atErr := unstructured.NestedMap(object.Object, "status", "atProvider")
	if atErr != nil {
		metrics.RecordDriftError(ctx)
		return result, ok
	}

	result = drift.Detect(forProvider, atProvider, snapshot.Kind.ForProvider, c.options.DriftMode, c.options.DriftFields)
	ok = true

	c.storeDrift(uid, cachedDrift{resourceVersion: resourceVersion, result: result})

	return result, ok
}

// lookupDrift reads the memoised result for an object.
func (c *Collector) lookupDrift(uid types.UID) (cached cachedDrift, found bool) {
	c.driftMutex.Lock()
	defer c.driftMutex.Unlock()

	cached, found = c.driftCache[uid]

	return cached, found
}

// storeDrift memoises a freshly computed result.
func (c *Collector) storeDrift(uid types.UID, entry cachedDrift) {
	c.driftMutex.Lock()
	defer c.driftMutex.Unlock()

	c.driftCache[uid] = entry
}

// pruneDriftCache drops memoised results for objects that no longer exist, so
// the cache stays bounded by the size of the cluster rather than growing with
// every resource the cluster has ever held.
func (c *Collector) pruneDriftCache(live map[types.UID]struct{}) {
	c.driftMutex.Lock()
	defer c.driftMutex.Unlock()

	for uid := range c.driftCache {
		_, alive := live[uid]
		if !alive {
			delete(c.driftCache, uid)
		}
	}
}
