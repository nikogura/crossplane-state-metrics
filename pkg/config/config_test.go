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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nikogura/crossplane-state-metrics/pkg/config"
)

// TestParseList covers the house rule that every list-valued setting accepts
// commas AND newlines, so a YAML block scalar stays one entry per line instead
// of one long comma string.
func TestParseList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		value    string
		expected []string
	}{
		{name: "empty", value: "", expected: nil},
		{name: "single", value: "crossplane", expected: []string{"crossplane"}},
		{
			name:     "comma separated",
			value:    "*.crossplane.io,*.upbound.io",
			expected: []string{"*.crossplane.io", "*.upbound.io"},
		},
		{
			name:     "comma separated with spaces",
			value:    "*.crossplane.io, *.upbound.io",
			expected: []string{"*.crossplane.io", "*.upbound.io"},
		},
		{
			name:     "newline separated block scalar",
			value:    "*.crossplane.io\n*.upbound.io\n",
			expected: []string{"*.crossplane.io", "*.upbound.io"},
		},
		{
			name:     "indented block scalar",
			value:    "  *.crossplane.io\n  *.upbound.io",
			expected: []string{"*.crossplane.io", "*.upbound.io"},
		},
		{
			name:     "carriage returns are separators too",
			value:    "a\r\nb",
			expected: []string{"a", "b"},
		},
		{
			name:     "mixed commas and newlines",
			value:    "a,b\nc, d\n",
			expected: []string{"a", "b", "c", "d"},
		},
		{
			name:     "blank lines and trailing separators are dropped",
			value:    "a,,\n\n  \nb,",
			expected: []string{"a", "b"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			actual := config.ParseList(testCase.value)
			assert.Equal(t, testCase.expected, actual)
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(nil)
	require.NoError(t, err)

	assert.Equal(t, config.DefaultMetricsAddr, cfg.MetricsAddr)
	assert.Equal(t, []string{"crossplane", "composite", "claim"}, cfg.Categories,
		"composite and claim are required: XRD-generated CRDs carry neither the crossplane category nor a matching group")
	assert.Equal(t, []string{"*.crossplane.io", "*.upbound.io"}, cfg.Groups)
	assert.Empty(t, cfg.Namespaces, "watching every namespace is the zero-config default")
	assert.True(t, cfg.Drift)
	assert.Equal(t, config.DriftModeSubset, cfg.DriftMode)
	assert.False(t, cfg.DriftFields, "per-field detail is opt-in because it multiplies cardinality")
	assert.False(t, cfg.MetricsPush, "scraping is the default; push is opt-in")
	assert.Equal(t, config.DefaultResync, cfg.Resync)
}

func TestLoadFlags(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{
		"--metrics-addr=:9999",
		"--namespaces=team-a,team-b",
		"--categories=crossplane,managed",
		"--drift-mode=strict",
		"--drift-fields=true",
		"--resync=5m",
		"--otlp-metrics-push=true",
	})
	require.NoError(t, err)

	assert.Equal(t, ":9999", cfg.MetricsAddr)
	assert.Equal(t, []string{"team-a", "team-b"}, cfg.Namespaces)
	assert.Equal(t, []string{"crossplane", "managed"}, cfg.Categories)
	assert.Equal(t, config.DriftModeStrict, cfg.DriftMode)
	assert.True(t, cfg.DriftFields)
	assert.Equal(t, 5*time.Minute, cfg.Resync)
	assert.True(t, cfg.MetricsPush)
}

// TestLoadListFlagsAcceptNewlines proves the block-scalar form survives the
// whole path from flag value to parsed config, not just ParseList in isolation.
func TestLoadListFlagsAcceptNewlines(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{"--groups=*.crossplane.io\n*.upbound.io\n*.m.upbound.io"})
	require.NoError(t, err)

	assert.Equal(t, []string{"*.crossplane.io", "*.upbound.io", "*.m.upbound.io"}, cfg.Groups)
}

func TestLoadEnvironment(t *testing.T) {
	t.Setenv("CSM_METRICS_ADDR", ":7777")
	t.Setenv("CSM_GROUPS", "*.example.io")
	t.Setenv("CSM_DRIFT", "false")
	t.Setenv("CSM_KUBE_QPS", "25.5")

	cfg, err := config.Load(nil)
	require.NoError(t, err)

	assert.Equal(t, ":7777", cfg.MetricsAddr)
	assert.Equal(t, []string{"*.example.io"}, cfg.Groups)
	assert.False(t, cfg.Drift)
	assert.InDelta(t, 25.5, cfg.KubeQPS, 0.001)
}

// TestFlagsBeatEnvironment pins the documented precedence order.
func TestFlagsBeatEnvironment(t *testing.T) {
	t.Setenv("CSM_METRICS_ADDR", ":7777")

	cfg, err := config.Load([]string{"--metrics-addr=:8888"})
	require.NoError(t, err)

	assert.Equal(t, ":8888", cfg.MetricsAddr)
}

// TestUnparseableEnvironmentFallsBack keeps a typo in one variable from
// refusing to start the exporter.
func TestUnparseableEnvironmentFallsBack(t *testing.T) {
	t.Setenv("CSM_RESYNC", "not-a-duration")
	t.Setenv("CSM_KUBE_BURST", "banana")

	cfg, err := config.Load(nil)
	require.NoError(t, err)

	assert.Equal(t, config.DefaultResync, cfg.Resync)
	assert.Equal(t, config.DefaultKubeBurst, cfg.KubeBurst)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		message string
	}{
		{
			name:    "no discovery signals",
			args:    []string{"--categories=", "--groups="},
			message: "at least one of categories or groups",
		},
		{
			name:    "unknown drift mode",
			args:    []string{"--drift-mode=fuzzy"},
			message: "drift-mode must be",
		},
		{
			name:    "zero drift field cap",
			args:    []string{"--drift-fields-max=0"},
			message: "drift-fields-max must be at least 1",
		},
		{
			name:    "non-positive resync",
			args:    []string{"--resync=0"},
			message: "resync must be positive",
		},
		{
			name:    "empty metrics address",
			args:    []string{"--metrics-addr="},
			message: "metrics-addr must not be empty",
		},
		{
			name:    "unknown log level",
			args:    []string{"--log-level=chatty"},
			message: "log-level must be",
		},
		{
			name:    "non-positive kube qps",
			args:    []string{"--kube-qps=0"},
			message: "kube-qps must be positive",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(testCase.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
		})
	}
}
