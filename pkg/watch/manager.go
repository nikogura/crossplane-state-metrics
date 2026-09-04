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

// Package watch keeps a live set of informers in step with the Crossplane
// custom resource definitions installed in a cluster.
//
// The design point is that CRD kinds are DISCOVERED, never enumerated. A new
// provider brings dozens of new kinds; they are picked up from the CRD watch
// without a configuration change or a restart, and a provider uninstall takes
// its kinds back out.
//
// Each kind gets its own informer with its own stop channel rather than
// sharing an informer factory. Factories cannot stop one informer without
// stopping all of them, so a factory-based design leaves a failing watch
// running against a resource that no longer exists every time a provider is
// uninstalled. Per-kind lifecycle costs a few lines and removes that whole
// class of noise — while still never tearing down and rebuilding the informer
// set wholesale, which is the failure mode that makes kube-state-metrics
// unusable at Crossplane's CRD breadth.
package watch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/nikogura/crossplane-state-metrics/pkg/discovery"
	"github.com/nikogura/crossplane-state-metrics/pkg/metrics"
)

// crdResource is the CustomResourceDefinition resource the manager watches to
// discover everything else.
//
//nolint:gochecknoglobals // an immutable resource coordinate, not mutable state
var crdResource = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// Options configures a Manager.
type Options struct {
	// Matcher decides which CRDs are Crossplane-owned.
	Matcher discovery.Matcher

	// Namespaces restricts namespaced kinds to these namespaces. Empty watches
	// all. Cluster-scoped kinds always watch cluster-wide regardless.
	Namespaces []string

	// Resync is the informer resync period.
	Resync time.Duration

	// TrimCache drops unread fields from objects before they are cached.
	TrimCache bool
}

// Snapshot is one kind's objects as of a single read of the caches.
type Snapshot struct {
	// Kind describes the resource these objects belong to.
	Kind discovery.Kind

	// Objects are the cached objects of this kind.
	Objects []*unstructured.Unstructured

	// Synced reports whether every informer backing this kind has completed
	// its initial list. Metrics for an unsynced kind are incomplete rather
	// than wrong.
	Synced bool
}

// Manager owns the CRD watch and the per-kind informers it spawns.
type Manager struct {
	client  dynamic.Interface
	options Options
	logger  *slog.Logger

	mutex sync.RWMutex
	// registrations is keyed by CRD name rather than by resource, so a CRD
	// whose storage version changes replaces its old informer instead of
	// leaving an orphan behind under the previous version's key.
	registrations map[string]*registration

	crdInformer cache.SharedIndexInformer
}

// registration is one discovered kind and the informers serving it. Its
// informer list is built in full before the registration is published and is
// never mutated afterwards, so a reader that has copied it under the lock can
// use it safely once the lock is released.
type registration struct {
	kind      discovery.Kind
	informers []cache.SharedIndexInformer
	stop      chan struct{}
}

// registrationView is a registration's lock-protected fields, copied out so
// the informer caches can be read without holding the manager's lock.
type registrationView struct {
	kind      discovery.Kind
	informers []cache.SharedIndexInformer
}

// New creates a Manager. Nothing is watched until Run is called.
func New(client dynamic.Interface, options Options, logger *slog.Logger) (manager *Manager) {
	manager = &Manager{
		client:        client,
		options:       options,
		logger:        logger,
		registrations: make(map[string]*registration),
	}

	return manager
}

// Run starts the CRD watch and blocks until the context is cancelled, then
// stops every informer it started.
//
// Run does not return an error for a Kubernetes API that is unavailable. The
// informer machinery retries with backoff, and the exporter keeps serving its
// last-good view rather than crash-looping while a dependency recovers.
func (m *Manager) Run(ctx context.Context) (err error) {
	informer := dynamicinformer.NewFilteredDynamicInformer(
		m.client, crdResource, metav1.NamespaceAll, m.options.Resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, nil,
	).Informer()

	m.watchErrors(ctx, informer, crdResource.String())

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(object any) { m.onCRD(ctx, object) },
		UpdateFunc: func(_ any, object any) { m.onCRD(ctx, object) },
		DeleteFunc: func(object any) { m.onCRDDelete(ctx, object) },
	})
	if err != nil {
		m.logger.ErrorContext(ctx, "registering CRD event handler", slog.String("error", err.Error()))
		return err
	}

	m.mutex.Lock()
	m.crdInformer = informer
	m.mutex.Unlock()

	m.logger.InfoContext(ctx, "starting CustomResourceDefinition watch")

	go informer.Run(ctx.Done())

	<-ctx.Done()

	m.stopAll(ctx)

	return err
}

// Ready reports whether the CRD watch has completed its initial list. Until it
// has, the exporter is running but its view of the cluster is incomplete, so
// it must not be sent traffic.
func (m *Manager) Ready() (ready bool) {
	m.mutex.RLock()
	informer := m.crdInformer
	m.mutex.RUnlock()

	if informer == nil {
		return ready
	}

	ready = informer.HasSynced()

	return ready
}

// Snapshot returns every watched kind together with its cached objects. It
// also refreshes the watched-kind and informer-sync gauges, since taking a
// snapshot is the moment those counts are known to be current.
//
// The registrations' fields are copied out under the read lock and the caches
// are read afterwards, outside it. Reading the caches can take real time on a
// large fleet, and holding the lock across that would stall CRD registration
// behind every scrape.
func (m *Manager) Snapshot() (snapshots []Snapshot) {
	m.mutex.RLock()

	views := make([]registrationView, 0, len(m.registrations))
	for _, current := range m.registrations {
		views = append(views, registrationView{kind: current.kind, informers: current.informers})
	}

	m.mutex.RUnlock()

	snapshots = make([]Snapshot, 0, len(views))

	var synced, pending int

	for _, current := range views {
		snapshot := current.snapshot()
		if snapshot.Synced {
			synced++
		} else {
			pending++
		}

		snapshots = append(snapshots, snapshot)
	}

	metrics.SetKindsWatched(len(views))
	metrics.SetInformerSyncState(synced, pending)

	return snapshots
}

// onCRD handles a CustomResourceDefinition appearing or changing.
func (m *Manager) onCRD(ctx context.Context, object any) {
	definition, ok := object.(*unstructured.Unstructured)
	if !ok {
		return
	}

	name := definition.GetName()

	kind, usable := discovery.ParseCRD(definition)

	wanted := usable &&
		discovery.Established(definition) &&
		m.options.Matcher.Matches(kind.GVR.Group, kind.GVK.Kind, kind.Categories)

	if !wanted {
		m.deregister(ctx, name, "no longer matched")
		return
	}

	m.register(ctx, name, kind)
}

// onCRDDelete handles a CustomResourceDefinition being removed, including the
// tombstone form the informer delivers when a delete was missed.
func (m *Manager) onCRDDelete(ctx context.Context, object any) {
	definition, ok := object.(*unstructured.Unstructured)
	if !ok {
		tombstone, isTombstone := object.(cache.DeletedFinalStateUnknown)
		if !isTombstone {
			return
		}

		definition, ok = tombstone.Obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
	}

	metrics.RecordCRDEvent(ctx, "delete")
	m.deregister(ctx, definition.GetName(), "definition deleted")
}

// register starts informers for a newly matched kind. It is idempotent: CRD
// update events fire on every status change, and an unchanged kind must not
// restart its informers.
func (m *Manager) register(ctx context.Context, name string, kind discovery.Kind) {
	if m.refreshExisting(name, kind) {
		return
	}

	// Build the registration in full before publishing it. Appending to a
	// registration already visible in the map would race every reader that had
	// taken the lock and copied a half-populated informer list.
	current := &registration{kind: kind, stop: make(chan struct{})}

	for _, namespace := range m.namespacesFor(kind) {
		informer := dynamicinformer.NewFilteredDynamicInformer(
			m.client, kind.GVR, namespace, m.options.Resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, nil,
		).Informer()

		m.watchErrors(ctx, informer, kind.GVR.String())
		m.trimCache(ctx, informer, kind)

		current.informers = append(current.informers, informer)
	}

	m.mutex.Lock()

	// Re-check: a concurrent event may have registered this kind while the
	// informers above were being built. The ones just built were never
	// started, so dropping them costs nothing.
	existing, present := m.registrations[name]
	if present && existing.kind.GVR == kind.GVR {
		existing.kind = kind
		m.mutex.Unlock()

		return
	}

	if present {
		close(existing.stop)
	}

	m.registrations[name] = current
	m.mutex.Unlock()

	for _, informer := range current.informers {
		go informer.Run(current.stop)
	}

	metrics.RecordCRDEvent(ctx, "register")

	if present {
		m.logger.InfoContext(ctx, "replaced informers for changed definition",
			slog.String("crd", name), slog.String("kind", kind.String()))
	}

	m.logger.InfoContext(ctx, "watching Crossplane kind",
		slog.String("kind", kind.String()),
		slog.Bool("managed", kind.Managed),
		slog.Bool("namespaced", kind.Namespaced))
}

// refreshExisting updates the schema of an already-registered, unchanged kind
// and reports whether that was all that was needed. A CRD upgraded in place
// can change its schema without changing its storage version, so the schema is
// refreshed while the running informers are left alone.
func (m *Manager) refreshExisting(name string, kind discovery.Kind) (handled bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	existing, present := m.registrations[name]
	if present && existing.kind.GVR == kind.GVR {
		existing.kind = kind
		handled = true
	}

	return handled
}

// deregister stops a kind's informers and drops it from the registry. It is a
// no-op for a CRD that was never registered.
func (m *Manager) deregister(ctx context.Context, name string, reason string) {
	m.mutex.Lock()

	current, present := m.registrations[name]
	if !present {
		m.mutex.Unlock()
		return
	}

	close(current.stop)
	delete(m.registrations, name)
	m.mutex.Unlock()

	metrics.RecordCRDEvent(ctx, "deregister")

	m.logger.InfoContext(ctx, "stopped watching Crossplane kind",
		slog.String("kind", current.kind.String()),
		slog.String("reason", reason))
}

// stopAll stops every informer the manager started.
func (m *Manager) stopAll(ctx context.Context) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for name, current := range m.registrations {
		close(current.stop)
		delete(m.registrations, name)
	}

	m.logger.InfoContext(ctx, "stopped all Crossplane informers")
}

// namespacesFor returns the namespaces to watch for a kind. Cluster-scoped
// kinds ignore namespace scoping entirely — most Crossplane objects are
// cluster scoped, and silently dropping them because a namespace filter was
// set for namespaced managed resources would be a surprising way to lose half
// the cluster's state.
func (m *Manager) namespacesFor(kind discovery.Kind) (namespaces []string) {
	if !kind.Namespaced || len(m.options.Namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
		return namespaces
	}

	namespaces = m.options.Namespaces

	return namespaces
}

// watchErrors routes informer watch failures to a counter and a log line
// instead of client-go's default global logging, so a broken kind is
// attributable and alertable.
func (m *Manager) watchErrors(ctx context.Context, informer cache.SharedIndexInformer, resource string) {
	handlerErr := informer.SetWatchErrorHandler(func(_ *cache.Reflector, watchErr error) {
		metrics.RecordInformerSyncError(ctx, resource)
		m.logger.WarnContext(ctx, "informer watch error; retrying with backoff",
			slog.String("resource", resource),
			slog.String("error", watchErr.Error()))
	})
	if handlerErr != nil {
		m.logger.WarnContext(ctx, "could not install watch error handler",
			slog.String("resource", resource),
			slog.String("error", handlerErr.Error()))
	}
}

// trimCache installs the cache transform that strips unread fields before an
// object is stored, when trimming is enabled.
func (m *Manager) trimCache(ctx context.Context, informer cache.SharedIndexInformer, kind discovery.Kind) {
	if !m.options.TrimCache {
		return
	}

	err := informer.SetTransform(trimTransform(kind))
	if err != nil {
		m.logger.WarnContext(ctx, "could not install cache transform; objects will be cached in full",
			slog.String("kind", kind.String()),
			slog.String("error", err.Error()))
	}
}

// snapshot reads one registration's caches.
func (r registrationView) snapshot() (snapshot Snapshot) {
	snapshot = Snapshot{Kind: r.kind, Synced: true}

	for _, informer := range r.informers {
		if !informer.HasSynced() {
			snapshot.Synced = false
			continue
		}

		for _, item := range informer.GetStore().List() {
			object, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			snapshot.Objects = append(snapshot.Objects, object)
		}
	}

	return snapshot
}
