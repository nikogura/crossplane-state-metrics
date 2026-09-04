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

package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceContextHandler decorates log records with the active trace and span IDs
// so logs, metrics, and traces correlate in the backend. It is a thin wrapper
// over any slog.Handler (e.g. the JSON handler).
type traceContextHandler struct {
	inner slog.Handler
}

// NewSlogHandler wraps an slog.Handler so every record emitted within a span
// carries trace_id and span_id attributes.
func NewSlogHandler(inner slog.Handler) (handler slog.Handler) {
	handler = &traceContextHandler{inner: inner}
	return handler
}

// Enabled delegates to the wrapped handler.
func (h *traceContextHandler) Enabled(ctx context.Context, level slog.Level) (enabled bool) {
	enabled = h.inner.Enabled(ctx, level)
	return enabled
}

// Handle injects trace correlation attributes when the context carries a valid
// span, then delegates to the wrapped handler.
func (h *traceContextHandler) Handle(ctx context.Context, record slog.Record) (err error) {
	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}

	err = h.inner.Handle(ctx, record)
	return err
}

// WithAttrs re-wraps so the decoration survives logger.With(...).
func (h *traceContextHandler) WithAttrs(attrs []slog.Attr) (handler slog.Handler) {
	handler = &traceContextHandler{inner: h.inner.WithAttrs(attrs)}
	return handler
}

// WithGroup re-wraps so the decoration survives logger.WithGroup(...).
func (h *traceContextHandler) WithGroup(name string) (handler slog.Handler) {
	handler = &traceContextHandler{inner: h.inner.WithGroup(name)}
	return handler
}
