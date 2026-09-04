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

// Coordinates and field values of the Crossplane v2 activation model.
const (
	managedResourceDefinitionKind = "ManagedResourceDefinition"

	// stateActive is the spec.state value that causes Crossplane to create the
	// underlying CRD. The field defaults to Inactive.
	stateActive = "Active"

	// LabelDefinesGroup and LabelDefinesKind describe the managed resource a
	// definition governs, as opposed to the definition object itself.
	LabelDefinesGroup = "defines_group"
	LabelDefinesKind  = "defines_kind"
)

// isManagedResourceDefinition reports whether a snapshot holds
// ManagedResourceDefinitions.
func isManagedResourceDefinition(snapshot watch.Snapshot) (matches bool) {
	matches = snapshot.Kind.GVK.Group == apiExtensionsGroup &&
		snapshot.Kind.GVK.Kind == managedResourceDefinitionKind

	return matches
}

// emitActivation reports whether each Crossplane v2 ManagedResourceDefinition
// is active.
//
// In v2 a provider no longer installs every CRD it knows how to manage.
// Definitions arrive Inactive by default, and spec.state toggles whether the
// underlying CRD is created at all. An Inactive definition therefore means the
// kind does not exist in the cluster: no CRD, nothing to watch, no metrics.
//
// That is invisible in every other signal here. Discovery reports what exists,
// so a kind that was never activated simply never appears, and an operator
// asking why a managed resource has no data gets silence rather than the
// answer. spec.state is a spec field, not a condition, so the universal
// condition loop cannot surface it either — hence a purpose-built metric, for
// the same reason Compositions get a revision metric.
func (c *Collector) emitActivation(ch chan<- prometheus.Metric, definitions []*unstructured.Unstructured, allowance *budget) {
	for _, definition := range definitions {
		state, found, stateErr := unstructured.NestedString(definition.Object, "spec", "state")
		if stateErr != nil {
			continue
		}

		// The field defaults to Inactive, so an absent value is not unknown.
		if !found {
			state = ""
		}

		value := float64(0)
		if state == stateActive {
			value = 1
		}

		if !allowance.take(1) {
			return
		}

		group, _, _ := unstructured.NestedString(definition.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(definition.Object, "spec", "names", "kind")

		ch <- prometheus.MustNewConstMetric(c.definitionActive, prometheus.GaugeValue, value,
			definition.GetName(), group, kind)
	}
}
