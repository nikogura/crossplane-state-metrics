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

// Package metrics declares the exporter's own OpenTelemetry instruments — the
// golden signals for the exporter process itself — and serves them alongside
// the Crossplane state metrics on the admin listener.
//
// The Crossplane state metrics are NOT declared here. They are produced by a
// native Prometheus collector (see pkg/collector) that walks the informer
// caches on each scrape and emits exactly the objects that exist at that
// moment. OTel's instruments accumulate attribute sets for the life of the
// process, which is the wrong model for state that churns: a deleted managed
// resource would keep reporting forever. Self-observability accumulates, so it
// uses OTel; object state does not, so it does not.
package metrics

import (
	"context"
	"errors"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribute keys, shared so labels stay consistent across call sites.
const (
	// AttrReason categorises a failure.
	AttrReason = "reason"

	// AttrGVR identifies the group/version/resource an informer serves.
	AttrGVR = "gvr"

	// AttrEvent is the kind of CRD lifecycle event observed.
	AttrEvent = "event"

	// AttrVerb is the HTTP verb of an outbound Kubernetes API request.
	AttrVerb = "verb"

	// AttrCode is the response code of an outbound Kubernetes API request.
	AttrCode = "code"
)

//nolint:gochecknoglobals // OTel instruments are process-wide singletons by design.
var (
	scrapes            metric.Int64Counter
	scrapeErrors       metric.Int64Counter
	scrapeDuration     metric.Float64Histogram
	scrapesInFlight    metric.Int64UpDownCounter
	informerSyncErrors metric.Int64Counter
	crdEvents          metric.Int64Counter
	driftDuration      metric.Float64Histogram
	driftErrors        metric.Int64Counter
	kubeRequests       metric.Int64Counter
	kubeRequestSeconds metric.Float64Histogram

	// Backing values for the observable gauges. The watch manager updates
	// these; the callbacks read them at collection time.
	kindsWatchedValue     atomic.Int64
	informersSyncedValue  atomic.Int64
	informersPendingValue atomic.Int64
)

// Init creates every instrument against the supplied meter. It must be called
// once, after the global meter provider has been installed. Instruments are
// nil-safe until then, so tooling that never calls Init records nothing rather
// than panicking.
func Init(meter metric.Meter, version string) (err error) {
	var errs []error

	scrapes, err = meter.Int64Counter("crossplane_state_scrapes_total",
		metric.WithDescription("Total metric scrapes served"))
	errs = append(errs, err)

	scrapeErrors, err = meter.Int64Counter("crossplane_state_scrape_errors_total",
		metric.WithDescription("Total scrapes that failed to collect cleanly, by reason"))
	errs = append(errs, err)

	scrapeDuration, err = meter.Float64Histogram("crossplane_state_scrape_duration_seconds",
		metric.WithDescription("Time taken to walk the informer caches and build the metric set"))
	errs = append(errs, err)

	scrapesInFlight, err = meter.Int64UpDownCounter("crossplane_state_scrapes_in_flight",
		metric.WithDescription("Scrapes currently being served"))
	errs = append(errs, err)

	informerSyncErrors, err = meter.Int64Counter("crossplane_state_informer_sync_errors_total",
		metric.WithDescription("Total informer sync failures, by group/version/resource"))
	errs = append(errs, err)

	crdEvents, err = meter.Int64Counter("crossplane_state_crd_events_total",
		metric.WithDescription("Total CustomResourceDefinition lifecycle events acted on, by event"))
	errs = append(errs, err)

	driftDuration, err = meter.Float64Histogram("crossplane_state_drift_duration_seconds",
		metric.WithDescription("Time taken to compute drift across every managed resource in one scrape"))
	errs = append(errs, err)

	driftErrors, err = meter.Int64Counter("crossplane_state_drift_errors_total",
		metric.WithDescription("Total managed resources whose drift could not be computed"))
	errs = append(errs, err)

	kubeRequests, err = meter.Int64Counter("crossplane_state_kube_requests_total",
		metric.WithDescription("Total outbound Kubernetes API requests, by verb and response code"))
	errs = append(errs, err)

	kubeRequestSeconds, err = meter.Float64Histogram("crossplane_state_kube_request_duration_seconds",
		metric.WithDescription("Outbound Kubernetes API request duration in seconds, by verb"))
	errs = append(errs, err)

	errs = append(errs, registerGauges(meter, version))

	err = errors.Join(errs...)

	return err
}

// SetKindsWatched records how many Crossplane kinds are currently informed on.
func SetKindsWatched(count int) {
	kindsWatchedValue.Store(int64(count))
}

// SetInformerSyncState records how many informers have completed their initial
// sync and how many are still pending. Pending informers are the exporter's
// saturation signal: metrics for those kinds are incomplete until they clear.
func SetInformerSyncState(synced int, pending int) {
	informersSyncedValue.Store(int64(synced))
	informersPendingValue.Store(int64(pending))
}

// RecordScrape records one served scrape and how long collection took.
func RecordScrape(ctx context.Context, seconds float64) {
	if scrapes != nil {
		scrapes.Add(ctx, 1)
	}

	if scrapeDuration != nil {
		scrapeDuration.Record(ctx, seconds)
	}
}

// RecordScrapeError records a scrape that could not collect cleanly.
func RecordScrapeError(ctx context.Context, reason string) {
	if scrapeErrors == nil {
		return
	}

	scrapeErrors.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrReason, reason)))
}

// ScrapeStarted and ScrapeFinished bracket a scrape for the in-flight gauge.
func ScrapeStarted(ctx context.Context) {
	if scrapesInFlight != nil {
		scrapesInFlight.Add(ctx, 1)
	}
}

// ScrapeFinished closes the bracket opened by ScrapeStarted.
func ScrapeFinished(ctx context.Context) {
	if scrapesInFlight != nil {
		scrapesInFlight.Add(ctx, -1)
	}
}

// RecordInformerSyncError records an informer that failed to sync.
func RecordInformerSyncError(ctx context.Context, gvr string) {
	if informerSyncErrors == nil {
		return
	}

	informerSyncErrors.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrGVR, gvr)))
}

// RecordCRDEvent records a CRD lifecycle event the watcher acted on.
func RecordCRDEvent(ctx context.Context, event string) {
	if crdEvents == nil {
		return
	}

	crdEvents.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrEvent, event)))
}

// RecordDrift records how long the drift pass took across all managed
// resources in one scrape.
func RecordDrift(ctx context.Context, seconds float64) {
	if driftDuration != nil {
		driftDuration.Record(ctx, seconds)
	}
}

// RecordDriftError records a managed resource whose drift could not be
// computed.
func RecordDriftError(ctx context.Context) {
	if driftErrors != nil {
		driftErrors.Add(ctx, 1)
	}
}

// RecordKubeRequest records one outbound Kubernetes API request.
func RecordKubeRequest(ctx context.Context, verb string, code string) {
	if kubeRequests == nil {
		return
	}

	kubeRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrVerb, verb),
		attribute.String(AttrCode, code),
	))
}

// RecordKubeRequestDuration records the latency of one outbound Kubernetes API
// request.
func RecordKubeRequestDuration(ctx context.Context, verb string, seconds float64) {
	if kubeRequestSeconds == nil {
		return
	}

	kubeRequestSeconds.Record(ctx, seconds, metric.WithAttributes(attribute.String(AttrVerb, verb)))
}

// registerGauges installs the observable gauges backed by atomic values.
func registerGauges(meter metric.Meter, version string) (err error) {
	var errs []error

	_, err = meter.Int64ObservableGauge("crossplane_state_kinds_watched",
		metric.WithDescription("Crossplane kinds currently informed on"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) (cbErr error) {
			observer.Observe(kindsWatchedValue.Load())
			return cbErr
		}))
	errs = append(errs, err)

	_, err = meter.Int64ObservableGauge("crossplane_state_informers_synced",
		metric.WithDescription("Informers that have completed their initial sync"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) (cbErr error) {
			observer.Observe(informersSyncedValue.Load())
			return cbErr
		}))
	errs = append(errs, err)

	_, err = meter.Int64ObservableGauge("crossplane_state_informers_pending",
		metric.WithDescription("Informers still completing their initial sync; metrics for those kinds are incomplete"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) (cbErr error) {
			observer.Observe(informersPendingValue.Load())
			return cbErr
		}))
	errs = append(errs, err)

	_, err = meter.Int64ObservableGauge("crossplane_state_build_info",
		metric.WithDescription("Exporter build information; the value is always 1 and the version is carried as a label"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) (cbErr error) {
			observer.Observe(1, metric.WithAttributes(attribute.String("version", version)))
			return cbErr
		}))
	errs = append(errs, err)

	err = errors.Join(errs...)

	return err
}
