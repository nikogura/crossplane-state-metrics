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

package metrics_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
)

// newTestServer builds a server over an empty registry.
func newTestServer() (server *metrics.Server) {
	server = metrics.NewServer(":0", prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	return server
}

// get issues a request against the admin mux.
func get(t *testing.T, server *metrics.Server, path string) (recorder *httptest.ResponseRecorder) {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	recorder = httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	return recorder
}

// TestLivenessIgnoresDependencies is the self-healing contract. Liveness must
// report healthy even when the Kubernetes API is unreachable: failing it would
// have the kubelet restart the exporter into a CrashLoopBackOff that fixes
// nothing and throws away the cached state it was still serving.
func TestLivenessIgnoresDependencies(t *testing.T) {
	t.Parallel()

	server := newTestServer()

	// No readiness checker installed at all stands in for "dependencies are
	// not up yet".
	response := get(t, server, "/healthz")
	assert.Equal(t, http.StatusOK, response.Code)

	server.SetReadinessChecker(func() (ready bool) { return ready })

	response = get(t, server, "/healthz")
	assert.Equal(t, http.StatusOK, response.Code, "liveness is about this process, not its dependencies")
}

func TestReadiness(t *testing.T) {
	t.Parallel()

	server := newTestServer()

	response := get(t, server, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code,
		"readiness must not claim health before anything can verify it")

	server.SetReadinessChecker(func() (ready bool) { return ready })

	response = get(t, server, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)

	server.SetReadinessChecker(func() (ready bool) { ready = true; return ready })

	response = get(t, server, "/readyz")
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "ready")
}

func TestMetricsEndpointServesTheGatherer(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()

	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "crossplane_state_test_value",
		Help: "A gauge used to prove the endpoint serves its registry.",
	})
	gauge.Set(42)

	err := registry.Register(gauge)
	require.NoError(t, err)

	server := metrics.NewServer(":0", registry, slog.New(slog.DiscardHandler))

	response := get(t, server, "/metrics")
	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "crossplane_state_test_value 42")
}

func TestRootAndNotFound(t *testing.T) {
	t.Parallel()

	server := newTestServer()

	root := get(t, server, "/")
	assert.Equal(t, http.StatusOK, root.Code)
	assert.Contains(t, root.Body.String(), "/metrics")

	missing := get(t, server, "/nope")
	assert.Equal(t, http.StatusNotFound, missing.Code)
}
