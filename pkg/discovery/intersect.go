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

package discovery

import (
	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
)

// IntersectSchemas reduces the declared spec.forProvider schema to the fields
// that status.atProvider also declares, recursing through nested objects and
// array elements. It returns nil when the two schemas describe no field in
// common.
//
// This is what makes drift meaningful rather than merely computable. Providers
// differ in how much of the declared configuration they observe back:
//
//   - Upjet-generated resources derive both halves from the same underlying
//     schema, so atProvider mirrors forProvider almost field for field and the
//     intersection is nearly the whole schema.
//   - Other providers report only computed results. provider-talos's
//     Configuration declares clusterName, node and machineType under
//     forProvider while atProvider carries only generatedTime,
//     machineConfiguration and machineConfigurationHash.
//
// Comparing a declared field against an observed side that has no schema slot
// for it is comparing against something that structurally cannot be there. It
// is absence of an echo, not evidence of divergence, and reporting it as drift
// would mark every object of such a kind permanently drifted. Kinds whose
// intersection is empty are therefore not drift-comparable at all.
func IntersectSchemas(forProvider *drift.Schema, atProvider *drift.Schema) (intersection *drift.Schema) {
	intersection = intersectAtDepth(forProvider, atProvider, 0)
	return intersection
}

// intersectAtDepth performs the intersection, bounded by the same depth guard
// the converter uses.
func intersectAtDepth(forProvider *drift.Schema, atProvider *drift.Schema, depth int) (intersection *drift.Schema) {
	if forProvider == nil || atProvider == nil || depth > maxSchemaDepth {
		return intersection
	}

	// A subtree the observed side does not describe is compared verbatim; there
	// is no declaration to intersect against.
	if atProvider.PreserveUnknownFields || forProvider.PreserveUnknownFields {
		intersection = forProvider
		return intersection
	}

	// Two scalars have no children to narrow and are directly comparable. This
	// also covers the value schema of a free-form map and the element schema of
	// a scalar array, which would otherwise intersect away to nothing.
	if isLeaf(forProvider) || isLeaf(atProvider) {
		intersection = forProvider
		return intersection
	}

	result := &drift.Schema{
		Type:         forProvider.Type,
		SetSemantics: forProvider.SetSemantics,
	}

	result.Properties = intersectProperties(forProvider, atProvider, depth)
	result.AdditionalProperties = intersectAtDepth(forProvider.AdditionalProperties, atProvider.AdditionalProperties, depth+1)
	result.Items = intersectAtDepth(forProvider.Items, atProvider.Items, depth+1)

	if isEmptySchema(result) {
		return intersection
	}

	intersection = result

	return intersection
}

// intersectProperties keeps the object properties both sides declare.
func intersectProperties(forProvider *drift.Schema, atProvider *drift.Schema, depth int) (properties map[string]*drift.Schema) {
	for name, declared := range forProvider.Properties {
		observed, found := atProvider.Properties[name]
		if !found {
			observed = atProvider.AdditionalProperties
		}

		if observed == nil {
			continue
		}

		// A leaf on either side intersects to the declared leaf: there are no
		// child properties to narrow, and the values are directly comparable.
		if isLeaf(declared) || isLeaf(observed) {
			if properties == nil {
				properties = map[string]*drift.Schema{}
			}

			properties[name] = declared

			continue
		}

		child := intersectAtDepth(declared, observed, depth+1)
		if child == nil {
			continue
		}

		if properties == nil {
			properties = map[string]*drift.Schema{}
		}

		properties[name] = child
	}

	return properties
}

// isLeaf reports whether a schema describes a scalar rather than a container.
func isLeaf(schema *drift.Schema) (leaf bool) {
	if schema == nil {
		return leaf
	}

	leaf = len(schema.Properties) == 0 && schema.Items == nil && schema.AdditionalProperties == nil

	return leaf
}

// isEmptySchema reports whether an intersection produced nothing comparable.
func isEmptySchema(schema *drift.Schema) (empty bool) {
	empty = len(schema.Properties) == 0 &&
		schema.Items == nil &&
		schema.AdditionalProperties == nil

	return empty
}
