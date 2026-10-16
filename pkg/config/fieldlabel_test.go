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

package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nikogura/crossplane-state-metrics/pkg/config"
)

func TestParseFieldLabel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		entry   string
		want    config.FieldLabel
		message string
	}{
		{
			name:  "namespaced managed resource field",
			entry: "ecs.aws.m.upbound.io/Cluster:spec.forProvider.region",
			want: config.FieldLabel{
				Group: "ecs.aws.m.upbound.io",
				Kind:  "Cluster",
				Path:  []string{"spec", "forProvider", "region"},
			},
		},
		{
			name:  "surrounding whitespace is trimmed",
			entry: "  ecs.aws.upbound.io/Cluster : status.atProvider.arn ",
			want: config.FieldLabel{
				Group: "ecs.aws.upbound.io",
				Kind:  "Cluster",
				Path:  []string{"status", "atProvider", "arn"},
			},
		},
		{name: "missing path", entry: "ecs.aws.upbound.io/Cluster", message: "field label"},
		{name: "missing kind", entry: "ecs.aws.upbound.io/:spec.x", message: "kind"},
		{name: "missing group", entry: "/Cluster:spec.x", message: "group"},
		{name: "empty path segment", entry: "ecs.aws.upbound.io/Cluster:spec..region", message: "path"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := config.ParseFieldLabel(tc.entry)
			if tc.message != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.message)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestFieldLabelName: the label is the path's leaf, in snake case, under the
// exporter's xp_ prefix.
func TestFieldLabelName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		leaf string
		want string
	}{
		{leaf: "region", want: "xp_region"},
		{leaf: "availabilityZone", want: "xp_availability_zone"},
		{leaf: "instanceID", want: "xp_instance_id"},
		{leaf: "vpcCIDRBlock", want: "xp_vpc_cidr_block"},
		{leaf: "my-field.v2", want: "xp_my_field_v2"},
		{leaf: "Region", want: "xp_region"},
		{leaf: "ec2", want: "xp_ec2"},
	}

	for _, tc := range cases {
		t.Run(tc.leaf, func(t *testing.T) {
			t.Parallel()

			label := config.FieldLabel{Group: "g", Kind: "K", Path: []string{"spec", "forProvider", tc.leaf}}
			assert.Equal(t, tc.want, label.Name())
		})
	}
}

func TestFieldLabelPathAndMatch(t *testing.T) {
	t.Parallel()

	label, err := config.ParseFieldLabel("ecs.aws.m.upbound.io/Cluster:spec.forProvider.region")
	require.NoError(t, err)

	assert.Equal(t, "spec.forProvider.region", label.PathString())
	assert.True(t, label.Matches("ecs.aws.m.upbound.io", "Cluster"))
	assert.False(t, label.Matches("ecs.aws.upbound.io", "Cluster"), "the cluster-scoped group is a different kind")
	assert.False(t, label.Matches("ecs.aws.m.upbound.io", "Service"))
}

// TestFieldLabelNames: the label set is the distinct names across every
// kind, sorted, so two kinds sharing a leaf share one label.
func TestFieldLabelNames(t *testing.T) {
	t.Parallel()

	labels, err := config.ParseFieldLabels(
		"ecs.aws.m.upbound.io/Service:spec.forProvider.region," +
			"ecs.aws.m.upbound.io/Cluster:spec.forProvider.region," +
			"ec2.aws.m.upbound.io/Instance:spec.forProvider.availabilityZone",
	)
	require.NoError(t, err)

	assert.Equal(t, []string{"xp_availability_zone", "xp_region"}, config.FieldLabelNames(labels))
	assert.Empty(t, config.FieldLabelNames(nil))
}

// TestFieldLabelsFlag covers the whole path from flag value to parsed config,
// including the block-scalar form.
func TestFieldLabelsFlag(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{
		"--field-labels=ecs.aws.m.upbound.io/Cluster:spec.forProvider.region\nrds.aws.upbound.io/Instance:status.atProvider.engineVersion",
	})
	require.NoError(t, err)

	require.Len(t, cfg.FieldLabels, 2)
	assert.Equal(t, "Cluster", cfg.FieldLabels[0].Kind)
	assert.Equal(t, "xp_engine_version", cfg.FieldLabels[1].Name())

	defaults, err := config.Load(nil)
	require.NoError(t, err)
	assert.Empty(t, defaults.FieldLabels, "field labels are opt-in")
}

func TestFieldLabelsEnvironment(t *testing.T) {
	t.Setenv("CSM_FIELD_LABELS", "ecs.aws.m.upbound.io/Cluster:spec.forProvider.region")

	cfg, err := config.Load(nil)
	require.NoError(t, err)

	require.Len(t, cfg.FieldLabels, 1)
	assert.Equal(t, "xp_region", cfg.FieldLabels[0].Name())
}

// TestFieldLabelsRejected: malformed entries, a name that is already an
// identity label, and two fields on one kind that would share a name are all
// configuration errors, not runtime surprises.
func TestFieldLabelsRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		value   string
		message string
	}{
		{name: "malformed", value: "ecs.aws.upbound.io/Cluster", message: "field-labels"},
		{name: "reserved name", value: "ecs.aws.upbound.io/Cluster:metadata.name", message: "reserved label xp_name"},
		{name: "reserved ownership name", value: "ecs.aws.upbound.io/Cluster:spec.composite", message: "reserved label xp_composite"},
		{
			name:    "same name twice on one kind",
			value:   "ecs.aws.upbound.io/Cluster:spec.forProvider.region,ecs.aws.upbound.io/Cluster:status.atProvider.region",
			message: "both produce the label xp_region",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load([]string{"--field-labels=" + tc.value})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.message)
		})
	}

	_, err := config.Load([]string{"--field-labels=ecs.aws.upbound.io/Cluster:spec.forProvider.region,ecs.aws.upbound.io/Service:spec.forProvider.region"})
	require.NoError(t, err, "the same name on two kinds is one shared label")
}

// TestReservedLabelNames pins the identity and ownership labels a field label
// may not shadow.
func TestReservedLabelNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"xp_group", "xp_version", "xp_kind", "xp_name", "xp_namespace", "xp_external_name", "xp_composite", "xp_claim", "xp_claim_namespace"} {
		assert.True(t, config.ReservedLabelName(name), name)
	}

	assert.False(t, config.ReservedLabelName("xp_region"))
}
