---
title: Getting Started
description: Install image-patch-operator on your Kubernetes cluster using Helm.
---

## Prerequisites

- kubectl v1.21+
- A Kubernetes v1.21+ cluster
- Helm v3.8+ (OCI support)
- A container registry where the operator will push the *patched* images it builds
  (the controller image and chart themselves are published publicly)

## 1. Prepare a Values File

`image.registry` is required (the chart never silently pulls from docker.io). Point it at one of the public registries and set where the operator should push the images it builds:

```yaml
image:
  registry: ghcr.io/pinclr        # or quay.io/pinclr

config:
  defaultImageRegistry: registry.example.com/patched-images   # destination for built images; optional if every ImagePatch sets spec.targetImage
```

`image.repository` defaults to `image-patcher-operator` and `image.tag` to the chart's `appVersion`, so the values above resolve to `ghcr.io/pinclr/image-patcher-operator:<appVersion>`. The full, commented set of defaults lives in `charts/image-patcher/examples/values-example.yaml` — start from the example file, as there is no root `values.yaml` in the chart.

For Flux users, `charts/image-patcher/examples/flux-helmrelease-example.yaml` shows a complete `HelmRepository` + `HelmRelease` manifest sourcing the chart from an in-cluster ChartMuseum.

### Registry Credentials

The operator needs credentials to push the patched images it builds (and to pull private base images). List each registry under `registryCredentials` and the chart provisions the auth Secret for you — no manual `kubectl create secret`:

```yaml
registryCredentials:
  - registry: registry.example.com
    username: pushbot
    password: ...            # supply via a separate values file, see below
  - registry: other-registry.example.com
    username: robot
    password: ...
```

The chart base64-encodes these into a single docker `config.json` (`image-registry-secret`, key `config.json`). Both Kaniko (push) and the controller's dedup client read that one Secret and select the matching entry per registry, so **different target registries can use different creds**. When `config.buildNamespace` differs from the release namespace, the chart renders the Secret into both.

Keep passwords out of version control — put them in a separate, gitignored values file and layer it at install time:

```sh
helm install image-patch ... -f my-values.yaml -f secret-creds.yaml
```

Leave `registryCredentials` empty (`[]`) to keep the legacy behaviour and provision the Secret yourself. Per-build overrides are available via the CR's `pushSecret`/`pullSecret` (see [CRD Reference](/crd-reference/)).

## 2. Install the Chart

Install directly from the published OCI chart:

```sh
helm install image-patch oci://ghcr.io/pinclr/charts/image-patcher \
  --version <version> \
  -n image-patch-system --create-namespace \
  -f my-values.yaml
```

:::note
The chart asserts `--namespace image-patch-system` at render time — installing into any other namespace fails fast with an actionable error. ClusterRoleBinding subjects and the leader-election Role are pinned to that namespace.
:::

Verify:

```sh
kubectl -n image-patch-system get pods
```

## Build from Source (Alternative)

To run a self-built controller image — e.g. for an air-gapped or private registry — build and push it yourself, then install the local chart:

```sh
make docker-build docker-push IMAGE_REGISTRIES="registry.example.com/myns"
helm install image-patch ./charts/image-patcher \
  -n image-patch-system --create-namespace \
  -f charts/image-patcher/examples/values-example.yaml \
  -f my-overrides.yaml
```

Built artifact: `registry.example.com/myns/image-patcher-operator:<appVersion>`. `IMAGE_REGISTRIES` is space-separated to publish to several registries at once. Set `my-overrides.yaml`'s `image.registry` to match what you pushed.

## Upgrade

For controller-only upgrades (no CRD schema change):

```sh
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

If the chart's CRD has changed (Helm does not update CRDs on upgrade), apply it first:

```sh
kubectl apply -f charts/image-patcher/crds/
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

## Uninstall

```sh
helm uninstall image-patch -n image-patch-system
```

CRDs are preserved by Helm convention so existing `ImagePatch` resources are not destroyed. Delete them explicitly if desired:

```sh
kubectl delete -f charts/image-patcher/crds/
```

## Test Manifests

Example manifests are provided under `test/k8s/`:

- `test/k8s/sshd/` — simple SSH-enabled image patch
- `test/k8s/complicated/` — full example with apt mirror, rootfs overlay, supervisor, podman

```sh
kubectl apply -k test/k8s/sshd/
# or
kubectl apply -k test/k8s/complicated/
```
