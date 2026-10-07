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

func TestParseFieldMetric(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		entry   string
		want    config.FieldMetric
		message string
	}{
		{
			name:  "namespaced managed resource field",
			entry: "ecs.aws.m.upbound.io/Service:status.atProvider.desiredCount",
			want: config.FieldMetric{
				Group: "ecs.aws.m.upbound.io",
				Kind:  "Service",
				Path:  []string{"status", "atProvider", "desiredCount"},
			},
		},
		{
			name:  "surrounding whitespace is trimmed",
			entry: "  ecs.aws.upbound.io/Service : spec.forProvider.desiredCount ",
			want: config.FieldMetric{
				Group: "ecs.aws.upbound.io",
				Kind:  "Service",
				Path:  []string{"spec", "forProvider", "desiredCount"},
			},
		},
		{
			name:  "single segment path",
			entry: "example.org/Thing:count",
			want:  config.FieldMetric{Group: "example.org", Kind: "Thing", Path: []string{"count"}},
		},
		{name: "missing path", entry: "ecs.aws.upbound.io/Service", message: "group/Kind:dotted.path"},
		{name: "missing kind", entry: "ecs.aws.upbound.io/:status.x", message: "kind"},
		{name: "missing group", entry: "/Service:status.x", message: "group"},
		{name: "empty path", entry: "ecs.aws.upbound.io/Service:", message: "path"},
		{name: "empty path segment", entry: "ecs.aws.upbound.io/Service:status..count", message: "path"},
		{name: "path containing a slash", entry: "ecs.aws.upbound.io/Service:status/count", message: "group/Kind:dotted.path"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := config.ParseFieldMetric(tc.entry)
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

func TestFieldMetricPathAndMatch(t *testing.T) {
	t.Parallel()

	metric, err := config.ParseFieldMetric("ecs.aws.m.upbound.io/Service:status.atProvider.desiredCount")
	require.NoError(t, err)

	assert.Equal(t, "status.atProvider.desiredCount", metric.PathString(), "the field label is the dotted path as written")
	assert.True(t, metric.Matches("ecs.aws.m.upbound.io", "Service"))
	assert.False(t, metric.Matches("ecs.aws.upbound.io", "Service"), "the cluster-scoped group is a different kind")
	assert.False(t, metric.Matches("ecs.aws.m.upbound.io", "Cluster"))
}

// TestFieldMetricsFlag covers the whole path from flag value to parsed config,
// including the block-scalar form.
func TestFieldMetricsFlag(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{
		"--field-metrics=ecs.aws.m.upbound.io/Service:status.atProvider.desiredCount\nrds.aws.upbound.io/Instance:status.atProvider.allocatedStorage",
	})
	require.NoError(t, err)

	require.Len(t, cfg.FieldMetrics, 2)
	assert.Equal(t, "Service", cfg.FieldMetrics[0].Kind)
	assert.Equal(t, []string{"status", "atProvider", "allocatedStorage"}, cfg.FieldMetrics[1].Path)

	defaults, err := config.Load(nil)
	require.NoError(t, err)
	assert.Empty(t, defaults.FieldMetrics, "field metrics are opt-in")
}

func TestFieldMetricsEnvironment(t *testing.T) {
	t.Setenv("CSM_FIELD_METRICS", "ecs.aws.m.upbound.io/Service:status.atProvider.desiredCount")

	cfg, err := config.Load(nil)
	require.NoError(t, err)

	require.Len(t, cfg.FieldMetrics, 1)
	assert.Equal(t, "status.atProvider.desiredCount", cfg.FieldMetrics[0].PathString())
}

func TestFieldMetricsRejectMalformedEntries(t *testing.T) {
	t.Parallel()

	_, err := config.Load([]string{"--field-metrics=ecs.aws.upbound.io/Service"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field-metrics")
}
