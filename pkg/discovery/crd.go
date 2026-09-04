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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
)

// Kind describes one discovered Crossplane CRD: what to watch, and what can be
// computed from the objects found there.
type Kind struct {
	// GVR is the resource to inform on.
	GVR schema.GroupVersionResource

	// GVK identifies the kind for metric labels.
	GVK schema.GroupVersionKind

	// Namespaced reports whether objects of this kind live in a namespace.
	Namespaced bool

	// Managed reports whether this is a managed resource — a kind declaring
	// both spec.forProvider and status.atProvider, and therefore the only kind
	// of object drift can be computed for.
	Managed bool

	// DriftComparable reports whether the two halves actually describe any
	// field in common. A managed resource whose provider reports only computed
	// results, echoing none of the declared configuration back, has nothing to
	// compare; drift is not emitted for it rather than reporting every object
	// permanently drifted.
	DriftComparable bool

	// Categories are the CRD's declared categories, retained for diagnostics.
	Categories []string

	// ForProvider is the schema used to prune both sides of the drift
	// comparison: the declared spec.forProvider schema narrowed to the fields
	// status.atProvider also declares. It is nil unless DriftComparable.
	ForProvider *drift.Schema
}

// String renders the kind as group/version, Kind for logging.
func (k Kind) String() (rendered string) {
	rendered = k.GVK.GroupVersion().String() + ", Kind=" + k.GVK.Kind
	return rendered
}

// ParseCRD extracts a Kind from a CustomResourceDefinition. The usable result
// is false when the definition carries no served version, which is the case
// while a CRD is being installed or torn down.
func ParseCRD(object *unstructured.Unstructured) (kind Kind, usable bool) {
	group, _, _ := unstructured.NestedString(object.Object, "spec", "group")
	resourceName, _, _ := unstructured.NestedString(object.Object, "spec", "names", "plural")
	kindName, _, _ := unstructured.NestedString(object.Object, "spec", "names", "kind")
	scope, _, _ := unstructured.NestedString(object.Object, "spec", "scope")
	categories, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "names", "categories")

	if resourceName == "" || kindName == "" {
		return kind, usable
	}

	version, versionSchema, found := preferredVersion(object)
	if !found {
		return kind, usable
	}

	kind = Kind{
		GVR:        schema.GroupVersionResource{Group: group, Version: version, Resource: resourceName},
		GVK:        schema.GroupVersionKind{Group: group, Version: version, Kind: kindName},
		Namespaced: strings.EqualFold(scope, "Namespaced"),
		Categories: categories,
	}

	forProvider, hasForProvider := nestedMap(versionSchema, "properties", "spec", "properties", "forProvider")
	atProvider, hasAtProvider := nestedMap(versionSchema, "properties", "status", "properties", "atProvider")

	// A managed resource is identified structurally rather than by category.
	// The schema cannot lie about whether both halves of the comparison exist,
	// whereas a category is a label a provider author can get wrong.
	if hasForProvider && hasAtProvider {
		kind.Managed = true

		// Narrow the declared schema to the fields the provider observes back.
		// An empty intersection means this kind reports only computed results
		// and cannot be compared at all.
		kind.ForProvider = IntersectSchemas(ConvertSchema(forProvider), ConvertSchema(atProvider))
		kind.DriftComparable = kind.ForProvider != nil
	}

	usable = true

	return kind, usable
}

// Established reports whether the API server has accepted the CRD and is
// serving it. Informing on a CRD before it is established races the API server
// and produces spurious sync errors.
func Established(object *unstructured.Unstructured) (established bool) {
	conditions, found, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	if !found {
		return established
	}

	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		conditionType, _, _ := unstructured.NestedString(condition, "type")
		conditionStatus, _, _ := unstructured.NestedString(condition, "status")

		if conditionType == "Established" && conditionStatus == "True" {
			established = true
			return established
		}
	}

	return established
}

// preferredVersion picks the version to watch: the storage version when it is
// served, otherwise the first served version. Watching the storage version
// means the objects arrive without a conversion round trip.
func preferredVersion(object *unstructured.Unstructured) (version string, versionSchema map[string]any, found bool) {
	versions, exists, _ := unstructured.NestedSlice(object.Object, "spec", "versions")
	if !exists {
		return version, versionSchema, found
	}

	var fallbackVersion string

	var fallbackSchema map[string]any

	for _, entry := range versions {
		candidate, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		served, _, _ := unstructured.NestedBool(candidate, "served")
		if !served {
			continue
		}

		name, _, _ := unstructured.NestedString(candidate, "name")
		openAPISchema, _ := nestedMap(candidate, "schema", "openAPIV3Schema")

		storage, _, _ := unstructured.NestedBool(candidate, "storage")
		if storage {
			version = name
			versionSchema = openAPISchema
			found = true

			return version, versionSchema, found
		}

		if fallbackVersion == "" {
			fallbackVersion = name
			fallbackSchema = openAPISchema
		}
	}

	if fallbackVersion != "" {
		version = fallbackVersion
		versionSchema = fallbackSchema
		found = true
	}

	return version, versionSchema, found
}

// nestedMap reads a nested map, reporting whether it was present.
func nestedMap(source map[string]any, fields ...string) (value map[string]any, found bool) {
	if source == nil {
		return value, found
	}

	value, found, _ = unstructured.NestedMap(source, fields...)

	return value, found
}
