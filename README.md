# Crossplane State Metrics

Per-object health and configuration drift for every Crossplane object in a
cluster, exported in Prometheus format.

Prometheus **scrapes** `/metrics`. Nothing is pushed by default.

## The Problem

Crossplane tells you a great deal about itself in aggregate and almost nothing
about individual objects.

- **Native provider metrics** (`crossplane_managed_resource_synced` / `_ready` /
  `_exists`, emitted by every crossplane-runtime provider) are aggregate gauges
  labelled only by `gvk` — a count per kind, per provider pod. You learn that
  seven of nine Roles are Ready. You never learn *which* Role is broken. There
  is no namespace dimension, and no drift.
- **kube-state-metrics CustomResourceState** can emit per-object condition
  labels, but every GVK must be enumerated by hand (it has no wildcards) and it
  builds a cluster-wide informer factory per GVK. Past a few dozen CRD kinds it
  churns, CPU-starves, and silently emits a fraction of what was configured. It
  also cannot compute drift — a field diff is not a condition. It is the wrong
  tool at Crossplane's CRD breadth.
- The upstream proposal for per-resource state metrics
  ([crossplane/crossplane#3240](https://github.com/crossplane/crossplane/issues/3240))
  is still a draft.

So two questions go unanswered per object: **is this one healthy**, and **has it
drifted**.

## Crossplane v2 And v1

Built for **Crossplane v2**, and v1 works where the two overlap.

What v2 changes, and how each is handled:

- **Namespaced managed resources.** v2 adds namespace-scoped MRs under
  `*.m.upbound.io` alongside the cluster-scoped `*.upbound.io` ones. Both are
  discovered; a namespaced object reports its own namespace in `xp_namespace`.
  Namespace scoping via `--namespaces` applies to namespaced kinds only —
  cluster-scoped kinds are always watched cluster-wide, because silently
  dropping most of the Crossplane object graph to honour a filter meant for
  namespaced resources would be a surprising way to lose half the cluster.
- **Namespaced composite resources.** An XRD's `spec.scope` is `Namespaced` by
  default in the v2 XRD schema. Composites are found by the `composite`
  category regardless of scope or API group.
- **Deliberate activation.** A v2 provider no longer installs every CRD it
  knows how to manage. `ManagedResourceDefinition.spec.state` defaults to
  `Inactive`, and an inactive definition means the CRD is never created — so
  the kind does not exist and emits no other metric.
  `crossplane_state_managed_resource_definition_active` is the only place that
  absence is visible. See below.
- **New kinds.** `Operation`, `CronOperation`, `WatchOperation`
  (`ops.crossplane.io`), `ManagedResourceActivationPolicy`, and `ClusterUsage`
  (`protection.crossplane.io`) all carry the `crossplane` category and are
  covered by the universal condition metric with no special handling.
- **Claims are legacy.** v2 supersedes claims with namespaced composites, but
  the `claim` category stays in the defaults so v1 and `LegacyCluster`-scoped
  XRDs keep working.

Nothing here is version-gated: the exporter reads what the cluster actually
serves, so a mixed v1/v2 estate reports both.

### Why Activation Deserves Its Own Metric

Discovery reports what exists. A kind that was never activated simply never
appears, so an operator asking why a managed resource has no data gets silence
rather than an answer. And `spec.state` is a spec field, not a condition, so
the universal condition loop cannot surface it either — the same reason
Compositions get a revision metric.

## What It Does

**Kinds are discovered, never enumerated.** Three signals are combined, because
Crossplane labels its CRDs three different ways:

- `crossplane` is carried by every core CRD and every provider-installed managed
  resource, including the Crossplane v2 namespaced ones under `*.m.upbound.io`.
- `composite` and `claim` are what crossplane-runtime appends to the CRDs it
  generates from an XRD. **Those carry no `crossplane` category**, and they live
  in whatever API group the XRD author chose, so neither the category above nor
  the group globs below would find them. Without these two the composite layer
  is invisible — and that is the layer a platform team actually works in.
- API group globs pick up the few kinds that ship with no categories at all,
  `ProviderConfig` being the one operators notice.

A new provider brings dozens of new kinds and they are picked up from the CRD
watch with no configuration change and no restart.

**Conditions are read generically.** Every Crossplane object — managed
resources, composites, claims, definitions, packages, revisions, operations,
usages — carries the identical `xpv1.Condition` shape. One loop covers all of
them, so `Synced`/`Ready`, `Installed`/`Healthy`, `Established`/`Offered` and
`Succeeded` all fall out without per-kind knowledge, and a condition type
Crossplane adds tomorrow appears without a code change.

**That breadth is for diagnosis, not completeness.** A Provider going
`Healthy=False` is *why* four hundred managed resources went `Synced=False`.
Covering packages puts provider health → composite health → managed-resource
health → drift on one board as an actual causal chain.

## What It Does Not Do

Three honest limits:

1. **Drift is managed-resource only.** Only managed resources carry both
   `spec.forProvider` and `status.atProvider`. The composite-level analogue —
   "does this composite still match what its Composition would render?" —
   requires re-running the composition function pipeline. That is a different
   program.
2. **Four kinds carry no status conditions at all**: `Composition`,
   `EnvironmentConfig`, `DeploymentRuntimeConfig` and `ImageConfig` are pure
   spec objects. They still appear in `crossplane_state_resource_info`, and
   Compositions get a purpose-built revision metric instead, because revision
   churn is the signal they do have.
3. **The drift comparison is structural, not a provider plan.** It reads only
   what the Kubernetes API already holds and makes no cloud API calls. It
   catches the common cases — someone changed the cloud out of band, git and
   reality diverged — but it will not catch a difference that exists only in
   representation and that the provider would normalise away.

## Metrics

| Metric | Type | Description |
| --- | --- | --- |
| `crossplane_state_resource_info` | gauge | One series per Crossplane object. Value always 1; identity in labels. Covers the kinds that have no conditions. |
| `crossplane_state_resource_condition` | gauge | One series per object per status condition. Value always 1; state carried in the `status` label. |
| `crossplane_state_mr_drift` | gauge | 1 when a managed resource's declared state no longer matches its observed state, else 0. |
| `crossplane_state_mr_drift_field` | gauge | One series per differing field path. Opt-in, capped per resource. |
| `crossplane_state_composition_revisions` | gauge | Revisions existing for a Composition. |
| `crossplane_state_composition_current_revision` | gauge | Highest revision number for a Composition. |
| `crossplane_state_managed_resource_definition_active` | gauge | 1 when a Crossplane v2 `ManagedResourceDefinition` is Active, 0 when Inactive. |
| `crossplane_state_kind_aggregated` | gauge | 1 for a kind reported in aggregate rather than per object. |
| `crossplane_state_resource_count` | gauge | Object count for an aggregated kind. |
| `crossplane_state_condition_count` | gauge | Objects of an aggregated kind in a given condition state. |
| `crossplane_state_drift_count` | gauge | Drifted managed resources of an aggregated kind. |
| `crossplane_state_scrape_truncated` | gauge | 1 when the series cap cut the scrape short. **Alert on this.** |
| `crossplane_state_series_emitted` | gauge | Series emitted last scrape, for sizing against `--max-series`. |

Exporter self-observability — the golden signals for the exporter process:

| Metric | Type | Description |
| --- | --- | --- |
| `crossplane_state_scrapes_total` | counter | Traffic: scrapes served. |
| `crossplane_state_scrape_errors_total` | counter | Errors, by `reason`. |
| `crossplane_state_scrape_duration_seconds` | histogram | Latency: time to walk the caches. |
| `crossplane_state_scrapes_in_flight` | gauge | Saturation: overlapping scrapes. |
| `crossplane_state_informers_pending` | gauge | Saturation: informers still completing their initial list. |
| `crossplane_state_informers_synced` | gauge | Informers that have synced. |
| `crossplane_state_kinds_watched` | gauge | Crossplane kinds currently informed on. |
| `crossplane_state_informer_sync_errors_total` | counter | Informer failures, by `gvr`. Also how a missing RBAC grant announces itself. |
| `crossplane_state_crd_events_total` | counter | CRD lifecycle events acted on, by `event`. |
| `crossplane_state_drift_duration_seconds` | histogram | Time spent comparing managed resources per scrape. |
| `crossplane_state_drift_errors_total` | counter | Managed resources whose drift could not be computed. |
| `crossplane_state_kube_requests_total` | counter | Outbound API requests, by `verb` and `code`. |
| `crossplane_state_kube_request_duration_seconds` | histogram | Outbound API latency, by `verb`. |
| `crossplane_state_build_info` | gauge | Always 1; carries the `version` label. |

### Label Design

**Every label describing the Crossplane object is `xp_`-prefixed.** One rule,
stated once: anything `xp_`-prefixed came from this exporter; anything
unprefixed came from your scrape configuration.

The prefix is not decoration. In a multi-cluster Prometheus or Thanos setup,
external labels such as `cluster` and `environment` are stamped on at ingestion
and silently **overwrite** any same-named label a target emits. And a bare
`namespace` label resolves to the *exporter pod's* namespace via the scrape
target, colliding with the resource's own namespace — Prometheus renames the
emitted one to `exported_namespace`, and people write queries against the wrong
one for months.

So the object's namespace is `xp_namespace`, and `namespace` and `pod` are left
alone for the scrape target to own. The shipped dashboard relies on exactly
that: its resource panels template off the scrape-supplied `namespace`/`pod`,
unambiguously, because nothing the exporter emits can collide with them.

Labels: `xp_group`, `xp_version`, `xp_kind`, `xp_name`, `xp_namespace`, and
optionally `xp_external_name`. Condition series add `condition`, `status` and
`reason`.

### Cardinality

**Read this before pointing the exporter at a large estate. It is entirely
possible to take down a shared Prometheus with it.**

The arithmetic is simple and linear: **four series per managed resource** —
one `resource_info`, two `resource_condition` (`Synced`, `Ready`), one
`mr_drift`. Composites and packages cost three (no drift). Compositions, XRDs
and definitions number in the dozens and are rounding error.

Measured, not estimated:

| Managed resources | Series | Scrape time | Allocated per scrape |
| --- | --- | --- | --- |
| 1,000 | 4,000 | 5 ms | 5 MB |
| 10,000 | 40,000 | 51 ms | 57 MB |
| 50,000 | 200,000 | 279 ms | 285 MB |
| 250,000 | 1,000,000 | ~1.4 s | ~1.4 GB |

A million active series is a serious load for a Prometheus that is also doing
other work. **Past roughly 100,000 series, give this exporter its own
Prometheus** — a dedicated instance scraping only this target, remote-writing
into your long-term store. That isolates the blast radius: a Crossplane estate
that doubles overnight then degrades one Prometheus you can resize, instead of
the one holding your alerting rules.

Whatever the size, scrape it on a long interval. The shipped `ServiceMonitor`
uses 60s with a 50s timeout, because per-object state does not move fast enough
to justify anything shorter.

#### What Is Bounded By Design

- **State lives in labels, the value is always 1.** The kube-state-metrics
  alternative — one series per object per condition per *status* — would be
  **three times** the series, since it emits `true`, `false` and `unknown` rows
  for every condition.
- **`message` is never emitted.** Only `type`, `status` and `reason`. A
  condition's `message` is free-form provider text and would be an unbounded
  label value, which is the fastest way there is to detonate a Prometheus.
  `reason` is a CamelCase token and is effectively enumerated by
  crossplane-runtime.
- **Drift is one bit.** `mr_drift` costs one series no matter how far an object
  has diverged.
- **Kinds that cannot drift emit no drift series at all** (see above).

#### What Can Blow Up, And The Controls

**`--drift-fields` is the bomb.** It emits one series per differing field path,
and paths include array indices, so a single object with a large drifted array
becomes hundreds of series on its own. Measured on 5,000 objects with 40 drifted
fields each:

| Setting | Series |
| --- | --- |
| **off (default)** | 10,000 |
| on, `--drift-fields-max=10` (default cap) | 60,000 |
| on, cap raised to 1000 | **210,000** |

A 21× blowup. It is opt-in and capped at 10 paths per resource for that reason.
Turn it on to investigate, then turn it back off.

Four controls, in the order you should reach for them:

1. **`--exclude-kinds` / `--exclude-groups`** — the cheapest win. Most estates
   have a couple of kinds that are numerous and individually uninteresting
   (`ProviderConfigUsage` is the classic). Dropping them costs nothing.
2. **`--aggregate-threshold N`** — once a kind exceeds N objects, report it as
   counts by kind and condition instead of per object. An aggregated kind costs
   a fixed handful of series — around seven — regardless of whether it holds
   200 objects or 200,000. You lose per-object drilldown for that kind, and
   `crossplane_state_kind_aggregated` marks it so the absence reads as
   deliberate rather than as the objects not existing.
3. **`--namespaces`** — scopes namespaced kinds. Cluster-scoped kinds are always
   watched cluster-wide regardless, so this narrows less than it looks.
4. **`--max-series`** — the backstop, defaulting to 1,000,000. On reaching it
   the scrape stops emitting and `crossplane_state_scrape_truncated` goes to 1.
   **Alert on that metric.** Truncation is deterministic — kinds are walked in a
   stable order and objects sorted within them — because a surviving set that
   varied between scrapes would churn series and generate staleness markers
   faster than the cardinality the cap was meant to prevent.

`crossplane_state_series_emitted` reports the current draw, so you can size
against the cap before you hit it.

#### Memory Is A Separate Limit

Informer caches hold every watched object in memory, so RAM scales with fleet
size independently of series count. `--trim-cache` (on by default) drops the
fields the exporter never reads before an object is stored. Measured on a
representative managed resource:

| | Cached size |
| --- | --- |
| Untrimmed | 1,719 bytes |
| Trimmed, drift-comparable kind | 357 bytes (**−80%**) |
| Trimmed, everything else | 105 bytes (**−94%**) |

`metadata.managedFields` dominates — server-side apply records an entry per
field per manager, and on a Crossplane managed resource it is routinely larger
than the rest of the object combined. Nothing here reads it. For kinds that are
never drift-compared, the whole `spec` and `status.atProvider` go too.

Size the pod's memory request and limit against your object count, and watch the
dashboard's memory panels — they report utilisation as a percentage of both.

### Useful Queries

```promql
# Which objects are unhealthy, and why.
crossplane_state_resource_condition{condition="Ready", status!="True"}

# Fleet rollup by kind.
count by (xp_kind) (crossplane_state_resource_condition{condition="Ready", status="True"})

# The causal layer: a provider going unhealthy explains the fleet going unsynced.
crossplane_state_resource_condition{xp_kind="Provider", condition="Healthy", status!="True"}

# Everything that has drifted from what git declares.
crossplane_state_mr_drift > 0

# Compositions being edited under running composites.
increase(crossplane_state_composition_revisions[24h]) > 0

# Crossplane v2 kinds that are available but switched off.
crossplane_state_managed_resource_definition_active == 0

# Am I near the series cap?
crossplane_state_series_emitted

# The scrape is incomplete. Alert on this.
crossplane_state_scrape_truncated == 1
```

## Drift Detection

### Which Kinds Can Drift At All

Not every managed resource is comparable, and assuming otherwise is how a drift
exporter earns a reputation for crying wolf.

Providers differ in how much of the declared configuration they observe back.
Upjet-generated resources derive both halves from the same underlying schema, so
`atProvider` mirrors `forProvider` almost field for field. Others report only
computed results: `provider-talos`'s `Configuration` declares `clusterName`,
`node` and `machineType` under `forProvider`, while its `atProvider` carries only
`generatedTime`, `machineConfiguration` and `machineConfigurationHash` — **no
field in common**.

Comparing a declared field against an observed side with no schema slot for it
is comparing against something that structurally cannot be there. That is
absence of an echo, not evidence of divergence, and treating it as drift would
mark every object of such a kind permanently drifted.

So the declared schema is first narrowed to the fields `atProvider` also
declares. Kinds whose intersection is empty are **not drift-comparable**, and
emit no `crossplane_state_mr_drift` series at all. They still appear in
`crossplane_state_resource_info` and their conditions are still reported.

### The Comparison

For each comparable managed resource, both `spec.forProvider` (declared) and
`status.atProvider` (observed) are pruned against that **intersected** schema,
recursively, keeping only properties the schema declares at every level
of nesting including arrays of objects. Pruning the observed side against the
*declared* schema is the move that makes this usable: `status.atProvider` is
full of provider-computed fields — ARNs, IDs, timestamps, nested status blocks —
that nobody declared and nobody could. Left in, every resource would report
permanent drift.

Values carrying no information (`null`, empty string, empty object, empty array)
are then dropped as unset. `false` and `0` are values, not absences, and are
kept. Arrays marked `x-kubernetes-list-type: set` are canonically ordered before
comparison, so a reordering is not drift.

Two modes:

- **`subset`** (default) treats `spec.forProvider` as a subset assertion: every
  field the author declared must match what was observed, and fields left
  undeclared are "don't care". This kills the three dominant false-positive
  classes — provider-computed fields, provider-injected tags such as
  `aws:createdBy`, and provider defaults applied to fields nobody set.
- **`strict`** additionally reports fields present at the provider that nobody
  declared. Noisier, and occasionally what you want.

Results are memoised by `resourceVersion`. Drift can only change when the object
changes, so a steady fleet costs almost nothing per scrape and the cache is
pruned to the live object set every time.

## Configuration

Flags and environment variables both work. Precedence is **flag, then
environment variable, then default**. Every setting has a working default: the
common case — scrape every Crossplane object in the cluster — needs no flags at
all.

| Flag | Environment | Default | Description |
| --- | --- | --- | --- |
| `--metrics-addr` | `CSM_METRICS_ADDR` | `:8080` | Listen address for `/metrics`, `/healthz`, `/readyz`. |
| `--categories` | `CSM_CATEGORIES` | `crossplane,composite,claim` | CRD categories identifying Crossplane kinds. |
| `--groups` | `CSM_GROUPS` | `*.crossplane.io,*.upbound.io` | API group globs identifying Crossplane kinds. |
| `--namespaces` | `CSM_NAMESPACES` | *(all)* | Namespaces to watch. Cluster-scoped kinds ignore this. |
| `--exclude-kinds` | `CSM_EXCLUDE_KINDS` | *(none)* | Kinds to skip. |
| `--exclude-groups` | `CSM_EXCLUDE_GROUPS` | *(none)* | API group globs to skip. |
| `--drift` | `CSM_DRIFT` | `true` | Compute managed-resource drift. |
| `--drift-mode` | `CSM_DRIFT_MODE` | `subset` | `subset` or `strict`. |
| `--drift-fields` | `CSM_DRIFT_FIELDS` | `false` | Emit per-field drift detail. |
| `--drift-fields-max` | `CSM_DRIFT_FIELDS_MAX` | `10` | Field paths emitted per resource. |
| `--external-name-label` | `CSM_EXTERNAL_NAME_LABEL` | `false` | Add `xp_external_name` as a label. |
| `--resync` | `CSM_RESYNC` | `10m` | Informer resync period. |
| `--kube-qps` | `CSM_KUBE_QPS` | `50` | Client-side rate limit, queries per second. |
| `--kube-burst` | `CSM_KUBE_BURST` | `100` | Client-side burst allowance. |
| `--max-series` | `CSM_MAX_SERIES` | `1000000` | Series ceiling per scrape; 0 disables. See **Cardinality**. |
| `--aggregate-threshold` | `CSM_AGGREGATE_THRESHOLD` | `0` | Report a kind in aggregate above this object count; 0 disables. |
| `--trim-cache` | `CSM_TRIM_CACHE` | `true` | Drop unread fields from cached objects to cut memory. |
| `--log-level` | `CSM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `--kubeconfig` | `KUBECONFIG` | *(in-cluster)* | Explicit kubeconfig path. |
| `--otlp-metrics-push` | `CSM_OTLP_METRICS_PUSH` | `false` | Additionally push metrics over OTLP. |
| `--otlp-metrics-interval` | `CSM_OTLP_METRICS_INTERVAL` | `60s` | OTLP push interval. |

### List Values Accept Commas And Newlines

Every list-valued setting splits on commas **and** newlines, so a YAML block
scalar stays readable at one entry per line rather than becoming one long
comma-delimited string:

```yaml
env:
  - name: CSM_GROUPS
    value: |
      *.crossplane.io
      *.upbound.io
      *.m.upbound.io

  - name: CSM_EXCLUDE_KINDS
    value: |
      ProviderConfigUsage
      StoreConfig
```

Both of these are equivalent to the comma form. Blank lines, indentation and
trailing separators are all harmless.

### Tracing

Tracing exports over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` (or
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) is set, and is a cheap no-op otherwise, so
the exporter behaves identically with or without a collector. Logs are
structured JSON and carry `trace_id`/`span_id` when emitted inside a span.

### Pushing Metrics

Prometheus scrapes; that is the supported path and the default. For an
environment that genuinely cannot scrape — a collector-only pipeline, or a
network where nothing can reach the pod — `--otlp-metrics-push` additionally
exports over OTLP on an interval.

It is strictly additive: enabling it does not disable or degrade `/metrics`. The
push carries the **same series a scrape would**, per-object state included, by
bridging the state registry into the periodic reader. Enabling push without an
OTLP endpoint set is a startup error rather than a silent no-op.

## Deployment

Two paths, both tested in CI.

### Helm

```bash
helm install crossplane-state-metrics \
  oci://ghcr.io/nikogura/charts/crossplane-state-metrics \
  --namespace crossplane-system \
  --set serviceMonitor.enabled=true
```

### Kustomize

```bash
kubectl apply -k kubernetes/
```

The Kustomize example deploys into `crossplane-system` and includes both a
`ServiceMonitor` and a `PodMonitor` — exposing `/metrics` is not the same as
having it scraped, so the scrape object ships with the distribution. Apply
whichever your Prometheus selects on.

### RBAC

Read-only, and deliberately **not** cluster-wide.

The core (`""`) API group is excluded from the default rules. The exporter reads
only CRD-defined resources, so it never needs Secrets, ConfigMaps or Pods — and
a blanket `apiGroups: ["*"]` grant would hand it every Secret in the cluster.

The cost is that provider API groups must be enumerated, because RBAC matches
API groups exactly and has no glob: `*.upbound.io` cannot be expressed. Adding a
provider means adding its groups.

A missing grant fails **visibly**: the kind is discovered, its informer is
denied, and the denial lands in `crossplane_state_informer_sync_errors_total{gvr}`
and in the logs. Alert on that metric and you hear about it the first time it
happens.

List what a cluster actually needs:

```bash
kubectl get crd -o jsonpath='{range .items[*]}{.spec.group}{"\n"}{end}' | sort -u
```

Then set them:

```yaml
rbac:
  providerApiGroups:
    - aws.upbound.io
    - s3.aws.upbound.io
    - ec2.aws.upbound.io
```

If enumerating is genuinely impractical, `rbac.clusterReadAll=true` grants
cluster-wide read — including every Secret. Know what you are turning on.

### Health Probes

- **`/healthz`** — liveness. Always reports healthy while the process runs. It
  deliberately does not depend on the Kubernetes API: if the API is unreachable
  the exporter keeps serving its last-good cache and recovers on its own, and
  restarting it would discard that cache to fix nothing.
- **`/readyz`** — readiness. Reports ready once the CRD watch has completed its
  initial list. Before that the view of the cluster is partial, which is worse
  to scrape than nothing.

## Dashboard

`dashboards/crossplane-state-metrics.json` ships with the distribution and
imports into any Grafana: datasources are template variables
(`${prometheus}`, `${loki}`, `${tempo}`), never hardcoded UIDs.

It carries fleet health and drift, the provider/package layer that explains
fleet failures, Composition churn, the exporter's own golden signals, a Loki
logs panel, a Tempo traces panel, and pod resource panels — CPU and memory as a
percentage of **limit** *and* of **request**, plus CPU CFS throttling as a
percentage of periods.

Both denominators are there because they answer opposite questions. Percentage
of limit asks whether the pod is **under**-provisioned: approaching the limit
means throttling or an OOM kill. Percentage of request asks whether it is
**over**-provisioned: riding far below 100% is reserved capacity nobody uses,
which wastes node allocatable and hurts bin-packing; riding above 100% means
under-requested and at risk of eviction under node pressure. Throttling is the
leading indicator — a CPU-limited exporter throttles before its scrape latency
visibly climbs.

A CI test asserts the dashboard only references metrics the exporter actually
emits, so renaming an instrument without updating the dashboard fails the build
rather than silently leaving an empty panel.

## Development

```bash
make lint             # namedreturns, then golangci-lint (both with the integration tag)
make test             # unit + integration + dashboard check + Kustomize and Helm renders
make integration-test # just the integration suite
make docker-verify    # build multi-arch and prove each arch carries its own ELF
make build            # binary into bin/
```

`make test` is the single "run everything" target. A green build that never
renders the distro, and never talks to a real API server, is not a green build.

### Integration Tests

The unit tests use a fake dynamic client. That proves the logic but not the
wiring: it cannot show that discovery survives a real watch, that informers sync
against a real API server, or that the bytes an operator scrapes reflect objects
that really exist.

`test/integration` starts a real `kube-apiserver` and `etcd` through envtest,
installs the real Crossplane CRDs, creates real objects, and asserts on real
Prometheus text output. It covers discovery by category and group glob, status
conditions written through the status subresource, drift including the
provider-injected-tag and provider-computed-field false-positive classes, the
non-comparable-kind case above, composite resources found by the `composite`
category despite a non-Crossplane API group, Crossplane v2 namespaced managed
resources reporting their own namespace, a CRD installed **while running** being
picked up with no restart, and a deleted object disappearing from the next
scrape.

They are behind the `integration` build tag and need envtest binaries, which
`make envtest-assets` fetches. Both linters run with that tag set — a
build-tagged file is otherwise invisible to them and would ship unchecked.

### Multi-Arch Verification

`make docker-verify` builds the manifest list and then checks that the arm64
manifest really contains an `aarch64` ELF and the amd64 one an `x86-64` ELF.
Declaring two platforms to buildx is not the same as shipping two correct
binaries: a per-arch tag holding the wrong binary passes the build, the push,
and every manifest inspection, then fails at runtime. CI gates publishing on it.

Run locally against a cluster:

```bash
go run ./cmd/crossplane-state-metrics --kubeconfig ~/.kube/config --log-level debug
curl -s localhost:8080/metrics | grep crossplane_state_
```

### Layout

```
cmd/crossplane-state-metrics   entrypoint
pkg/config                     flags and environment, list parsing
pkg/discovery                  CRD matching, schema extraction, kind identification
pkg/watch                      CRD watch and per-kind informer lifecycle
pkg/drift                      the comparison engine (no Kubernetes dependency)
pkg/collector                  informer caches to Prometheus metrics
pkg/metrics                    OTel instruments, admin server, client-go adapters
pkg/observability              OTel wiring: metrics, tracing, log correlation
test/integration               envtest suite against a real API server
scripts/verify-multiarch.sh    per-architecture ELF check for the image
```

`pkg/drift` deliberately takes plain Go maps and a reduced schema type, so the
comparison has no Kubernetes dependency and is exercised directly in tests.

The discovery tests run against **unmodified real Crossplane CRDs** checked into
`pkg/discovery/testdata` — the core `apiextensions` and `pkg` definitions plus a
provider's managed resource and ProviderConfig — so the parser is verified
against what actually ships rather than against an idea of it.

### Why A Native Collector, Not OTel Instruments

The exporter's own counters use OpenTelemetry. The per-object state metrics do
not, and the distinction is deliberate.

OTel instruments accumulate: once an attribute set has been recorded it is
exported for the life of the process. That is correct for counters and wrong for
object state — a deleted managed resource would keep reporting `Ready` forever.
A native `prometheus.Collector` that walks the informer caches on each scrape
emits exactly what exists at that moment, and objects that go away simply stop
being emitted.

Both register into registries served by the same endpoint, so one scrape returns
both.

## License

Apache 2.0. See [LICENSE](LICENSE).
