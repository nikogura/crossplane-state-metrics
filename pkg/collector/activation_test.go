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

package collector_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/collector"
	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// definitionKind is the Crossplane v2 ManagedResourceDefinition kind.
func definitionKind() (kind discovery.Kind) {
	kind = discovery.Kind{
		GVR: schema.GroupVersionResource{
			Group: "apiextensions.crossplane.io", Version: "v1alpha1", Resource: "managedresourcedefinitions",
		},
		GVK: schema.GroupVersionKind{
			Group: "apiextensions.crossplane.io", Version: "v1alpha1", Kind: "ManagedResourceDefinition",
		},
		Categories: []string{"crossplane"},
	}

	return kind
}

// definition builds a ManagedResourceDefinition with the supplied state.
func definition(name string, group string, kind string, state string) (built *unstructured.Unstructured) {
	spec := map[string]any{
		"group": group,
		"names": map[string]any{"kind": kind},
	}

	// An absent state is meaningful: the field defaults to Inactive.
	if state != "" {
		spec["state"] = state
	}

	built = object(name, "", map[string]any{"spec": spec})

	return built
}

// TestManagedResourceDefinitionActivation covers the Crossplane v2 activation
// model. An Inactive definition means the CRD is never created, so the kind
// does not exist and produces no other metric at all — this gauge is the only
// place that fact is visible.
func TestManagedResourceDefinitionActivation(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:   definitionKind(),
		Synced: true,
		Objects: []*unstructured.Unstructured{
			definition("buckets.s3.aws.upbound.io", "s3.aws.upbound.io", "Bucket", "Active"),
			definition("tables.dynamodb.aws.upbound.io", "dynamodb.aws.upbound.io", "Table", "Inactive"),
		},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_managed_resource_definition_active 1 when a Crossplane v2 ManagedResourceDefinition is Active, 0 when Inactive. An inactive definition means the underlying CRD is never created, so that managed resource kind does not exist in the cluster and produces no metrics at all.
# TYPE crossplane_state_managed_resource_definition_active gauge
crossplane_state_managed_resource_definition_active{defines_group="dynamodb.aws.upbound.io",defines_kind="Table",xp_name="tables.dynamodb.aws.upbound.io"} 0
crossplane_state_managed_resource_definition_active{defines_group="s3.aws.upbound.io",defines_kind="Bucket",xp_name="buckets.s3.aws.upbound.io"} 1
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected),
		"crossplane_state_managed_resource_definition_active")
	require.NoError(t, err)
}

// TestManagedResourceDefinitionDefaultsInactive pins the upstream default: the
// field defaults to Inactive, so an absent state must read 0 rather than being
// optimistically treated as active.
func TestManagedResourceDefinitionDefaultsInactive(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:   definitionKind(),
		Synced: true,
		Objects: []*unstructured.Unstructured{
			definition("unset.example.io", "example.io", "Thing", ""),
		},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	expected := `
# HELP crossplane_state_managed_resource_definition_active 1 when a Crossplane v2 ManagedResourceDefinition is Active, 0 when Inactive. An inactive definition means the underlying CRD is never created, so that managed resource kind does not exist in the cluster and produces no metrics at all.
# TYPE crossplane_state_managed_resource_definition_active gauge
crossplane_state_managed_resource_definition_active{defines_group="example.io",defines_kind="Thing",xp_name="unset.example.io"} 0
`

	err := testutil.CollectAndCompare(subject, strings.NewReader(expected),
		"crossplane_state_managed_resource_definition_active")
	require.NoError(t, err)
}

// TestActivationOnlyForDefinitions proves the gauge is scoped to
// ManagedResourceDefinitions and does not leak onto other kinds.
func TestActivationOnlyForDefinitions(t *testing.T) {
	t.Parallel()

	source := stubSource{snapshots: []watch.Snapshot{{
		Kind:    bucketKind(),
		Synced:  true,
		Objects: []*unstructured.Unstructured{object("logs", "", map[string]any{})},
	}}}

	subject := collector.New(source, defaultConfig(t), discardLogger())

	assert.Equal(t, 0, testutil.CollectAndCount(subject,
		"crossplane_state_managed_resource_definition_active"))
}
