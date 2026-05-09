# image-patch-operator

A Kubernetes operator that patches container images declaratively using the `ImagePatch` CRD. It generates a Dockerfile from the CR spec and builds the patched image in-cluster using [Kaniko](https://github.com/GoogleContainerTools/kaniko).

## Description

image-patch-operator lets you define image customizations (apt packages, shell commands, environment variables, entrypoint, etc.) as a Kubernetes custom resource. The controller watches `ImagePatch` resources, generates a Dockerfile, and launches a Kaniko job to build and push the patched image to a container registry.

## Getting Started

### Prerequisites

- kubectl v1.21+
- A Kubernetes v1.21+ cluster
- Helm v3.x
- A container registry you can push the controller image to

### 1. Build and push the controller image

The image tag is sourced from `appVersion` in `charts/image-patcher/Chart.yaml`, so the build always matches what the chart will pull. You only need to point the build at your registry:

```sh
make docker-build docker-push IMAGE_REGISTRY=registry.example.com
```

Built artifact: `<IMAGE_REGISTRY>/image-patch-system/image-patch-operator:<appVersion>`.

Override `IMAGE_REPOSITORY` if you publish under a different path, or `PLATFORM` for non-amd64 targets.

### 2. Prepare a values file

Copy and edit one of the bundled examples:

```sh
cp charts/image-patcher/examples/values-ysyb.yaml my-values.yaml
```

Minimal contents:

```yaml
image:
  registry: registry.example.com   # must match the registry you pushed to in step 1

kaniko:
  image:
    registry: registry.example.com           # only override for air-gapped envs
    repository: image-patch-system/kaniko-executor

config:
  defaultImageRegistry: registry.example.com/patched-images
```

Defaults you usually do not need to touch live in `charts/image-patcher/values.yaml`.

### 3. Install the chart

```sh
helm install image-patch ./charts/image-patcher \
  -n image-patch-system --create-namespace \
  -f my-values.yaml
```

> The chart asserts `--namespace image-patch-system` at render time — installing into any other namespace fails fast with an actionable error. ClusterRoleBinding subjects and the leader-election Role are pinned to that namespace, so a different value would yield a half-broken release.

Verify:

```sh
kubectl -n image-patch-system get pods
```

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
