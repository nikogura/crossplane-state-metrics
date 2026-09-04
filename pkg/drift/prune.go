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

package drift

// Prune recursively reduces a value to the fields the supplied schema
// declares, dropping everything else, and then drops values that carry no
// information (nil, empty string, empty object, empty array).
//
// Pruning both sides of the comparison against the *forProvider* schema is
// what makes the comparison usable: status.atProvider is full of
// provider-computed fields — ARNs, IDs, timestamps, nested status blocks —
// that the author never declared and never could. Left in, every resource
// would report permanent drift.
func Prune(value any, schema *Schema) (pruned any) {
	if schema == nil {
		return pruned
	}

	// A subtree the schema does not describe cannot be pruned; compare it whole.
	if schema.PreserveUnknownFields {
		pruned = value
		return pruned
	}

	switch typed := value.(type) {
	case map[string]any:
		pruned = pruneMap(typed, schema)
	case []any:
		pruned = pruneSlice(typed, schema)
	default:
		pruned = value
	}

	return pruned
}

// pruneMap keeps only the keys the object schema declares, recursing into each.
func pruneMap(value map[string]any, schema *Schema) (pruned any) {
	out := make(map[string]any, len(value))

	for key, child := range value {
		childSchema, found := schema.Property(key)
		if !found {
			continue
		}

		childPruned := Prune(child, childSchema)
		if IsEmpty(childPruned) {
			continue
		}

		out[key] = childPruned
	}

	if len(out) == 0 {
		return pruned
	}

	pruned = out

	return pruned
}

// pruneSlice recurses into each element using the array's item schema. An
// array whose items the schema does not describe is dropped entirely.
func pruneSlice(value []any, schema *Schema) (pruned any) {
	if schema.Items == nil {
		return pruned
	}

	out := make([]any, 0, len(value))

	for _, element := range value {
		elementPruned := Prune(element, schema.Items)
		if IsEmpty(elementPruned) {
			continue
		}

		out = append(out, elementPruned)
	}

	if len(out) == 0 {
		return pruned
	}

	pruned = out

	return pruned
}

// IsEmpty reports whether a value carries no information and should be treated
// as unset. Note that false and 0 are meaningful values, not empty ones — only
// nil, the empty string, and empty containers are empty.
func IsEmpty(value any) (empty bool) {
	switch typed := value.(type) {
	case nil:
		empty = true
	case string:
		empty = typed == ""
	case map[string]any:
		empty = len(typed) == 0
	case []any:
		empty = len(typed) == 0
	default:
		empty = false
	}

	return empty
}
