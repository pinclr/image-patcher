---
title: Advanced Features
description: Build dedup, Kaniko cache, build-time mirrors, registry pull-through, Healthcheck CronJob, Grafana dashboard, Prometheus metrics, and chart signature verification.
---

## Content-Addressed Build Dedup

The controller computes a deterministic hash of every `ImagePatch` spec. Before creating a Kaniko Job it HEADs `<repo>:dedup-<hash>` in the registry:

- **Cache hit:** retags the existing manifest under the user tag (pure manifest copy, no rebuild) and marks the CR `Succeeded` with `Status.DedupHit=true`.
- **Cache miss:** Kaniko pushes both the user tag and the dedup tag in one shot.

Enabled by default. Disable with `dedup.enabled: false` if your registry retention or quota rules can't tolerate the extra tags.

## Kaniko Build Cache

Set `kaniko.buildCache.enabled: true` to cache intermediate `RUN` layers in a registry repo. The cache repo is derived automatically as `<config.defaultImageRegistry>/kaniko-build-cache`. Multi-node safe.

See `charts/image-patcher/examples/values-example.yaml` for the full `kaniko.buildOptions` tuning surface (`snapshotMode`, `singleSnapshot`, `ignorePaths`, `cacheTTL`).

## Build-Time Mirrors

`kaniko.buildAptMirror` and `kaniko.buildPypiMirror` redirect `apt-get` and `pip install` through mirrors during the Kaniko build **without baking any mirror config into the produced image**. Useful on clusters where upstream registries are slow or unreachable:

```yaml
kaniko:
  buildAptMirror: http://mirrors.163.com/ubuntu        # NetEase mirror
  buildPypiMirror: https://pypi.tuna.tsinghua.edu.cn/simple  # TUNA mirror
```

:::note
These are orthogonal to `spec.apt.mirror`, which bakes a mirror URL into the image's `/etc/apt/sources.list` permanently.
:::

## Registry Pull-Through Mirrors

`kaniko.registryMap` maps upstream registry hosts to pull-through mirrors for base-image fetches. Useful for air-gapped clusters or blocked egress:

```yaml
kaniko:
  registryMap:
    docker.io: docker.mirror.example.com
```

Only affects base-image pulls; pushes still go to the real target registry. `docker.io` is silently normalized to `index.docker.io` before being passed to Kaniko.

## Healthcheck CronJob

Set `healthcheck.enabled: true` to run a synthetic canary CronJob that exercises the full build pipeline (base pull → Dockerfile generation → Kaniko build → registry push) and cache machinery on a schedule. Results surface through the `image_patcher_builds_total` metric.

See `healthcheck.*` in `charts/image-patcher/examples/values-example.yaml` for configuration.

## Grafana Dashboard

Set `dashboards.enabled: true` to bundle the operator's dashboard as a ConfigMap picked up by the kube-prometheus-stack Grafana sidecar automatically. Labels default to `grafana_dashboard=1`; override with `dashboards.label.*` if your sidecar uses different selectors.

## Prometheus Metrics

The controller exposes Prometheus metrics on `:8443/metrics` (HTTPS, authn/authz via controller-runtime). Enable `ServiceMonitor` auto-discovery with `metrics.serviceMonitor.enabled: true`.

See `docs/design/metrics.md` for the full metrics reference.

## Chart Signature Verification

Released charts are signed with [cosign](https://github.com/sigstore/cosign) keyless in CI — the signature is bound to the GitHub Actions workflow identity via Sigstore:

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/pinclr/image-patcher/.github/workflows/cd.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/pinclr/charts/image-patcher:<version>
```
