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
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// fieldMetricsByKind indexes the configured field metrics by the kind they
// apply to, so the per-object loop does one map lookup per kind rather than a
// scan per object.
func fieldMetricsByKind(metrics []config.FieldMetric) (index map[schema.GroupKind][]config.FieldMetric) {
	index = make(map[schema.GroupKind][]config.FieldMetric, len(metrics))

	for _, metric := range metrics {
		key := schema.GroupKind{Group: metric.Group, Kind: metric.Kind}
		index[key] = append(index[key], metric)
	}

	return index
}

// emitFields emits the configured numeric fields of one object.
//
// A field that is absent, or holds anything but a number, produces no series.
// The alternative — emitting 0, or NaN — would be read as a value, and a
// missing series is the honest report of a value that is not there.
func (c *Collector) emitFields(ch chan<- prometheus.Metric, snapshot watch.Snapshot, object *unstructured.Unstructured, allowance *budget) {
	fields := c.fieldMetrics[snapshot.Kind.GVK.GroupKind()]
	if len(fields) == 0 {
		return
	}

	var identity []string

	for _, field := range fields {
		raw, found, lookupErr := unstructured.NestedFieldNoCopy(object.Object, field.Path...)
		if lookupErr != nil || !found {
			continue
		}

		value, isNumber := drift.Number(raw)
		if !isNumber {
			continue
		}

		if !allowance.take(1) {
			return
		}

		if identity == nil {
			identity = c.objectValues(snapshot, object)
		}

		labels := append(append([]string{}, identity...), field.PathString())

		ch <- prometheus.MustNewConstMetric(c.resourceField, prometheus.GaugeValue, value, labels...)
	}
}
