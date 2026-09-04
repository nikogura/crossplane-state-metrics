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

package metrics

import (
	"context"
	"net/url"
	"time"

	clientmetrics "k8s.io/client-go/tools/metrics"
)

// RegisterKubeClientMetrics routes client-go's request instrumentation into
// this exporter's own metrics, so every outbound call to the Kubernetes API is
// counted and timed.
//
// Together with the client-side rate limit and client-go's own backoff on
// watch failures, this satisfies the rule that every outbound connection is
// rate limited, retried with backoff, logged, and measured.
//
// client-go permits registration exactly once per process; calling this more
// than once is silently ignored by the library.
func RegisterKubeClientMetrics() {
	clientmetrics.Register(clientmetrics.RegisterOpts{
		RequestLatency: kubeLatencyAdapter{},
		RequestResult:  kubeResultAdapter{},
	})
}

// kubeLatencyAdapter records outbound request duration.
type kubeLatencyAdapter struct{}

// Observe records one request's latency against its verb. The URL is
// deliberately discarded: it carries resource names and would put unbounded
// cardinality into a label.
func (kubeLatencyAdapter) Observe(ctx context.Context, verb string, _ url.URL, latency time.Duration) {
	RecordKubeRequestDuration(ctx, verb, latency.Seconds())
}

// kubeResultAdapter counts outbound requests by result.
type kubeResultAdapter struct{}

// Increment counts one completed request by verb and response code. The host
// is discarded; a single exporter talks to one API server.
func (kubeResultAdapter) Increment(ctx context.Context, code string, method string, _ string) {
	RecordKubeRequest(ctx, method, code)
}
