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

package drift_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
)

// objectSchema is a small helper for building object schemas in tests.
func objectSchema(properties map[string]*drift.Schema) (schema *drift.Schema) {
	schema = &drift.Schema{Type: "object", Properties: properties}
	return schema
}

// scalarSchema builds a leaf schema of the supplied type.
func scalarSchema(kind string) (schema *drift.Schema) {
	schema = &drift.Schema{Type: kind}
	return schema
}

// bucketSchema mirrors the shape of a realistic managed-resource forProvider
// schema: scalars, a free-form tag map, and an array of objects.
func bucketSchema() (schema *drift.Schema) {
	schema = objectSchema(map[string]*drift.Schema{
		"region":        scalarSchema("string"),
		"objectLock":    scalarSchema("boolean"),
		"retentionDays": scalarSchema("integer"),
		"tags": {
			Type:                 "object",
			AdditionalProperties: scalarSchema("string"),
		},
		"replication": objectSchema(map[string]*drift.Schema{
			"role": scalarSchema("string"),
		}),
		"rules": {
			Type: "array",
			Items: objectSchema(map[string]*drift.Schema{
				"id":     scalarSchema("string"),
				"status": scalarSchema("string"),
			}),
		},
	})

	return schema
}

func TestIsEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		value    any
		expected bool
	}{
		{name: "nil is empty", value: nil, expected: true},
		{name: "empty string is empty", value: "", expected: true},
		{name: "empty map is empty", value: map[string]any{}, expected: true},
		{name: "empty slice is empty", value: []any{}, expected: true},
		{name: "false is not empty", value: false, expected: false},
		{name: "zero is not empty", value: int64(0), expected: false},
		{name: "zero float is not empty", value: 0.0, expected: false},
		{name: "populated string is not empty", value: "x", expected: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual := drift.IsEmpty(testCase.value)
			assert.Equal(t, testCase.expected, actual)
		})
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		value    any
		schema   *drift.Schema
		expected any
	}{
		{
			name:     "nil schema prunes everything",
			value:    map[string]any{"region": "us-east-1"},
			schema:   nil,
			expected: nil,
		},
		{
			name: "undeclared provider-computed fields are dropped",
			value: map[string]any{
				"region": "us-east-1",
				"arn":    "arn:aws:s3:::bucket",
				"id":     "bucket-1234",
			},
			schema:   bucketSchema(),
			expected: map[string]any{"region": "us-east-1"},
		},
		{
			name: "nested undeclared fields are dropped at depth",
			value: map[string]any{
				"replication": map[string]any{
					"role":       "arn:aws:iam::1:role/r",
					"lastStatus": "Enabled",
				},
			},
			schema: bucketSchema(),
			expected: map[string]any{
				"replication": map[string]any{"role": "arn:aws:iam::1:role/r"},
			},
		},
		{
			name: "free-form maps survive via additionalProperties",
			value: map[string]any{
				"tags": map[string]any{"Environment": "prod", "Owner": "sre"},
			},
			schema: bucketSchema(),
			expected: map[string]any{
				"tags": map[string]any{"Environment": "prod", "Owner": "sre"},
			},
		},
		{
			name: "arrays of objects are pruned element-wise",
			value: map[string]any{
				"rules": []any{
					map[string]any{"id": "r1", "status": "Enabled", "createdAt": "2026-01-01"},
				},
			},
			schema: bucketSchema(),
			expected: map[string]any{
				"rules": []any{map[string]any{"id": "r1", "status": "Enabled"}},
			},
		},
		{
			name:     "empty values are dropped as unset",
			value:    map[string]any{"region": "", "tags": map[string]any{}},
			schema:   bucketSchema(),
			expected: nil,
		},
		{
			name:  "preserve-unknown-fields subtrees are kept verbatim",
			value: map[string]any{"blob": map[string]any{"anything": "goes"}},
			schema: objectSchema(map[string]*drift.Schema{
				"blob": {Type: "object", PreserveUnknownFields: true},
			}),
			expected: map[string]any{"blob": map[string]any{"anything": "goes"}},
		},
		{
			name:     "false is retained, not treated as unset",
			value:    map[string]any{"objectLock": false},
			schema:   bucketSchema(),
			expected: map[string]any{"objectLock": false},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual := drift.Prune(testCase.value, testCase.schema)
			assert.Equal(t, testCase.expected, actual)
		})
	}
}

func TestDetectNoDrift(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		forProvider map[string]any
		atProvider  map[string]any
	}{
		{
			name:        "identical declared and observed",
			forProvider: map[string]any{"region": "us-east-1"},
			atProvider:  map[string]any{"region": "us-east-1"},
		},
		{
			// The dominant false-positive class: status.atProvider is full of
			// ARNs, IDs and timestamps the author never declared.
			name:        "provider-computed fields do not count as drift",
			forProvider: map[string]any{"region": "us-east-1"},
			atProvider: map[string]any{
				"region":    "us-east-1",
				"arn":       "arn:aws:s3:::bucket",
				"id":        "bucket-1234",
				"createdAt": "2026-01-01T00:00:00Z",
			},
		},
		{
			// Second false-positive class: the cloud adds its own tags.
			name: "provider-injected tags do not count as drift in subset mode",
			forProvider: map[string]any{
				"tags": map[string]any{"Environment": "prod"},
			},
			atProvider: map[string]any{
				"tags": map[string]any{"Environment": "prod", "aws:createdBy": "someone"},
			},
		},
		{
			// Third false-positive class: provider defaults for fields the
			// author deliberately left unset.
			name:        "provider defaults on undeclared fields are not drift",
			forProvider: map[string]any{"region": "us-east-1"},
			atProvider:  map[string]any{"region": "us-east-1", "objectLock": false},
		},
		{
			name:        "int64 and float64 representations of a number are equal",
			forProvider: map[string]any{"retentionDays": int64(30)},
			atProvider:  map[string]any{"retentionDays": 30.0},
		},
		{
			name:        "declared false matches observed false",
			forProvider: map[string]any{"objectLock": false},
			atProvider:  map[string]any{"objectLock": false},
		},
		{
			name: "equal arrays of objects",
			forProvider: map[string]any{
				"rules": []any{map[string]any{"id": "r1", "status": "Enabled"}},
			},
			atProvider: map[string]any{
				"rules": []any{map[string]any{"id": "r1", "status": "Enabled", "createdAt": "x"}},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := drift.Detect(testCase.forProvider, testCase.atProvider, bucketSchema(), drift.ModeSubset, true)
			assert.False(t, result.Drifted, "expected no drift, got fields %v", result.Fields)
			assert.Empty(t, result.Fields)
		})
	}
}

func TestDetectDrift(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		forProvider    map[string]any
		atProvider     map[string]any
		expectedFields []string
	}{
		{
			name:           "changed scalar",
			forProvider:    map[string]any{"region": "us-east-1"},
			atProvider:     map[string]any{"region": "us-west-2"},
			expectedFields: []string{"region"},
		},
		{
			name:           "declared field missing at provider",
			forProvider:    map[string]any{"region": "us-east-1"},
			atProvider:     map[string]any{},
			expectedFields: []string{"region"},
		},
		{
			name: "changed tag value",
			forProvider: map[string]any{
				"tags": map[string]any{"Environment": "prod"},
			},
			atProvider: map[string]any{
				"tags": map[string]any{"Environment": "staging"},
			},
			expectedFields: []string{"tags.Environment"},
		},
		{
			name: "nested object field changed",
			forProvider: map[string]any{
				"replication": map[string]any{"role": "arn:aws:iam::1:role/a"},
			},
			atProvider: map[string]any{
				"replication": map[string]any{"role": "arn:aws:iam::1:role/b"},
			},
			expectedFields: []string{"replication.role"},
		},
		{
			name: "array element field changed reports an indexed path",
			forProvider: map[string]any{
				"rules": []any{map[string]any{"id": "r1", "status": "Enabled"}},
			},
			atProvider: map[string]any{
				"rules": []any{map[string]any{"id": "r1", "status": "Disabled"}},
			},
			expectedFields: []string{"rules[0].status"},
		},
		{
			name: "array length change reports the array path",
			forProvider: map[string]any{
				"rules": []any{
					map[string]any{"id": "r1", "status": "Enabled"},
					map[string]any{"id": "r2", "status": "Enabled"},
				},
			},
			atProvider: map[string]any{
				"rules": []any{map[string]any{"id": "r1", "status": "Enabled"}},
			},
			expectedFields: []string{"rules"},
		},
		{
			name: "several fields drift at once and are reported sorted",
			forProvider: map[string]any{
				"region": "us-east-1",
				"tags":   map[string]any{"Environment": "prod"},
			},
			atProvider: map[string]any{
				"region": "us-west-2",
				"tags":   map[string]any{"Environment": "staging"},
			},
			expectedFields: []string{"region", "tags.Environment"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := drift.Detect(testCase.forProvider, testCase.atProvider, bucketSchema(), drift.ModeSubset, true)
			require.True(t, result.Drifted)
			assert.Equal(t, testCase.expectedFields, result.Fields)
		})
	}
}

func TestDetectStrictMode(t *testing.T) {
	t.Parallel()

	forProvider := map[string]any{"tags": map[string]any{"Environment": "prod"}}
	atProvider := map[string]any{"tags": map[string]any{"Environment": "prod", "aws:createdBy": "someone"}}

	subset := drift.Detect(forProvider, atProvider, bucketSchema(), drift.ModeSubset, true)
	assert.False(t, subset.Drifted, "subset mode must ignore undeclared observed fields")

	strict := drift.Detect(forProvider, atProvider, bucketSchema(), drift.ModeStrict, true)
	require.True(t, strict.Drifted, "strict mode must report undeclared observed fields")
	assert.Equal(t, []string{"tags.aws:createdBy"}, strict.Fields)
}

func TestDetectSetSemantics(t *testing.T) {
	t.Parallel()

	schema := objectSchema(map[string]*drift.Schema{
		"zones": {
			Type:         "array",
			SetSemantics: true,
			Items:        scalarSchema("string"),
		},
		"ordered": {
			Type:  "array",
			Items: scalarSchema("string"),
		},
	})

	reordered := drift.Detect(
		map[string]any{"zones": []any{"a", "b", "c"}},
		map[string]any{"zones": []any{"c", "a", "b"}},
		schema, drift.ModeSubset, true)
	assert.False(t, reordered.Drifted, "set-typed arrays must be order-insensitive")

	ordered := drift.Detect(
		map[string]any{"ordered": []any{"a", "b"}},
		map[string]any{"ordered": []any{"b", "a"}},
		schema, drift.ModeSubset, true)
	assert.True(t, ordered.Drifted, "arrays without set semantics must stay order-sensitive")
}

// TestDetectShortCircuit pins the hot-path behaviour: with field collection
// off the walk still reports drift correctly, it just stops early.
func TestDetectShortCircuit(t *testing.T) {
	t.Parallel()

	forProvider := map[string]any{"region": "us-east-1", "objectLock": true}
	atProvider := map[string]any{"region": "us-west-2", "objectLock": false}

	result := drift.Detect(forProvider, atProvider, bucketSchema(), drift.ModeSubset, false)
	assert.True(t, result.Drifted)
	assert.Empty(t, result.Fields, "field collection is off, so no paths are gathered")
}

func TestDetectEmptyInputs(t *testing.T) {
	t.Parallel()

	result := drift.Detect(nil, nil, bucketSchema(), drift.ModeSubset, true)
	assert.False(t, result.Drifted)

	// An observe-only resource that has not yet been observed has an empty
	// atProvider; every declared field is therefore missing.
	unobserved := drift.Detect(map[string]any{"region": "us-east-1"}, nil, bucketSchema(), drift.ModeSubset, true)
	assert.True(t, unobserved.Drifted)
	assert.Equal(t, []string{"region"}, unobserved.Fields)
}
