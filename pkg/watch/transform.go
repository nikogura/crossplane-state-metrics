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

package watch

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
)

// Object fields the exporter reads. Everything else is dropped from the cache.
const (
	fieldSpec     = "spec"
	fieldStatus   = "status"
	fieldMetadata = "metadata"
)

// trimTransform builds a cache.TransformFunc that strips the parts of an object
// the exporter never reads, before it is stored.
//
// This bounds memory rather than cardinality, and the two are independent: an
// informer cache holds every watched object in full, so RAM scales with fleet
// size even when the emitted series count is modest. Two things dominate:
//
//   - metadata.managedFields. Server-side apply records an entry per field per
//     manager, and on a Crossplane managed resource it is routinely larger than
//     the rest of the object put together. Nothing here ever reads it.
//   - spec. Only drift-comparable managed resources need spec.forProvider, and
//     only ManagedResourceDefinition and CompositionRevision need their own
//     small spec fields. For every other kind — composites, packages, XRDs,
//     Compositions, Operations — the entire spec is dead weight.
//
// status is always kept: conditions live there, and they are the one signal
// every Crossplane object carries.
func trimTransform(kind discovery.Kind) (transform cache.TransformFunc) {
	keepSpec := kind.DriftComparable || needsSpec(kind)

	transform = func(input any) (output any, err error) {
		object, ok := input.(*unstructured.Unstructured)
		if !ok {
			// Tombstones and anything unexpected pass through untouched.
			output = input
			return output, err
		}

		unstructured.RemoveNestedField(object.Object, fieldMetadata, "managedFields")

		if !keepSpec {
			delete(object.Object, fieldSpec)
		}

		if !kind.DriftComparable {
			// atProvider is the observed half of the drift comparison and is
			// large. Without a comparable schema it is never read.
			unstructured.RemoveNestedField(object.Object, fieldStatus, "atProvider")
		}

		output = object

		return output, err
	}

	return transform
}

// needsSpec reports whether a non-managed kind still has spec fields the
// collector reads: the Crossplane v2 activation state, and a composition
// revision's number.
func needsSpec(kind discovery.Kind) (needed bool) {
	if kind.GVK.Group != "apiextensions.crossplane.io" {
		return needed
	}

	switch kind.GVK.Kind {
	case "ManagedResourceDefinition", "CompositionRevision":
		needed = true
	default:
		needed = false
	}

	return needed
}
