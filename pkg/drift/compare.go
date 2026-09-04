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

import (
	"fmt"
	"sort"
	"strconv"
)

// Mode values accepted by Detect. They mirror the config package's constants,
// repeated here so this package stands alone.
const (
	// ModeSubset treats spec.forProvider as a subset assertion: every declared
	// field must match what was observed, and fields left undeclared are "don't
	// care". Provider-applied defaults and provider-injected tags therefore do
	// not register as drift.
	ModeSubset = "subset"

	// ModeStrict additionally reports fields present at the provider that the
	// author never declared.
	ModeStrict = "strict"
)

// Result reports the outcome of a drift comparison.
type Result struct {
	// Drifted is true when the declared and observed states diverged.
	Drifted bool

	// Fields holds the dotted paths that differ, sorted and deduplicated. It is
	// populated only when Detect is asked to collect them.
	Fields []string
}

// Detect prunes both the declared spec.forProvider and the observed
// status.atProvider against the forProvider schema, then compares them under
// the requested mode.
//
// When collectFields is false the walk stops at the first difference, which is
// the hot path: the drift gauge alone needs one bit, and this runs for every
// managed resource on every scrape.
func Detect(forProvider map[string]any, atProvider map[string]any, schema *Schema, mode string, collectFields bool) (result Result) {
	declared := Prune(forProvider, schema)
	observed := Prune(atProvider, schema)

	fields := make([]string, 0)
	walker := &walker{mode: mode, collect: collectFields, fields: &fields}

	walker.walk("", declared, observed, schema)

	result.Drifted = walker.drifted

	if collectFields {
		result.Fields = dedupeSorted(fields)
	}

	return result
}

// walker carries the comparison state so the recursive walk does not have to
// thread five arguments through every call.
type walker struct {
	mode    string
	collect bool
	drifted bool
	fields  *[]string
}

// record marks drift at the supplied path and reports whether the walk should
// continue. When field collection is off the first difference is conclusive.
func (w *walker) record(path string) (keepGoing bool) {
	w.drifted = true

	if !w.collect {
		return keepGoing
	}

	if path != "" {
		*w.fields = append(*w.fields, path)
	}

	keepGoing = true

	return keepGoing
}

// walk compares two pruned values, descending through objects and arrays.
//
// An absent counterpart is not a leaf difference: when the declared side is a
// container and the observed side pruned away to nothing, the walk descends
// into an empty counterpart so each missing field is attributed to its own
// path rather than collapsing to the parent.
func (w *walker) walk(path string, declared any, observed any, schema *Schema) {
	if w.drifted && !w.collect {
		return
	}

	declaredMap, declaredIsMap := declared.(map[string]any)
	if declaredIsMap {
		w.walkDeclaredMap(path, declaredMap, observed, schema)
		return
	}

	declaredSlice, declaredIsSlice := declared.([]any)
	if declaredIsSlice {
		w.walkDeclaredSlice(path, declaredSlice, observed, schema)
		return
	}

	if !scalarEqual(declared, observed) {
		w.record(path)
	}
}

// walkDeclaredMap handles a declared object against whatever was observed.
func (w *walker) walkDeclaredMap(path string, declared map[string]any, observed any, schema *Schema) {
	observedMap, observedIsMap := observed.(map[string]any)
	if observedIsMap {
		w.walkMap(path, declared, observedMap, schema)
		return
	}

	// A value of a different kind entirely is a difference at this path.
	if observed != nil {
		w.record(path)
		return
	}

	w.walkMap(path, declared, map[string]any{}, schema)
}

// walkDeclaredSlice handles a declared array against whatever was observed.
func (w *walker) walkDeclaredSlice(path string, declared []any, observed any, schema *Schema) {
	observedSlice, observedIsSlice := observed.([]any)
	if observedIsSlice {
		w.walkSlice(path, declared, observedSlice, schema)
		return
	}

	if observed != nil {
		w.record(path)
		return
	}

	w.walkSlice(path, declared, nil, schema)
}

// walkMap compares every declared key against the observed object. In strict
// mode it also reports observed keys the author never declared.
func (w *walker) walkMap(path string, declared map[string]any, observed map[string]any, schema *Schema) {
	for key, declaredValue := range declared {
		if w.drifted && !w.collect {
			return
		}

		childSchema, _ := schema.Property(key)
		childPath := joinPath(path, key)

		observedValue, present := observed[key]
		if !present {
			w.record(childPath)
			continue
		}

		w.walk(childPath, declaredValue, observedValue, childSchema)
	}

	if w.mode != ModeStrict {
		return
	}

	for key := range observed {
		_, declaredHasKey := declared[key]
		if declaredHasKey {
			continue
		}

		keepGoing := w.record(joinPath(path, key))
		if !keepGoing {
			return
		}
	}
}

// walkSlice compares arrays element-wise. Arrays the schema marks as sets are
// canonically ordered first, so a reordering is not reported as drift.
func (w *walker) walkSlice(path string, declared []any, observed []any, schema *Schema) {
	var itemSchema *Schema
	if schema != nil {
		itemSchema = schema.Items
	}

	if schema != nil && schema.SetSemantics {
		declared = canonicalOrder(declared)
		observed = canonicalOrder(observed)
	}

	if len(declared) != len(observed) {
		w.record(path)
		return
	}

	for index, declaredValue := range declared {
		if w.drifted && !w.collect {
			return
		}

		w.walk(path+"["+strconv.Itoa(index)+"]", declaredValue, observed[index], itemSchema)
	}
}

// scalarEqual compares two leaf values. JSON decoding leaves numbers as either
// int64 or float64 depending on the path taken, so numbers are compared after
// widening rather than by Go type identity.
func scalarEqual(left any, right any) (equal bool) {
	leftNumber, leftIsNumber := toFloat(left)
	rightNumber, rightIsNumber := toFloat(right)

	if leftIsNumber && rightIsNumber {
		equal = leftNumber == rightNumber
		return equal
	}

	equal = left == right

	return equal
}

// toFloat widens any JSON numeric representation to float64.
func toFloat(value any) (number float64, isNumber bool) {
	switch typed := value.(type) {
	case int64:
		number = float64(typed)
		isNumber = true
	case int32:
		number = float64(typed)
		isNumber = true
	case int:
		number = float64(typed)
		isNumber = true
	case float64:
		number = typed
		isNumber = true
	case float32:
		number = float64(typed)
		isNumber = true
	default:
		isNumber = false
	}

	return number, isNumber
}

// canonicalOrder returns a copy of the slice ordered by each element's rendered
// form. fmt renders maps with sorted keys, so the ordering is deterministic.
func canonicalOrder(values []any) (ordered []any) {
	ordered = make([]any, len(values))
	copy(ordered, values)

	sort.SliceStable(ordered, func(i int, j int) (less bool) {
		less = fmt.Sprintf("%v", ordered[i]) < fmt.Sprintf("%v", ordered[j])
		return less
	})

	return ordered
}

// joinPath appends a key to a dotted field path.
func joinPath(prefix string, key string) (path string) {
	if prefix == "" {
		path = key
		return path
	}

	path = prefix + "." + key

	return path
}

// dedupeSorted sorts the paths and removes duplicates.
func dedupeSorted(fields []string) (unique []string) {
	if len(fields) == 0 {
		return unique
	}

	sort.Strings(fields)

	unique = make([]string, 0, len(fields))

	for index, field := range fields {
		if index > 0 && field == fields[index-1] {
			continue
		}

		unique = append(unique, field)
	}

	return unique
}
