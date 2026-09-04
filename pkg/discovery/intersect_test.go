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
	"github.com/nikogura/crossplane-state-metrics/pkg/drift"
)

// obj builds an object schema from child schemas.
func obj(props map[string]*drift.Schema) (schema *drift.Schema) {
	schema = &drift.Schema{Type: "object", Properties: props}
	return schema
}

// leaf builds a scalar schema.
func leaf(kind string) (schema *drift.Schema) {
	schema = &drift.Schema{Type: kind}
	return schema
}

// TestIntersectUpjetShape covers the common case: a provider whose two halves
// derive from the same underlying schema, so atProvider mirrors forProvider
// and nearly everything is comparable.
func TestIntersectUpjetShape(t *testing.T) {
	t.Parallel()

	forProvider := obj(map[string]*drift.Schema{
		"region":     leaf("string"),
		"objectLock": leaf("boolean"),
	})
	atProvider := obj(map[string]*drift.Schema{
		"region":     leaf("string"),
		"objectLock": leaf("boolean"),
		"arn":        leaf("string"),
		"id":         leaf("string"),
	})

	result := discovery.IntersectSchemas(forProvider, atProvider)
	require.NotNil(t, result)
	assert.Contains(t, result.Properties, "region")
	assert.Contains(t, result.Properties, "objectLock")
	assert.NotContains(t, result.Properties, "arn", "provider-computed fields are not declared and cannot drift")
}

// TestIntersectComputedOnlyShape is the provider-talos case that motivated
// this: the two halves share no field at all, so the kind is not comparable
// and drift must not be reported for it.
func TestIntersectComputedOnlyShape(t *testing.T) {
	t.Parallel()

	forProvider := obj(map[string]*drift.Schema{
		"clusterName": leaf("string"),
		"node":        leaf("string"),
		"machineType": leaf("string"),
	})
	atProvider := obj(map[string]*drift.Schema{
		"generatedTime":            leaf("string"),
		"machineConfiguration":     leaf("string"),
		"machineConfigurationHash": leaf("string"),
	})

	result := discovery.IntersectSchemas(forProvider, atProvider)
	assert.Nil(t, result, "no shared field means nothing to compare")
}

// TestIntersectPartialOverlap keeps only the fields the provider observes back.
func TestIntersectPartialOverlap(t *testing.T) {
	t.Parallel()

	forProvider := obj(map[string]*drift.Schema{
		"region":      leaf("string"),
		"writeOnly":   leaf("string"),
		"description": leaf("string"),
	})
	atProvider := obj(map[string]*drift.Schema{
		"region":      leaf("string"),
		"description": leaf("string"),
		"arn":         leaf("string"),
	})

	result := discovery.IntersectSchemas(forProvider, atProvider)
	require.NotNil(t, result)
	assert.Len(t, result.Properties, 2)
	assert.Contains(t, result.Properties, "region")
	assert.Contains(t, result.Properties, "description")
	assert.NotContains(t, result.Properties, "writeOnly",
		"a declared field the provider never observes back cannot drift")
}

func TestIntersectNested(t *testing.T) {
	t.Parallel()

	forProvider := obj(map[string]*drift.Schema{
		"replication": obj(map[string]*drift.Schema{
			"role":     leaf("string"),
			"declOnly": leaf("string"),
		}),
		"unobserved": obj(map[string]*drift.Schema{"x": leaf("string")}),
	})
	atProvider := obj(map[string]*drift.Schema{
		"replication": obj(map[string]*drift.Schema{
			"role":       leaf("string"),
			"lastStatus": leaf("string"),
		}),
	})

	result := discovery.IntersectSchemas(forProvider, atProvider)
	require.NotNil(t, result)
	require.Contains(t, result.Properties, "replication")
	assert.Contains(t, result.Properties["replication"].Properties, "role")
	assert.NotContains(t, result.Properties["replication"].Properties, "declOnly")
	assert.NotContains(t, result.Properties, "unobserved",
		"a nested object with no observed counterpart is dropped whole")
}

func TestIntersectArraysAndMaps(t *testing.T) {
	t.Parallel()

	forProvider := obj(map[string]*drift.Schema{
		"rules": {Type: "array", Items: obj(map[string]*drift.Schema{
			"id":     leaf("string"),
			"status": leaf("string"),
		})},
		"tags": {Type: "object", AdditionalProperties: leaf("string")},
	})
	atProvider := obj(map[string]*drift.Schema{
		"rules": {Type: "array", Items: obj(map[string]*drift.Schema{
			"id":        leaf("string"),
			"createdAt": leaf("string"),
		})},
		"tags": {Type: "object", AdditionalProperties: leaf("string")},
	})

	result := discovery.IntersectSchemas(forProvider, atProvider)
	require.NotNil(t, result)

	require.Contains(t, result.Properties, "rules")
	require.NotNil(t, result.Properties["rules"].Items)
	assert.Contains(t, result.Properties["rules"].Items.Properties, "id")
	assert.NotContains(t, result.Properties["rules"].Items.Properties, "status",
		"an array element field the provider never reports back cannot drift")

	require.Contains(t, result.Properties, "tags")
	assert.NotNil(t, result.Properties["tags"].AdditionalProperties)
}

func TestIntersectNilInputs(t *testing.T) {
	t.Parallel()

	assert.Nil(t, discovery.IntersectSchemas(nil, obj(nil)))
	assert.Nil(t, discovery.IntersectSchemas(obj(nil), nil))
	assert.Nil(t, discovery.IntersectSchemas(nil, nil))
}

// TestIntersectPreserveUnknownFields keeps a schema-less subtree comparable,
// since there is no declaration to narrow against.
func TestIntersectPreserveUnknownFields(t *testing.T) {
	t.Parallel()

	forProvider := obj(map[string]*drift.Schema{"blob": leaf("string")})
	atProvider := &drift.Schema{Type: "object", PreserveUnknownFields: true}

	result := discovery.IntersectSchemas(forProvider, atProvider)
	require.NotNil(t, result)
	assert.Contains(t, result.Properties, "blob")
}
