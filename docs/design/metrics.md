# Prometheus Metrics — image-patch-operator

**Status:** Accepted
**Ticket:** MSP-46
**Last updated:** 2026-05-19

## Goals

- Surface the operator's domain behavior (image builds, reconciles, failures) as
  Prometheus metrics so SREs can dashboard and alert on it. The operator is the
  only component that knows about `ImagePatch` builds — no external scraper
  can reconstruct build duration or failure reason from raw kube events, so
  this instrumentation must live in-process.
- Reuse the metrics endpoint that controller-runtime already wires up (`:8443`
  authn/authz-protected; see `cmd/main.go`) so no new server or RBAC is needed.
- Pick names, labels, and buckets up-front so future contributors stay
  consistent and we do not need a label rename later.

## Non-goals

- OpenTelemetry / tracing — out of scope for MSP-46.
- Per-`ImagePatch` time-series from this operator. We deliberately avoid
  `name` / `namespace` labels on histograms and counters; per-resource phase
  visibility belongs in kube-state-metrics (see *Operator vs kube-state-metrics*
  below).
- A `ServiceMonitor` / `PodMonitor` chart template. The metrics `Service`
  exists; whether to ship a `monitoring.coreos.com` resource depends on the
  consumer's Prometheus install and is tracked separately.

## Architecture

Three components, all already in place — this design only adds collectors:

1. **Reconciler (writer).** On interesting transitions (a Kaniko `Job` moves
   to Succeeded/Failed, a reconcile step errors) the reconciler calls
   `counter.Inc()` / `histogram.Observe(d)` on collectors registered with
   controller-runtime's global registry. This is in-memory only; no network
   I/O on the hot path.
2. **Metrics server (exposer).** The same process runs an HTTPS server on
   `:8443/metrics`, set up by `metricsserver.Options` in `cmd/main.go`. On
   scrape it serializes the registry. TLS and authn/authz (via
   `controller-runtime/pkg/metrics/filters`) are already configured.
3. **`Service` (discovery).** `charts/image-patcher/templates/service-metrics.yaml`
   fronts `:8443` so Prometheus (or a `ServiceMonitor`) can find it.

```
ImagePatch event ──▶ reconciler ──▶ in-memory collectors
                                          │
                          GET /metrics    │
        Prometheus ──▶ Service :8443 ──▶ manager metrics server ──▶ collectors
```

Exposing `/metrics` from the operator process is the standard pattern
kubebuilder scaffolds and what every mature controller does (cert-manager,
prometheus-operator, ArgoCD, Tekton, kube-controller-manager). It is the right
home for *workload* metrics — counters and histograms about the work the
operator performs. It is **not** the right home for *resource state* metrics
(see next section).

## What we already get for free

controller-runtime registers a baseline of process and reconciler metrics on
its global registry. We do **not** re-emit these:

| Metric | Source |
|---|---|
| `controller_runtime_reconcile_total{controller,result}` | reconciler outcomes |
| `controller_runtime_reconcile_errors_total{controller}` | reconciler errors |
| `controller_runtime_reconcile_time_seconds{controller}` | reconciler latency |
| `controller_runtime_active_workers{controller}` | concurrency in flight |
| `workqueue_*` | work queue depth / latency / retries |
| `rest_client_*` | API-server request rate and latency |
| `go_*`, `process_*` | runtime and process stats |

The custom metrics below cover what controller-runtime cannot infer: the
**build** that the reconciler kicks off in a Kaniko `Job`.

## Custom metrics

Namespace: `image_patcher`. All names follow the Prometheus convention
`<namespace>_<subsystem>_<name>_<unit>` and use base units (`seconds`, no
millis).

### Shipping now

| Name | Type | Labels | Description |
|---|---|---|---|
| `image_patcher_builds_total` | counter | `result`, `registry`, `image` | Image builds that reached a terminal state. Incremented once per terminal transition, never on requeues. |
| `image_patcher_build_duration_seconds` | histogram | `result`, `registry`, `image` | Wall time from the Kaniko `Job`'s `status.startTime` to the terminal transition observed by the reconciler. Buckets: `30, 60, 120, 300, 600, 1800, 3600` seconds. By design, this equals the sum of `pull + patch + push` once those are added. |
| `image_patcher_reconcile_failures_total` | counter | `reason` | Reconciler-side errors broken down by the operation that failed. Distinct from `controller_runtime_reconcile_errors_total`, which has no `reason`. |

Label values:

- `result` ∈ {`succeeded`, `failed`} — matches `ImagePatch.status.phase`.
- `registry` — the destination registry host parsed from the resolved target
  image. Example: `registry.luna.ogpu.cloud`.
- `image` — the target image **with** tag, **without** registry host. Example:
  for target `registry.luna.ogpu.cloud/patched-images/ubuntu-22.04-patch:latest`,
  the label is `patched-images/ubuntu-22.04-patch:latest`.
- `reason` ∈ {`configmap_apply`, `job_create`, `status_update`, `owner_ref`,
  `get_job`} — one per error-return site in the reconciler.

The build metrics are labeled by the **target** image (what we produce), not
the base image. This matches `ImagePatch.status.image` so an operator can
correlate metric series back to a specific build by status. When pull-side
metrics ship (see *Deferred metrics*), those will be labeled by the **base**
image instead.

### Why these three

- **`builds_total`** — primary "is the operator doing work" signal; the
  succeeded/failed split is the obvious SLO numerator/denominator.
- **`build_duration_seconds`** — bucketed for both alerting on regressions
  ("p95 build time over 20m") and capacity ("how many builds per hour can we
  sustain"). Buckets are tuned for image-build workloads: `apt install`
  finishes in tens of seconds, large CUDA layers can push past 20 minutes.
- **`reconcile_failures_total`** — the controller-runtime error counter tells
  you *that* reconciles are erroring; `reason` tells you *which step* so you
  can route alerts (CM/Job failures often mean RBAC; status updates usually
  mean an outdated client cache). Intentionally unlabeled by `image` —
  reconciler errors are usually systemic, and image labels would scatter the
  signal rather than help locate it.

### Deferred metrics

The metrics below are part of the intended end state but are gated on a way
to source phase timings out of Kaniko, which is a single-process binary that
the reconciler only sees at the `Job` level. They are listed here so the
naming is reserved and shipping them is purely an implementation task.

| Name | Type | Labels | Description |
|---|---|---|---|
| `image_patcher_pull_duration_seconds` | histogram | `result`, `registry`, `image` | Time spent pulling the base image. `image`/`registry` reflect the **base** image. |
| `image_patcher_patch_duration_seconds` | histogram | `result`, `registry`, `image` | Time spent running Dockerfile patch steps. `image`/`registry` reflect the **target** image. |
| `image_patcher_push_duration_seconds` | histogram | `result`, `registry`, `image` | Time spent pushing the patched image. `image`/`registry` reflect the **target** image. |
| `image_patcher_pull_failures_total` | counter | `registry`, `image` | Pulls that failed before patching could start. Distinguished from generic build failures so registry-side problems can be alerted on separately. |

`pull + patch + push` is invariant-equal to `build_duration_seconds` for a
given build, modulo a small constant (Kaniko startup / teardown). Dashboards
that need a phase breakdown should sum these three; dashboards that need
"total wall time" should keep using `build_duration_seconds` to avoid a
discontinuity when these metrics ship.

**Source of truth, TBD.** The leading candidate is parsing Kaniko's
`--log-format=json` output post-completion (adds `pods/log` RBAC and a
log-format dependency). Alternatives are splitting pull/patch/push into
separate containers or running a Pushgateway sidecar; both are larger
architectural changes. The decision is tracked separately — until it lands,
these metrics are not emitted.

### Buckets

Histograms in Prometheus cannot be re-bucketed after the fact, so we pick once:

```
[30, 60, 120, 300, 600, 1800, 3600] seconds
```

Rationale: most patches we care about today fall in the 60–600s range. We keep
a couple of higher buckets (30 min, 1 h) so a slow CUDA layer rebuild doesn't
all collapse into `+Inf`.

## Operator vs kube-state-metrics

We omit `namespace` and `name` labels from custom metrics on purpose. A single
namespace with many `ImagePatch` resources would otherwise create one
time-series per resource per histogram bucket — the well-known "Prometheus
cardinality blowup".

The standard split is:

- **Operator (this doc)** — workload metrics: build counts, durations, failure
  reasons, labeled by the **artifact** (`registry`, `image`). Low cardinality
  as long as the set of patched images is bounded.
- **kube-state-metrics** — per-resource state: which `ImagePatch` is currently
  in which phase, when it was created, how old it is. High cardinality, owned
  by the platform. kube-state-metrics ingests this directly from the CRD via
  its `CustomResourceStateMetrics` config; no operator code needed.

If a downstream user wants `imagepatch_phase{name="...",namespace="..."}` they
should configure kube-state-metrics, not ask this operator for it.

### Cardinality of the `image` label

`image` includes the tag (e.g. `patched-images/ubuntu-22.04-patch:latest`).
This is bounded for the intended usage pattern — patching a fixed set of base
images, occasional tag bumps — but is **unbounded** if downstream users mint
a new tag per build (e.g. one tag per git SHA). Worst-case series count per
histogram is roughly `buckets × |result| × |registry| × |image|`, so for the
three labeled metrics above:

```
3 metrics × 8 buckets × 2 results × N_registries × N_images
```

At 2 registries × 100 distinct image tags that's ~4.8k series, well within
Prometheus' comfortable range. At 2 × 10,000 (per-build tags) it's 480k,
which will hurt. If a deployment hits the "per-build tag" pattern, the
mitigation is either a recording rule that drops the `image` label or a
relabel rule at scrape time — not an operator-side change, because we don't
want to silently lose information from one user to spare another.

## Naming conventions for future metrics

- Prefix every custom metric with `image_patcher_`.
- Use base units, suffixed: `_seconds`, `_bytes`, `_total` (counters).
- Lower-snake-case label values; closed enum sets only — no free-form strings.
- Never put `namespace`, `name`, or any UUID into a label.

## Implementation

### Package layout

```
internal/metrics/metrics.go       Metric definitions + registration
internal/metrics/metrics_test.go  Sanity tests (registration, counter wiring)
```

`internal/metrics` registers all collectors with
`sigs.k8s.io/controller-runtime/pkg/metrics.Registry` in `init()` so they are
served on the same `:8443/metrics` endpoint the manager already exposes. No
changes to `cmd/main.go` are required.

### Recording points

In `internal/controller/imagepatch_controller.go`:

- **`handleExistingJob`** — when `imagePatch.Status.Phase != newPhase` and
  `newPhase` is terminal (`Succeeded` or `Failed`), call
  `metrics.RecordBuildResult(newPhase, registry, image, job.Status.StartTime)`.
  The "phase changed" guard is the existing transition guard, so we do not
  double-count on requeues.
- **Reconciler errors** — wrap the existing error-return sites with
  `metrics.RecordReconcileFailure(reason)` before returning. The `reason`
  string is a constant defined in the metrics package, not a free-form
  message, to keep label cardinality bounded.

### Label extraction

A small helper in `internal/metrics` parses the resolved target image into
`(registry, image)`:

- Split on the first `/`. If the left side contains `.`, `:`, or equals
  `localhost`, treat it as the registry host; otherwise it's a Docker Hub
  short name and `registry` is `docker.io`.
- The `image` label is everything to the right of the registry, tag included.

Examples:

| Resolved target | `registry` | `image` |
|---|---|---|
| `registry.luna.ogpu.cloud/patched-images/ubuntu-22.04-patch:latest` | `registry.luna.ogpu.cloud` | `patched-images/ubuntu-22.04-patch:latest` |
| `ubuntu-22.04-patch:latest` (no `defaultImageRegistry`) | `docker.io` | `ubuntu-22.04-patch:latest` |
| `localhost:5000/foo:bar` | `localhost:5000` | `foo:bar` |

### Duration source

`batchv1.Job.Status.StartTime` is set by the kubelet when the first pod starts,
and `Status.CompletionTime` is set on success. On failure, `CompletionTime` is
nil; we fall back to `time.Now()` as the end timestamp, which is correct to
within one reconcile interval and is the convention used by other operators.
When `StartTime` is nil (the rare case where the reconciler observes the Job
status before the kubelet has stamped one), we skip the observation rather
than recording a misleading zero.

## Scraping

The metrics endpoint is HTTPS on `:8443` with authn/authz via
`controller-runtime/pkg/metrics/filters`. A scraper needs:

1. A `ServiceAccount` bound to the existing `metrics-reader` `ClusterRole`
   (`charts/image-patcher/templates/rbac-metrics-reader.yaml`).
2. The bearer token for that account in its scrape config.
3. `insecure_skip_verify: true` against the in-cluster self-signed cert, or a
   cert-manager-issued cert (see the `[METRICS-WITH-CERTS]` TODO in
   `cmd/main.go`).

A Prometheus Operator user can drop in a `ServiceMonitor`:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: image-patch-operator
  namespace: image-patch-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: image-patcher
  endpoints:
    - port: https
      scheme: https
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tlsConfig:
        insecureSkipVerify: true
```

Whether to ship this as a chart template (gated by
`.Values.metrics.serviceMonitor.enabled`) is left for a follow-up — it depends
on whether downstream consumers have the Prometheus Operator CRDs installed.

## Example alerts

Sketches, not the final PromQL:

```promql
# Build failure ratio over 1h across all registries/images, paged at >20%
# with at least 5 builds.
sum(rate(image_patcher_builds_total{result="failed"}[1h]))
  / sum(rate(image_patcher_builds_total[1h])) > 0.2
  and sum(increase(image_patcher_builds_total[1h])) > 5
```

```promql
# Per-image P95 build duration regression beyond 20 minutes.
histogram_quantile(0.95,
  sum by (le, image) (rate(image_patcher_build_duration_seconds_bucket[30m]))
) > 1200
```

```promql
# Reconciler stuck on a particular failure mode.
sum by (reason) (rate(image_patcher_reconcile_failures_total[15m])) > 0.1
```
