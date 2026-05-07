# image-builder

A Kubernetes operator that patches container images declaratively using the `ImagePatch` CRD. It generates a Dockerfile from the CR spec and builds the patched image in-cluster using [Kaniko](https://github.com/GoogleContainerTools/kaniko).

## Description

image-builder lets you define image customizations (apt packages, shell commands, environment variables, entrypoint, etc.) as a Kubernetes custom resource. The controller watches `ImagePatch` resources, generates a Dockerfile, and launches a Kaniko job to build and push the patched image to a container registry.

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster

**1. Create your environment config file:**

```sh
cp config-example.env config-myenv.env
# Edit config-myenv.env to set BUILDER_IMAGE_NAME, DEFAULT_IMAGE_REGISTRY, etc.
```

Config variables:

| Variable | Stage | Description |
|---|---|---|
| `BUILDER_IMAGE_NAME` | build | Controller manager image name and tag |
| `CONTAINER_TOOL` | build | Container tool (`docker`, `podman`, etc.) |
| `PLATFORM` | build | Target platform (e.g. `linux/amd64`) |
| `DEFAULT_IMAGE_REGISTRY` | deploy | Default registry for auto-generated target images |
| `KANIKO_IMAGE_NAME` | deploy | Kaniko executor image (override for air-gapped environments) |

**2. Build and push your image:**

```sh
make docker-build docker-push ENV_FILE=config-myenv.env
```

**3. Install the CRDs into the cluster:**

```sh
make install
```

**4. Deploy the Manager to the cluster:**

```sh
make deploy ENV_FILE=config-myenv.env
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

### ImagePatch CRD

Create an `ImagePatch` resource to build a patched image:

```yaml
apiVersion: oms.oms.ogpu.cloud/v1alpha1
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

1. If `DEFAULT_IMAGE_REGISTRY` is set: `<registry>/<base-image-name>-patch:<base-tag>`
2. Fallback: `<base-image-name>-patch:<base-tag>`

The image name is the last segment of `spec.baseImage` (after the last `/`), and the tag is extracted after `:`. If no tag is specified, `latest` is used.

Example: `baseImage: registry.luna.ogpu.cloud/luna/ubuntu-22.04:latest` with `DEFAULT_IMAGE_REGISTRY=registry.luna.ogpu.cloud/patched-images` produces `registry.luna.ogpu.cloud/patched-images/ubuntu-22.04-patch:latest`.

### Test manifests

Example manifests are provided under `test/k8s/`:

- `test/k8s/sshd/` — simple SSH-enabled image patch
- `test/k8s/complicated/` — full example with apt mirror, rootfs overlay, supervisor, podman

```sh
kubectl apply -k test/k8s/sshd/
# or
kubectl apply -k test/k8s/complicated/
```

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer ENV_FILE=config-myenv.env
```

The makefile target generates an `install.yaml` file in the `dist` directory containing all resources needed to install the project.

2. Using the installer:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/image-builder/<tag or branch>/dist/install.yaml
```

## Contributing

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

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
