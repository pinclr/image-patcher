# OpenTelemetry Tracing — image-patch-operator

**Status:** Proposed
**Ticket:** TBD (OTel tracing)
**Last updated:** 2026-06-12

## Goals

- Give operators distributed-tracing visibility into the reconcile path so they
  can see *where time goes* and *where failures happen* within a single
  reconcile — dedup HEAD/retag, registry I/O, Dockerfile generation, and the
  creation of the build Job / ConfigMap / auth Secret.
- Complement, not duplicate, the existing Prometheus metrics (see
  [metrics.md](./metrics.md)): metrics answer "how often / how long in
  aggregate"; traces answer "what happened in this specific reconcile, in
  order, and which step was slow or errored".
- Be **opt-in and zero-cost when off**, **fail-open when misconfigured**, and
  use **standard OTLP/OTEL env conventions** so it drops into any collector
  without bespoke flags.

## Non-goals

- **End-to-end build tracing (the Kaniko Job).** Propagating trace context into
  the build Pod so the build itself is a child span is explicitly out of scope
  for the initial work — see *Deferred: end-to-end build tracing*. This doc
  covers **controller-side tracing only (scope A)**.
- Replacing logs or metrics. Tracing is a third signal, correlated with the
  other two, not a replacement.
- Tracing every requeue at 100% sampling. High-volume no-op reconciles are
  controlled by sampling; see *Sampling*.

## Background / current state

- The OTel SDK and the OTLP/gRPC trace exporter are already in `go.mod`, but as
  **`// indirect`** transitive deps — nothing in our code imports them. Tracing
  is greenfield, not half-built.
- The only existing in-process signal is **metrics** (controller-runtime's
  metrics server + the custom `internal/metrics` package). That package is the
  structural precedent for `internal/tracing`.
- `Reconcile(ctx, …)` and every sub-step already take `context.Context`. Spans
  propagate via `ctx`, so instrumentation is additive — no signature churn.

## Architecture

### Provider bootstrap (`internal/tracing`)

A small package that owns the global OpenTelemetry setup:

- `ConfigFromEnv() Config` — reads the env knobs (below).
- `Setup(ctx, cfg, log) func(context.Context) error` — configures the global
  `TracerProvider` + propagator and returns a shutdown/flush function. The
  returned shutdown is **always non-nil** so callers `defer` it
  unconditionally.
- `Tracer(name) trace.Tracer` — the single entry point the controller will use
  to start spans (PR2+). Returns the no-op tracer when disabled.

`cmd/main.go` calls `Setup` once, right after the logger is configured, sharing
the `ctrl.SetupSignalHandler()` context with the manager, and defers the
flush with a 5s timeout.

### Default off, no-op when disabled

When `TRACING_ENABLED != "true"`, `Setup` does nothing and leaves the global
provider as OpenTelemetry's built-in **no-op**. Any future `tracing.Tracer(…)`
call then produces non-recording spans — effectively free. This is what makes
instrumenting the hot reconcile path safe to merge ahead of any collector
being deployed.

### Fail-open

When enabled, exporter setup failures are **logged and swallowed**: the
controller starts and runs without traces rather than crash-looping because a
collector is down. This mirrors the dedup short-circuit's degradation
philosophy — observability is a non-critical signal.

### Exporter = OTLP/gRPC, configured by standard env

`Setup` constructs `otlptracegrpc.New(ctx)` with no endpoint options, so the
exporter reads the standard `OTEL_EXPORTER_OTLP_*` variables (endpoint, TLS /
insecure, headers, timeout) itself. This keeps us aligned with every OTel
collector deployment and avoids inventing chart-specific flags for transport.

### Sampling

Root spans use `ParentBased(TraceIDRatioBased(ratio))`, ratio from
`TRACING_SAMPLER_RATIO` (default 1.0). `ParentBased` means a sampled incoming
parent is always honored; the ratio only governs roots. Because reconciles fire
repeatedly per object (requeues), production deployments are expected to set a
fractional ratio.

### Propagation

W3C `TraceContext` + `Baggage` composite propagator is installed globally so
that, if/when trace context is attached to a CR or carried into a build, it
round-trips through the standard headers.

## Configuration

| Env var | Default | Meaning |
| --- | --- | --- |
| `TRACING_ENABLED` | `false` | Master switch. Off → global no-op tracer. |
| `TRACING_SAMPLER_RATIO` | `1.0` | Root-span sampling probability, `[0,1]`. |
| `OTEL_SERVICE_NAME` | `image-patch-operator` | Resource `service.name`. |
| `OTEL_SERVICE_VERSION` | _(unset)_ | Resource `service.version` (chart sets to `appVersion`). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (+ `_TRACES_ENDPOINT`, `_INSECURE`, `_HEADERS`, …) | SDK default | Exporter transport — read by the OTLP exporter directly. |

## Span model (phased)

Spans will be added incrementally (PR2+), highest value first:

- **`Reconcile`** root span, attributes: `namespace`/`name`, `specHash`, dedup
  hit/miss, build namespace, cross-namespace flag, resolved destination.
- **`tryDedupShortCircuit`** + the `internal/registry` `Exists` / `Retag` calls
  — the network-bound, latency-sensitive path.
- **create-or-update** of the build Job / ConfigMap / auth Secret.
- **Dockerfile generation**.

Span status is set from returned errors; the existing
`metrics.RecordReconcileFailure` reason becomes a span attribute so traces and
metrics share vocabulary.

## Correlation with logs & metrics

The controller logs via controller-runtime's `logr`. A follow-up wires
`trace_id` / `span_id` into log records so an SRE can pivot trace ↔ logs ↔
metrics. This is most of the operational payoff and is cheap once the provider
exists.

## Gotchas (designed around)

- **Requeue volume.** `Reconcile` runs on every watch event and requeue. 100%
  sampling is noisy and costly; the `ParentBased(TraceIDRatio)` sampler and a
  fractional default in production keep it bounded. We may later only emit a
  root span on meaningful state change.
- **Async builds don't fit one span.** A reconcile span ends when `Reconcile`
  returns, but a Kaniko build spans *many* reconciles. The realistic model is
  **per-reconcile-pass tracing**, not one span for "the whole build". Whole-build
  correlation needs **span links keyed by `specHash`** — deferred.
- **Roots, not children.** Reconciles are triggered by K8s watch events, not an
  inbound request carrying trace context, so each reconcile starts a fresh root
  trace. That is expected for an operator.

## Deferred: end-to-end build tracing (scope B)

Making the Kaniko build a child span of the triggering action requires:
(1) capturing/propagating a trace context (e.g. as a CR annotation at create
time), (2) carrying it into the build Pod (env/annotation), and (3) the build
emitting or being wrapped to emit a span — Kaniko emits no OTel spans natively.
This is real distributed-tracing work and is intentionally **not** in the
initial scope. Tracked as a research follow-up.

## Rollout / PRs

1. **PR1 — bootstrap (this design):** `internal/tracing` provider + `main.go`
   wiring + config, no-op default, fail-open, graceful flush. No spans yet.
   Dependency change is minimal — only flips the already-present otel deps from
   indirect to direct (no transitive version churn).
2. **PR2 — instrument** `Reconcile` + sub-steps with spans/attributes; wire
   `trace_id`/`span_id` into logs.
3. **PR3 — registry** client spans (`internal/registry`).
4. **PR4 — Helm surface:** `tracing.enabled` / sampler / `OTEL_*` values →
   controller Deployment env; chart README docs.
5. **(Later) PR5 — research:** end-to-end build tracing (scope B).

## Testing

The provider and config parsing are unit-tested (`internal/tracing`). Span
instrumentation (PR2+) is testable with OTel's `tracetest` in-memory exporter —
assert emitted span names/attributes/status — fitting the project's existing
heavy `_test.go` culture, with no live collector required.

## Alternatives considered

- **OTEL env-only auto-config (no `Config` struct).** Rejected for the master
  switch and sampler: we want an explicit, testable `TRACING_ENABLED` and a
  ratio that the chart can surface, while still delegating *transport* to the
  standard `OTEL_EXPORTER_OTLP_*` vars.
- **Crash on exporter failure.** Rejected — tracing is non-critical; failing
  open keeps the operator available.
- **HTTP (`otlptracehttp`) exporter.** gRPC is already the vendored exporter
  and is the common in-cluster collector transport; HTTP can be added later if
  needed without changing this design.
