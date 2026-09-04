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

// Package drift compares a managed resource's declared spec.forProvider
// against its observed status.atProvider and reports whether the two have
// diverged.
//
// The comparison is deliberately structural, not a provider "terraform plan":
// it reads only what the Kubernetes API already holds and makes no cloud API
// calls. That catches the common cases — someone changed the cloud out of
// band, or git and reality diverged — but it will not catch a difference that
// exists only in representation and that the provider would normalize away.
//
// The package takes plain Go maps and a reduced schema type, so it has no
// dependency on the Kubernetes libraries and can be exercised directly in
// tests.
package drift

// Schema is the subset of an OpenAPI v3 schema the comparison needs. The
// caller builds it from a CRD's spec.forProvider schema; see the discovery
// package.
type Schema struct {
	// Type is the OpenAPI type ("object", "array", "string", ...). It may be
	// empty for schemas that only constrain via composition.
	Type string

	// Properties holds the declared child schemas of an object.
	Properties map[string]*Schema

	// AdditionalProperties is the schema applied to undeclared keys of a map-
	// valued object, as used by free-form fields such as tags.
	AdditionalProperties *Schema

	// Items is the element schema of an array.
	Items *Schema

	// PreserveUnknownFields marks a subtree whose contents are not described by
	// the schema. Such a subtree is compared verbatim, because there is no
	// declaration to prune against.
	PreserveUnknownFields bool

	// SetSemantics marks an array whose element order carries no meaning
	// (x-kubernetes-list-type: set). Such arrays are canonically ordered before
	// comparison so a reordering is not reported as drift.
	SetSemantics bool
}

// Property returns the schema governing the named child of an object schema,
// falling back to the additionalProperties schema for undeclared keys. The
// found result is false when the key is not described by the schema at all, in
// which case the value is provider-computed and must be pruned away.
func (s *Schema) Property(name string) (schema *Schema, found bool) {
	if s == nil {
		return schema, found
	}

	schema, found = s.Properties[name]
	if found && schema != nil {
		return schema, found
	}

	if s.AdditionalProperties != nil {
		schema = s.AdditionalProperties
		found = true

		return schema, found
	}

	schema = nil
	found = false

	return schema, found
}
