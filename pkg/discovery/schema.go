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

// maxSchemaDepth bounds schema conversion. CRD schemas are acyclic, but a
// pathological or hand-crafted definition must not be able to drive the
// converter into unbounded recursion.
const maxSchemaDepth = 32

// ConvertSchema translates a CRD's OpenAPI v3 schema, in the untyped form the
// API server returns it, into the reduced schema the drift comparison needs.
// Returns nil for anything that is not a usable schema object.
func ConvertSchema(raw map[string]any) (converted *drift.Schema) {
	converted = convertSchemaAtDepth(raw, 0)
	return converted
}

// convertSchemaAtDepth performs the conversion, refusing to descend past
// maxSchemaDepth.
func convertSchemaAtDepth(raw map[string]any, depth int) (converted *drift.Schema) {
	if raw == nil || depth > maxSchemaDepth {
		return converted
	}

	converted = &drift.Schema{
		Type:                  stringField(raw, "type"),
		PreserveUnknownFields: boolField(raw, "x-kubernetes-preserve-unknown-fields"),
		SetSemantics:          stringField(raw, "x-kubernetes-list-type") == "set",
	}

	converted.Properties = convertProperties(raw, depth)
	converted.AdditionalProperties = convertAdditionalProperties(raw, depth)
	converted.Items = convertItems(raw, depth)

	return converted
}

// convertProperties converts the declared child schemas of an object.
func convertProperties(raw map[string]any, depth int) (properties map[string]*drift.Schema) {
	rawProperties, ok := raw["properties"].(map[string]any)
	if !ok {
		return properties
	}

	properties = make(map[string]*drift.Schema, len(rawProperties))

	for name, value := range rawProperties {
		child, childOK := value.(map[string]any)
		if !childOK {
			continue
		}

		properties[name] = convertSchemaAtDepth(child, depth+1)
	}

	return properties
}

// convertAdditionalProperties converts the schema applied to undeclared keys.
// The field is a union in OpenAPI: either a schema, or the bare boolean true
// meaning "any value is allowed", which is how free-form maps such as tags are
// most often written.
func convertAdditionalProperties(raw map[string]any, depth int) (additional *drift.Schema) {
	value, present := raw["additionalProperties"]
	if !present {
		return additional
	}

	asSchema, isSchema := value.(map[string]any)
	if isSchema {
		additional = convertSchemaAtDepth(asSchema, depth+1)
		return additional
	}

	allowed, isBool := value.(bool)
	if isBool && allowed {
		additional = &drift.Schema{PreserveUnknownFields: true}
	}

	return additional
}

// convertItems converts an array's element schema. OpenAPI allows either a
// single schema or a tuple of them; a tuple's first entry is used, since CRD
// schemas do not meaningfully use tuple validation.
func convertItems(raw map[string]any, depth int) (items *drift.Schema) {
	value, present := raw["items"]
	if !present {
		return items
	}

	asSchema, isSchema := value.(map[string]any)
	if isSchema {
		items = convertSchemaAtDepth(asSchema, depth+1)
		return items
	}

	asTuple, isTuple := value.([]any)
	if isTuple && len(asTuple) > 0 {
		first, firstOK := asTuple[0].(map[string]any)
		if firstOK {
			items = convertSchemaAtDepth(first, depth+1)
		}
	}

	return items
}

// stringField reads a string field, returning the empty string when absent or
// of another type.
func stringField(raw map[string]any, key string) (value string) {
	value, _ = raw[key].(string)
	return value
}

// boolField reads a boolean field, returning false when absent or of another
// type.
func boolField(raw map[string]any, key string) (value bool) {
	value, _ = raw[key].(bool)
	return value
}
