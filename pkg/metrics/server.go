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
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server timeouts. The read and write budgets are generous because a scrape
// of a very large fleet legitimately takes seconds, and a timeout that fires
// mid-collection turns a slow cluster into a metrics outage.
const (
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Server serves the exporter's admin surface: the Prometheus scrape endpoint
// and the health probes.
//
// The exporter has no client-facing listener, so there is nothing to separate
// this from — the whole process is the admin surface. A service that did face
// clients would have to bind this port away from them; that requirement is
// noted here so it is not forgotten if a client-facing listener is ever added.
type Server struct {
	addr     string
	gatherer prometheus.Gatherer
	logger   *slog.Logger

	// ready is swapped in once something can answer readiness. Until then the
	// exporter reports not-ready rather than claiming health it cannot verify.
	ready atomic.Pointer[func() bool]
}

// NewServer builds the admin server over the supplied gatherer.
func NewServer(addr string, gatherer prometheus.Gatherer, logger *slog.Logger) (server *Server) {
	server = &Server{addr: addr, gatherer: gatherer, logger: logger}
	return server
}

// SetReadinessChecker installs the function that decides readiness. It is set
// after construction because the component that knows — the watch manager —
// is built after the server that must report on it.
func (s *Server) SetReadinessChecker(checker func() (ready bool)) {
	s.ready.Store(&checker)
}

// Handler builds the admin mux. It is exported so the routes and probe
// semantics can be exercised without binding a port.
func (s *Server) Handler() (handler http.Handler) {
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{
		// A partial collection is more useful than an error page: one broken
		// kind must not blind an operator to every other kind.
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      &promLogger{logger: s.logger},
	}))
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/", s.handleRoot)

	handler = mux

	return handler
}

// Start serves until the context is cancelled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context) (err error) {
	server := &http.Server{
		Addr:              s.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
	}

	listenConfig := &net.ListenConfig{}

	listener, listenErr := listenConfig.Listen(ctx, "tcp", s.addr)
	if listenErr != nil {
		err = listenErr
		return err
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			s.logger.ErrorContext(ctx, "admin server shutdown", slog.String("error", shutdownErr.Error()))
		}
	}()

	s.logger.InfoContext(ctx, "serving metrics and health probes", slog.String("addr", s.addr))

	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}

	return err
}

// handleHealthz answers liveness.
//
// Liveness is about this process, not its dependencies. If the Kubernetes API
// is unreachable the exporter must keep running on its last-good cache and
// recover when the API returns — reporting unhealthy would have the kubelet
// restart it into a CrashLoopBackOff that fixes nothing and destroys the
// cached state it was still serving.
func (s *Server) handleHealthz(writer http.ResponseWriter, _ *http.Request) {
	writeText(writer, http.StatusOK, "ok")
}

// handleReadyz answers readiness: has the CRD watch completed its initial list
// yet? Before it has, the exporter would report a partial view of the cluster,
// which is worse than reporting nothing.
func (s *Server) handleReadyz(writer http.ResponseWriter, _ *http.Request) {
	checker := s.ready.Load()
	if checker == nil || !(*checker)() {
		writeText(writer, http.StatusServiceUnavailable, "syncing")
		return
	}

	writeText(writer, http.StatusOK, "ready")
}

// handleRoot points a browsing human at the endpoints that matter.
func (s *Server) handleRoot(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		writeText(writer, http.StatusNotFound, "not found")
		return
	}

	writeText(writer, http.StatusOK, "crossplane-state-metrics\n\n/metrics\n/healthz\n/readyz\n")
}

// writeText writes a plain-text response.
func writeText(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)

	_, _ = writer.Write([]byte(body))
}

// promLogger adapts slog to the promhttp error logger interface.
type promLogger struct {
	logger *slog.Logger
}

// Println records a collection error from the Prometheus HTTP handler.
func (p *promLogger) Println(values ...any) {
	p.logger.Error("metrics collection error", slog.Any("detail", values))
}
