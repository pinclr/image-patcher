# image-patcher

Helm chart for **image-patch-operator**, a Kubernetes operator that builds patched container images from `ImagePatch` custom resources using [Kaniko](https://github.com/GoogleContainerTools/kaniko).

Define image customizations (apt/pip packages, shell commands, env vars, entrypoint, …) as a Kubernetes CR; the controller generates a Dockerfile, runs a Kaniko build Job, and pushes the patched image to your registry.

## TL;DR

```sh
helm install image-patch oci://ghcr.io/pinclr/charts/image-patcher \
  --version <version> \
  --namespace image-patch-system --create-namespace \
  -f my-values.yaml
```

> The chart **must** be installed in the `image-patch-system` namespace — it asserts this at render time and fails fast otherwise (ClusterRoleBinding subjects and the leader-election Role are pinned to it).

## Prerequisites

- Kubernetes v1.21+
- Helm v3.8+ (OCI support)
- A container registry the operator can push the patched images it builds to

## Installing

`image.registry` is required (the chart never silently pulls from docker.io). Point it at the public controller image and set the default push destination:

```yaml
image:
  registry: ghcr.io/pinclr          # or quay.io/pinclr

config:
  defaultImageRegistry: registry.example.com/patched-images   # destination for built images; optional if every ImagePatch sets spec.targetImage
```

Then install from the published OCI chart (see TL;DR). Verify with `kubectl -n image-patch-system get pods`.

## Registry credentials

The operator needs credentials to push the images it builds (and to pull private base images). List each registry under `registryCredentials` and the chart provisions the auth Secret for you — no manual `kubectl create secret`:

```yaml
registryCredentials:
  - registry: registry.example.com
    username: ci-bot
    password: ...            # supply via a separate, gitignored values file
  - registry: other-registry.example.com
    username: robot
    password: ...
```

The chart base64-encodes these into one docker `config.json` Secret named `image-registry-secret` (key `config.json`). It is **general-purpose**: Kaniko uses it for base-image pull, layer cache, and the final push, and the dedup client reuses it. One `auths` entry per registry, so different source/target registries can each carry their own credential. When `config.buildNamespace` differs from the release namespace, the Secret is rendered into both.

Keep passwords out of version control — put them in a separate file layered at install time:

```sh
helm install image-patch ... -f my-values.yaml -f secret-creds.yaml
```

Leave `registryCredentials` empty (`[]`) to keep the legacy behaviour: provision an `Opaque` Secret named `image-registry-secret` (key `config.json`) yourself.

Per-build overrides are available on the CR via `spec.pushSecret` / `spec.pullSecret`; see the [repository README](https://github.com/pinclr/image-patcher#credential-precedence) for the precedence rules.

## Key configuration

| Key | Default | Description |
|---|---|---|
| `image.registry` | `""` (**required**) | Registry hosting the controller image (e.g. `ghcr.io/pinclr`). |
| `image.repository` | `image-patcher-operator` | Controller image repository. |
| `image.tag` | `.Chart.AppVersion` | Controller image tag. |
| `config.defaultImageRegistry` | `""` | Push destination when an `ImagePatch` omits `spec.targetImage`. |
| `config.buildNamespace` | `image-patch-system` | Namespace where Kaniko build Jobs and ConfigMaps land. |
| `registryCredentials` | `[]` | Registry `{registry, username, password}` pairs; auto-populated into `image-registry-secret`. |
| `dedup.enabled` | `true` | Content-addressed build dedup (HEAD + manifest retag short-circuit). |
| `kaniko.image` | _see values-example.yaml_ | Kaniko executor image. |
| `kaniko.buildCache.enabled` | `true` | Reuse intermediate `RUN` layers via a registry cache repo. |
| `kaniko.registryMap` | `{}` | `<target>=<mirror>` pull-through mirrors for base-image fetches. |
| `replicaCount` | `3` | Controller replicas (leader-elected). |
| `metrics.enabled` | `true` | Expose Prometheus metrics + optional `ServiceMonitor`. |
| `healthcheck.enabled` | `false` | Synthetic canary CronJob exercising the build path. |
| `dashboards.enabled` | `false` | Bundle a Grafana dashboard ConfigMap for the sidecar. |

The full, commented set of values lives in [`examples/values-example.yaml`](./examples/values-example.yaml), a complete private-registry deployment.

## The ImagePatch CR

```yaml
apiVersion: oms.ogpu.cloud/v1alpha1
kind: ImagePatch
metadata:
  name: my-app
spec:
  baseImage: ubuntu:24.04
  targetImage: registry.example.com/my-app-patch:24.04   # optional
  apt:
    install: [curl, openssh-server]
  shell:
    - name: setup
      run: mkdir -p /var/log/app
```

Full spec-field reference and credential-precedence rules are in the [repository README](https://github.com/pinclr/image-patcher#imagepatch-crd).

## Verifying the chart signature

Released charts are signed with [cosign](https://github.com/sigstore/cosign) **keyless** in CI — the signature is bound to the GitHub Actions workflow identity via Sigstore, so there is no public key to distribute. Verify a pulled chart against that identity:

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/pinclr/image-patcher/.github/workflows/cd.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/pinclr/charts/image-patcher:<version>
```

A successful verification prints the matched certificate claims; a tampered or unsigned artifact fails non-zero.

## Source

<https://github.com/pinclr/image-patcher>
