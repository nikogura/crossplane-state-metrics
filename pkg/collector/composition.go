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

	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// Coordinates of the Composition revision kind.
const (
	apiExtensionsGroup      = "apiextensions.crossplane.io"
	compositionRevisionKind = "CompositionRevision"
	compositionKind         = "Composition"

	// compositionNameLabel is the label Crossplane stamps on a revision naming
	// the Composition it came from. It is the fallback for the owner reference.
	compositionNameLabel = "crossplane.io/composition-name"
)

// revisionSummary aggregates the revisions belonging to one Composition.
type revisionSummary struct {
	count   int
	highest int64
}

// isCompositionRevision reports whether a snapshot holds CompositionRevisions.
func isCompositionRevision(snapshot watch.Snapshot) (matches bool) {
	matches = snapshot.Kind.GVK.Group == apiExtensionsGroup &&
		snapshot.Kind.GVK.Kind == compositionRevisionKind

	return matches
}

// emitCompositions emits revision counts per Composition.
//
// A Composition is one of the few Crossplane kinds with no status conditions
// at all — it is a pure spec object, validated when a composite renders rather
// than by a controller writing back a condition. The condition loop therefore
// says nothing about it, and revision churn is the signal it does have: a
// count that climbs steadily is a Composition being edited under running
// composites.
func (c *Collector) emitCompositions(ch chan<- prometheus.Metric, revisions []*unstructured.Unstructured, allowance *budget) {
	if len(revisions) == 0 {
		return
	}

	summaries := make(map[string]revisionSummary, len(revisions))

	for _, revision := range revisions {
		name := compositionNameOf(revision)
		if name == "" {
			continue
		}

		summary := summaries[name]
		summary.count++

		number, found, numErr := unstructured.NestedInt64(revision.Object, "spec", "revision")
		if numErr == nil && found && number > summary.highest {
			summary.highest = number
		}

		summaries[name] = summary
	}

	for name, summary := range summaries {
		if !allowance.take(2) {
			return
		}

		ch <- prometheus.MustNewConstMetric(c.compositionCount, prometheus.GaugeValue, float64(summary.count), name)
		ch <- prometheus.MustNewConstMetric(c.compositionActive, prometheus.GaugeValue, float64(summary.highest), name)
	}
}

// compositionNameOf resolves the Composition a revision belongs to. The owner
// reference is authoritative — Crossplane sets it so revisions are garbage
// collected with their Composition — and the label is the fallback for any
// revision whose owner reference has not been written yet.
func compositionNameOf(revision *unstructured.Unstructured) (name string) {
	for _, owner := range revision.GetOwnerReferences() {
		if owner.Kind == compositionKind {
			name = owner.Name
			return name
		}
	}

	name = revision.GetLabels()[compositionNameLabel]

	return name
}
