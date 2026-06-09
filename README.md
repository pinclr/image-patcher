# image-patch-operator

A Kubernetes operator that patches container images declaratively using the `ImagePatch` CRD. It generates a Dockerfile from the CR spec and builds the patched image in-cluster using [Kaniko](https://github.com/GoogleContainerTools/kaniko).

## Description

image-patch-operator lets you define image customizations (apt packages, shell commands, environment variables, entrypoint, etc.) as a Kubernetes custom resource. The controller watches `ImagePatch` resources, generates a Dockerfile, and launches a Kaniko job to build and push the patched image to a container registry.

## Getting Started

### Prerequisites

- kubectl v1.21+
- A Kubernetes v1.21+ cluster
- Helm v3.8+ (OCI support)
- A container registry where the operator will push the *patched* images it builds
  (the controller image and chart themselves are published publicly — see below)

### Published images and chart

Released artifacts are public — you do not need to build anything to install:

| Artifact | Location |
| --- | --- |
| Controller image | `ghcr.io/pinclr/image-patcher-operator:<appVersion>` (also `:latest`) |
| Controller image (mirror) | `quay.io/pinclr/image-patcher-operator:<appVersion>` |
| Helm chart (OCI) | `oci://ghcr.io/pinclr/charts/image-patcher`, version `<version>` |

The chart is also listed on [Artifact Hub](https://artifacthub.io/packages/search?ts_query_web=image-patcher). The image tag tracks `appVersion`, and the chart version tracks `version`, both in `charts/image-patcher/Chart.yaml`.

### 1. Prepare a values file

`image.registry` is required (the chart never silently pulls from docker.io). Point it at one of the public registries and set where the operator should push the images it builds:

```yaml
image:
  registry: ghcr.io/pinclr        # or quay.io/pinclr

config:
  defaultImageRegistry: registry.example.com/patched-images   # destination for built images; optional if every ImagePatch sets spec.targetImage
```

`image.repository` defaults to `image-patcher-operator` and `image.tag` to the chart's `appVersion`, so the values above resolve to `ghcr.io/pinclr/image-patcher-operator:<appVersion>`. Other defaults you usually do not need to touch live in `charts/image-patcher/values.yaml`; the bundled `charts/image-patcher/examples/` show a private-registry setup.

#### Registry credentials

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

The chart base64-encodes these into a single docker `config.json` (`image-registry-secret`, key `config.json`). Both Kaniko (push) and the controller's dedup client read that one Secret and select the matching entry per registry, so **different target registries can use different creds**. When `config.buildNamespace` differs from the release namespace, the chart renders the Secret into both so builds and the controller each find it.

Keep passwords out of version control: put them in a separate, gitignored values file and layer it at install time so only the passwords come from the secret file:

```sh
helm install image-patch ... -f my-values.yaml -f secret-creds.yaml
```

Leave `registryCredentials` empty (`[]`) to keep the legacy behaviour — provision a Secret named `image-registry-secret` (`Opaque`, key `config.json`) in the release namespace (and the build namespace when they differ) yourself. Per-build overrides are available via the CR's `pushSecret`/`pullSecret` (see [Spec fields](#spec-fields)).

### 2. Install the chart

Install directly from the published OCI chart:

```sh
helm install image-patch oci://ghcr.io/pinclr/charts/image-patcher \
  --version <version> \
  -n image-patch-system --create-namespace \
  -f my-values.yaml
```

> The chart asserts `--namespace image-patch-system` at render time — installing into any other namespace fails fast with an actionable error. ClusterRoleBinding subjects and the leader-election Role are pinned to that namespace, so a different value would yield a half-broken release.

Verify:

```sh
kubectl -n image-patch-system get pods
```

### Build from source (alternative)

To run a self-built controller image — e.g. for an air-gapped or private registry — build and push it yourself, then install the local chart instead of the published one. The image tag is sourced from `appVersion` in `charts/image-patcher/Chart.yaml`, so the build always matches what the chart pulls:

```sh
make docker-build docker-push IMAGE_REGISTRIES="registry.example.com/myns"
helm install image-patch ./charts/image-patcher \
  -n image-patch-system --create-namespace -f my-values.yaml
```

Built artifact: `registry.example.com/myns/image-patcher-operator:<appVersion>`. `IMAGE_REGISTRIES` is space-separated to publish to several registries at once; add `IMAGE_EXTRA_TAGS=latest` for extra tags, or override `IMAGE_REPOSITORY`/`PLATFORM` to change the path or target architecture. Set `my-values.yaml`'s `image.registry` (and `image.repository` if you overrode it) to match what you pushed.

### Upgrade

For controller-only upgrades (no CRD schema change):

```sh
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

If the chart's CRD has changed (Helm does not update CRDs on upgrade), apply it first:

```sh
kubectl apply -f charts/image-patcher/crds/
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

### Uninstall

```sh
helm uninstall image-patch -n image-patch-system
```

CRDs are preserved by Helm convention so existing `ImagePatch` resources are not destroyed. Delete them explicitly if desired:

```sh
kubectl delete -f charts/image-patcher/crds/
```

### ImagePatch CRD

Create an `ImagePatch` resource to build a patched image:

```yaml
apiVersion: oms.ogpu.cloud/v1alpha1
kind: ImagePatch
metadata:
  name: my-app
spec:
  baseImage: ubuntu:24.04
  targetImage: registry.example.com/my-app-patch:24.04  # optional
  env:
    DEBIAN_FRONTEND: "noninteractive"
  apt:
    mirror: http://mirror.example.com/ubuntu  # optional, codename auto-detected
    install:
      - curl
      - openssh-server
  shell:
    - name: setup
      run: |
        mkdir -p /var/log/app
        echo "done"
  entrypoint: ["/usr/bin/tini", "--"]
  cmd: ["/usr/bin/supervisord", "-c", "/etc/supervisor/supervisord.conf"]
```

#### Spec fields

| Field | Type | Required | Description |
|---|---|---|---|
| `baseImage` | string | yes | Base image to patch (e.g. `ubuntu:24.04`) |
| `targetImage` | string | no | Destination image. If omitted, auto-generated as `<registry>/<base-image-name>-patch:<base-tag>` |
| `pushSecret` | string | no | Name of a Secret in the build namespace whose docker config **replaces** the chart-level `image-registry-secret` as the push creds for this build. Use when the target registry needs creds the chart-level Secret lacks. Secret may be `kubernetes.io/dockerconfigjson` or carry a `config.json` key. |
| `pullSecret` | string | no | Name of a Secret in the build namespace whose auths are **merged on top** of the push creds, so this build can pull a private base image while still pushing to the target. Same Secret shapes as `pushSecret`. |
| `env` | map[string]string | no | Environment variables (`ENV` directives) |
| `apt.mirror` | string | no | APT mirror URL; Ubuntu codename is auto-detected from `/etc/os-release` |
| `apt.install` | []string | no | APT packages to install |
| `pip.install` | []string | no | pip packages to install |
| `shell` | []ShellStep | no | Shell commands to run (each becomes a `RUN` layer) |
| `shell[].name` | string | no | Step name (used as Dockerfile comment) |
| `shell[].run` | string | yes | Shell commands; multi-line commands are joined with `&&` |
| `shell[].workdir` | string | no | Working directory for this step |
| `shell[].user` | string | no | User to run this step as |
| `entrypoint` | []string | no | Container entrypoint |
| `cmd` | []string | no | Container default command |

#### Target image resolution

When `spec.targetImage` is not specified, the controller auto-generates the target image name by parsing `spec.baseImage`:

1. If `config.defaultImageRegistry` is set: `<registry>/<base-image-name>-patch:<base-tag>`
2. Fallback: `<base-image-name>-patch:<base-tag>`

The image name is the last segment of `spec.baseImage` (after the last `/`), and the tag is extracted after `:`. If no tag is specified, `latest` is used.

#### Credential precedence

By default every build uses the chart-level `image-registry-secret` (from `registryCredentials`) for both pull and push. The per-CR fields override this for a single build:

- **`pushSecret` replaces, it does not merge.** When set, its docker config becomes the entire base for that build and the chart-level `image-registry-secret` is ignored — so `pushSecret` must itself carry every registry the build touches (target plus base image, unless the base is public/mirrored or covered by `pullSecret`).
- **`pullSecret` merges on top** of the push creds (CR `pushSecret` if set, else the chart-level default), with the pull entry winning on per-registry conflicts. Use it to add private base-image creds without restating the push creds.
- Referencing a missing `pushSecret`/`pullSecret` fails the reconcile (the Secret must already exist in the build namespace), unlike a missing chart-level `image-registry-secret`, which only surfaces when the build pod starts.

Example: `baseImage: registry.luna.ogpu.cloud/luna/ubuntu-22.04:latest` with `config.defaultImageRegistry=registry.luna.ogpu.cloud/patched-images` produces `registry.luna.ogpu.cloud/patched-images/ubuntu-22.04-patch:latest`.

### Test manifests

Example manifests are provided under `test/k8s/`:

- `test/k8s/sshd/` — simple SSH-enabled image patch
- `test/k8s/complicated/` — full example with apt mirror, rootfs overlay, supervisor, podman

```sh
kubectl apply -k test/k8s/sshd/
# or
kubectl apply -k test/k8s/complicated/
```

## Development

### Run the controller locally

For quick iteration without rebuilding the controller image:

```sh
make install   # apply CRDs to your current kubeconfig context
make run       # run the controller against ~/.kube/config
```

### Code generation

After editing API types or adding `+kubebuilder:rbac:` markers:

```sh
make manifests generate    # regenerate CRDs/RBAC and DeepCopy methods
make sync-crds             # propagate updated CRDs to the chart
```

### Tests

```sh
make test       # unit + envtest
make test-e2e   # end-to-end on a Kind cluster
make lint
```

### Chart inspection

```sh
make helm-lint       # lint chart
make helm-template   # render to /tmp/image-patcher.rendered.yaml
```

## Contributing

**NOTE:** Run `make help` for more information on all potential `make` targets.

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html).

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
