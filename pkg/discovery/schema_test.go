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

package discovery_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
)

func TestConvertSchemaNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, discovery.ConvertSchema(nil))
}

func TestConvertSchemaProperties(t *testing.T) {
	t.Parallel()

	converted := discovery.ConvertSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"region": map[string]any{"type": "string"},
			"nested": map[string]any{
				"type":       "object",
				"properties": map[string]any{"inner": map[string]any{"type": "integer"}},
			},
		},
	})

	require.NotNil(t, converted)
	assert.Equal(t, "object", converted.Type)
	require.Contains(t, converted.Properties, "region")
	assert.Equal(t, "string", converted.Properties["region"].Type)
	require.Contains(t, converted.Properties, "nested")
	require.Contains(t, converted.Properties["nested"].Properties, "inner")
	assert.Equal(t, "integer", converted.Properties["nested"].Properties["inner"].Type)
}

// TestConvertSchemaAdditionalProperties covers both spellings of the
// additionalProperties union, since free-form maps such as tags appear in real
// CRDs written both ways.
func TestConvertSchemaAdditionalProperties(t *testing.T) {
	t.Parallel()

	asSchema := discovery.ConvertSchema(map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string"},
	})
	require.NotNil(t, asSchema.AdditionalProperties)
	assert.Equal(t, "string", asSchema.AdditionalProperties.Type)

	asBool := discovery.ConvertSchema(map[string]any{
		"type":                 "object",
		"additionalProperties": true,
	})
	require.NotNil(t, asBool.AdditionalProperties)
	assert.True(t, asBool.AdditionalProperties.PreserveUnknownFields)

	denied := discovery.ConvertSchema(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	})
	assert.Nil(t, denied.AdditionalProperties)
}

func TestConvertSchemaItems(t *testing.T) {
	t.Parallel()

	single := discovery.ConvertSchema(map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	})
	require.NotNil(t, single.Items)
	assert.Equal(t, "string", single.Items.Type)

	tuple := discovery.ConvertSchema(map[string]any{
		"type":  "array",
		"items": []any{map[string]any{"type": "integer"}},
	})
	require.NotNil(t, tuple.Items)
	assert.Equal(t, "integer", tuple.Items.Type)

	none := discovery.ConvertSchema(map[string]any{"type": "array"})
	assert.Nil(t, none.Items)
}

func TestConvertSchemaExtensions(t *testing.T) {
	t.Parallel()

	preserve := discovery.ConvertSchema(map[string]any{
		"type":                                 "object",
		"x-kubernetes-preserve-unknown-fields": true,
	})
	assert.True(t, preserve.PreserveUnknownFields)

	set := discovery.ConvertSchema(map[string]any{
		"type":                   "array",
		"x-kubernetes-list-type": "set",
		"items":                  map[string]any{"type": "string"},
	})
	assert.True(t, set.SetSemantics)

	atomic := discovery.ConvertSchema(map[string]any{
		"type":                   "array",
		"x-kubernetes-list-type": "atomic",
		"items":                  map[string]any{"type": "string"},
	})
	assert.False(t, atomic.SetSemantics)
}

// TestConvertSchemaDepthGuard proves the converter refuses to descend forever
// into a pathologically deep definition rather than exhausting the stack.
func TestConvertSchemaDepthGuard(t *testing.T) {
	t.Parallel()

	deepest := map[string]any{"type": "string"}

	current := deepest
	for range 200 {
		current = map[string]any{
			"type":       "object",
			"properties": map[string]any{"child": current},
		}
	}

	converted := discovery.ConvertSchema(current)
	require.NotNil(t, converted)

	// Walk down until the converter stopped; it must terminate well short of
	// the 200 levels the fixture carries.
	depth := 0

	for cursor := converted; cursor != nil; depth++ {
		child, found := cursor.Properties["child"]
		if !found {
			break
		}

		cursor = child
	}

	assert.Less(t, depth, 200, "conversion must stop at the depth guard")
	assert.Positive(t, depth, "conversion must still handle reasonable nesting")
}
