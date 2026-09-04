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

// Package config holds the exporter's runtime configuration and the flag/env
// plumbing that populates it. Every setting has a working default, so the
// common case — scrape every Crossplane object in the cluster — needs no flags
// at all.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default configuration values. Discovery defaults are deliberately broad,
// because three different mechanisms decide what a Crossplane CRD looks like:
//
//   - "crossplane" is carried by every core Crossplane CRD and by every
//     provider-installed managed resource.
//   - "composite" and "claim" are appended by crossplane-runtime to the CRDs it
//     generates from a CompositeResourceDefinition. Those CRDs do NOT carry
//     "crossplane", and they live in whatever API group the XRD author chose,
//     so neither the category above nor the group globs below would find them.
//     Without these two, composite resources are invisible - and they are the
//     layer a platform team actually works in.
//   - The group globs pick up the handful of kinds that ship with no categories
//     at all, ProviderConfig being the one operators notice.
const (
	DefaultMetricsAddr   = ":8080"
	DefaultCategories    = "crossplane,composite,claim"
	DefaultGroups        = "*.crossplane.io,*.upbound.io"
	DefaultResync        = 10 * time.Minute
	DefaultDriftFieldMax = 10
	DefaultLogLevel      = "info"
	DefaultPushInterval  = 60 * time.Second

	// DefaultKubeQPS and DefaultKubeBurst rate-limit the exporter's outbound
	// calls to the Kubernetes API. The defaults sit above client-go's own
	// (5/10) because discovery plus a large informer fleet is list-heavy at
	// startup, and well below anything that would pressure an API server.
	DefaultKubeQPS   = 50.0
	DefaultKubeBurst = 100

	// DefaultMaxSeries is the series ceiling for the state collector. It is a
	// backstop, not a target: at four series per managed resource it allows a
	// fleet of roughly 250,000 objects. Crossing it truncates the scrape
	// deterministically rather than letting one exporter take down a shared
	// Prometheus.
	DefaultMaxSeries = 1000000

	// DefaultAggregateThreshold is the per-kind object count above which a kind
	// is reported in aggregate instead of per object. Zero disables it.
	DefaultAggregateThreshold = 0
)

// Drift comparison modes.
const (
	// DriftModeSubset treats the declared spec.forProvider as a subset
	// assertion: every field the author declared must match what the provider
	// observed, but fields the author left unset are "don't care". This is the
	// default because it does not flag provider-applied defaults and
	// provider-injected tags as drift.
	DriftModeSubset = "subset"

	// DriftModeStrict requires the pruned spec.forProvider and
	// status.atProvider to be deeply equal in both directions. Catches fields
	// that appeared at the provider without being declared, at the cost of
	// flagging provider defaults.
	DriftModeStrict = "strict"
)

// Config is the fully resolved exporter configuration.
type Config struct {
	// MetricsAddr is the listen address for /metrics, /healthz and /readyz.
	MetricsAddr string

	// Namespaces restricts which namespaces are watched. Empty means all.
	Namespaces []string

	// Categories lists the CRD categories that mark a kind as Crossplane-owned.
	Categories []string

	// Groups lists glob patterns matched against a CRD's API group. A CRD is
	// collected if it matches a category OR a group glob.
	Groups []string

	// ExcludeKinds and ExcludeGroups drop otherwise-matching CRDs. Kinds are
	// matched case-insensitively; groups are glob patterns.
	ExcludeKinds  []string
	ExcludeGroups []string

	// Drift enables the managed-resource drift comparison.
	Drift bool

	// DriftMode selects the comparison semantics: DriftModeSubset or
	// DriftModeStrict.
	DriftMode string

	// DriftFields enables the opt-in per-field drift detail metric.
	DriftFields bool

	// DriftFieldsMax caps how many differing field paths are emitted per
	// resource, bounding cardinality on a badly drifted object.
	DriftFieldsMax int

	// ExternalNameLabel adds the crossplane.io/external-name annotation as a
	// label on the resource info metric, giving a join key to cloud inventory
	// at the cost of extra label width.
	ExternalNameLabel bool

	// Resync is the informer resync period.
	Resync time.Duration

	// Kubeconfig is an explicit kubeconfig path. Empty means in-cluster.
	Kubeconfig string

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// MetricsPush additionally exports metrics over OTLP on an interval.
	// Prometheus scrapes /metrics by default; this is for environments that
	// cannot scrape at all. It never replaces the scrape endpoint.
	MetricsPush bool

	// PushInterval is the OTLP metrics export interval.
	PushInterval time.Duration

	// KubeQPS and KubeBurst rate-limit outbound Kubernetes API calls.
	KubeQPS   float64
	KubeBurst int

	// MaxSeries caps how many series the state collector will emit in one
	// scrape. Zero disables the cap. On reaching it the scrape is truncated
	// deterministically and crossplane_state_scrape_truncated goes to 1, so
	// the condition is loud rather than silent.
	MaxSeries int

	// AggregateThreshold is the object count above which a kind is reported in
	// aggregate — counts by kind and condition — instead of one series per
	// object. Zero disables it. Use it for kinds that are numerous and
	// individually uninteresting.
	AggregateThreshold int

	// TrimCache drops the parts of each cached object the exporter never reads.
	// Informer caches hold every watched object in memory, and managedFields
	// alone is frequently larger than the rest of the object combined.
	TrimCache bool
}

// ParseList splits a configuration value into a list, accepting commas AND
// newlines as separators so a YAML block scalar stays readable:
//
//	groups: |
//	  *.crossplane.io
//	  *.upbound.io
//
// Entries are trimmed and empties dropped, so trailing separators and blank
// lines are harmless.
func ParseList(value string) (items []string) {
	fields := strings.FieldsFunc(value, func(r rune) (isSep bool) {
		isSep = r == ',' || r == '\n' || r == '\r'
		return isSep
	})

	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed != "" {
			items = append(items, trimmed)
		}
	}

	return items
}

// Load resolves configuration from the supplied argument list and the process
// environment. Precedence is flag, then environment variable, then default.
// The args slice excludes the program name, as with os.Args[1:].
func Load(args []string) (cfg Config, err error) {
	set := flag.NewFlagSet("crossplane-state-metrics", flag.ContinueOnError)

	metricsAddr := set.String("metrics-addr", envOr("CSM_METRICS_ADDR", DefaultMetricsAddr),
		"listen address for /metrics, /healthz and /readyz")
	namespaces := set.String("namespaces", os.Getenv("CSM_NAMESPACES"),
		"comma- or newline-separated namespaces to watch; empty watches all")
	categories := set.String("categories", envOr("CSM_CATEGORIES", DefaultCategories),
		"comma- or newline-separated CRD categories identifying Crossplane kinds")
	groups := set.String("groups", envOr("CSM_GROUPS", DefaultGroups),
		"comma- or newline-separated API group globs identifying Crossplane kinds")
	excludeKinds := set.String("exclude-kinds", os.Getenv("CSM_EXCLUDE_KINDS"),
		"comma- or newline-separated kinds to skip")
	excludeGroups := set.String("exclude-groups", os.Getenv("CSM_EXCLUDE_GROUPS"),
		"comma- or newline-separated API group globs to skip")
	drift := set.Bool("drift", envBool("CSM_DRIFT", true),
		"compute managed-resource drift (spec.forProvider vs status.atProvider)")
	driftMode := set.String("drift-mode", envOr("CSM_DRIFT_MODE", DriftModeSubset),
		"drift comparison semantics: subset or strict")
	driftFields := set.Bool("drift-fields", envBool("CSM_DRIFT_FIELDS", false),
		"emit the per-field drift detail metric (raises cardinality)")
	driftFieldsMax := set.Int("drift-fields-max", envInt("CSM_DRIFT_FIELDS_MAX", DefaultDriftFieldMax),
		"maximum differing field paths emitted per resource")
	externalName := set.Bool("external-name-label", envBool("CSM_EXTERNAL_NAME_LABEL", false),
		"add the crossplane.io/external-name annotation as a metric label")
	resync := set.Duration("resync", envDuration("CSM_RESYNC", DefaultResync),
		"informer resync period")
	kubeconfig := set.String("kubeconfig", envOr("KUBECONFIG", ""),
		"path to a kubeconfig; empty uses in-cluster configuration")
	logLevel := set.String("log-level", envOr("CSM_LOG_LEVEL", DefaultLogLevel),
		"log level: debug, info, warn or error")
	metricsPush := set.Bool("otlp-metrics-push", envBool("CSM_OTLP_METRICS_PUSH", false),
		"additionally push metrics over OTLP; /metrics is always served regardless")
	pushInterval := set.Duration("otlp-metrics-interval", envDuration("CSM_OTLP_METRICS_INTERVAL", DefaultPushInterval),
		"OTLP metrics push interval")
	kubeQPS := set.Float64("kube-qps", envFloat("CSM_KUBE_QPS", DefaultKubeQPS),
		"client-side rate limit for outbound Kubernetes API calls, in queries per second")
	kubeBurst := set.Int("kube-burst", envInt("CSM_KUBE_BURST", DefaultKubeBurst),
		"client-side burst allowance for outbound Kubernetes API calls")
	maxSeries := set.Int("max-series", envInt("CSM_MAX_SERIES", DefaultMaxSeries),
		"maximum series the state collector emits per scrape; 0 disables the cap")
	aggregateThreshold := set.Int("aggregate-threshold", envInt("CSM_AGGREGATE_THRESHOLD", DefaultAggregateThreshold),
		"report a kind in aggregate once it has more than this many objects; 0 disables")
	trimCache := set.Bool("trim-cache", envBool("CSM_TRIM_CACHE", true),
		"drop unread fields from cached objects to reduce informer memory")

	err = set.Parse(args)
	if err != nil {
		err = fmt.Errorf("parsing flags: %w", err)
		return cfg, err
	}

	cfg = Config{
		MetricsAddr:        *metricsAddr,
		Namespaces:         ParseList(*namespaces),
		Categories:         ParseList(*categories),
		Groups:             ParseList(*groups),
		ExcludeKinds:       ParseList(*excludeKinds),
		ExcludeGroups:      ParseList(*excludeGroups),
		Drift:              *drift,
		DriftMode:          *driftMode,
		DriftFields:        *driftFields,
		DriftFieldsMax:     *driftFieldsMax,
		ExternalNameLabel:  *externalName,
		Resync:             *resync,
		Kubeconfig:         *kubeconfig,
		LogLevel:           *logLevel,
		MetricsPush:        *metricsPush,
		PushInterval:       *pushInterval,
		KubeQPS:            *kubeQPS,
		KubeBurst:          *kubeBurst,
		MaxSeries:          *maxSeries,
		AggregateThreshold: *aggregateThreshold,
		TrimCache:          *trimCache,
	}

	err = cfg.Validate()
	if err != nil {
		return cfg, err
	}

	return cfg, err
}

// Validate reports configuration that cannot produce a working exporter.
func (c *Config) Validate() (err error) {
	if c.MetricsAddr == "" {
		err = errors.New("metrics-addr must not be empty")
		return err
	}

	if len(c.Categories) == 0 && len(c.Groups) == 0 {
		err = errors.New("at least one of categories or groups must be set, or no kinds will be discovered")
		return err
	}

	if c.DriftMode != DriftModeSubset && c.DriftMode != DriftModeStrict {
		err = fmt.Errorf("drift-mode must be %q or %q, got %q", DriftModeSubset, DriftModeStrict, c.DriftMode)
		return err
	}

	if c.DriftFieldsMax < 1 {
		err = fmt.Errorf("drift-fields-max must be at least 1, got %d", c.DriftFieldsMax)
		return err
	}

	if c.Resync <= 0 {
		err = fmt.Errorf("resync must be positive, got %s", c.Resync)
		return err
	}

	if c.MetricsPush && c.PushInterval <= 0 {
		err = fmt.Errorf("otlp-metrics-interval must be positive, got %s", c.PushInterval)
		return err
	}

	if c.KubeQPS <= 0 {
		err = fmt.Errorf("kube-qps must be positive, got %v", c.KubeQPS)
		return err
	}

	if c.KubeBurst < 1 {
		err = fmt.Errorf("kube-burst must be at least 1, got %d", c.KubeBurst)
		return err
	}

	if c.MaxSeries < 0 {
		err = fmt.Errorf("max-series must not be negative, got %d", c.MaxSeries)
		return err
	}

	if c.AggregateThreshold < 0 {
		err = fmt.Errorf("aggregate-threshold must not be negative, got %d", c.AggregateThreshold)
		return err
	}

	err = c.validateLogLevel()
	return err
}

// validateLogLevel checks the log level against the accepted set.
func (c *Config) validateLogLevel() (err error) {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
		return err
	default:
		err = fmt.Errorf("log-level must be debug, info, warn or error, got %q", c.LogLevel)
		return err
	}
}

// envOr returns the environment variable's value, or fallback when unset.
func envOr(key string, fallback string) (value string) {
	value = os.Getenv(key)
	if value == "" {
		value = fallback
	}

	return value
}

// envBool parses a boolean environment variable, returning fallback when the
// variable is unset or unparseable.
func envBool(key string, fallback bool) (value bool) {
	value = fallback

	raw := os.Getenv(key)
	if raw == "" {
		return value
	}

	parsed, parseErr := strconv.ParseBool(strings.TrimSpace(raw))
	if parseErr == nil {
		value = parsed
	}

	return value
}

// envInt parses an integer environment variable, returning fallback when the
// variable is unset or unparseable.
func envInt(key string, fallback int) (value int) {
	value = fallback

	raw := os.Getenv(key)
	if raw == "" {
		return value
	}

	parsed, parseErr := strconv.Atoi(strings.TrimSpace(raw))
	if parseErr == nil {
		value = parsed
	}

	return value
}

// envFloat parses a floating-point environment variable, returning fallback
// when the variable is unset or unparseable.
func envFloat(key string, fallback float64) (value float64) {
	value = fallback

	raw := os.Getenv(key)
	if raw == "" {
		return value
	}

	parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if parseErr == nil {
		value = parsed
	}

	return value
}

// envDuration parses a duration environment variable, returning fallback when
// the variable is unset or unparseable.
func envDuration(key string, fallback time.Duration) (value time.Duration) {
	value = fallback

	raw := os.Getenv(key)
	if raw == "" {
		return value
	}

	parsed, parseErr := time.ParseDuration(strings.TrimSpace(raw))
	if parseErr == nil {
		value = parsed
	}

	return value
}
