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

package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/nikogura/crossplane-state-metrics/pkg/observability"
)

// TestSlogHandlerAddsTraceCorrelation proves logs emitted inside a span carry
// the trace and span IDs, which is what lets an operator jump from a log line
// to the trace it belongs to.
func TestSlogHandlerAddsTraceCorrelation(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	logger := slog.New(observability.NewSlogHandler(slog.NewJSONHandler(buffer, nil)))

	tracer := sdktrace.NewTracerProvider().Tracer("test")

	ctx, span := tracer.Start(context.Background(), "unit")
	logger.InfoContext(ctx, "inside a span")
	span.End()

	record := map[string]any{}
	err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &record)
	require.NoError(t, err)

	assert.NotEmpty(t, record["trace_id"])
	assert.NotEmpty(t, record["span_id"])
}

// TestSlogHandlerWithoutSpan proves the decoration is silent when there is no
// active span, rather than emitting empty correlation fields.
func TestSlogHandlerWithoutSpan(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	logger := slog.New(observability.NewSlogHandler(slog.NewJSONHandler(buffer, nil)))

	logger.InfoContext(context.Background(), "outside a span")

	record := map[string]any{}
	err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &record)
	require.NoError(t, err)

	assert.NotContains(t, record, "trace_id")
	assert.NotContains(t, record, "span_id")
}

// TestSlogHandlerSurvivesWith proves the decoration is not lost by With or
// WithGroup, which is how the handler is used in practice.
func TestSlogHandlerSurvivesWith(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	logger := slog.New(observability.NewSlogHandler(slog.NewJSONHandler(buffer, nil))).
		With(slog.String("component", "test"))

	tracer := sdktrace.NewTracerProvider().Tracer("test")

	ctx, span := tracer.Start(context.Background(), "unit")
	logger.InfoContext(ctx, "inside a span")
	span.End()

	record := map[string]any{}
	err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &record)
	require.NoError(t, err)

	assert.Equal(t, "test", record["component"])
	assert.NotEmpty(t, record["trace_id"])
}
