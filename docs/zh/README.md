# image-patch-operator

一个通过 `ImagePatch` CRD 以声明式方式修补容器镜像的 Kubernetes Operator。它根据 CR spec 生成 Dockerfile，并使用 [Kaniko](https://github.com/GoogleContainerTools/kaniko) 在集群内构建修补后的镜像。

[English](../../README.md)

## 简介

image-patch-operator 让你可以将镜像的定制内容（apt 软件包、shell 命令、环境变量、entrypoint 等）定义为一个 Kubernetes 自定义资源。控制器会监听 `ImagePatch` 资源，生成 Dockerfile，并启动一个 Kaniko Job 来构建修补后的镜像并推送到容器镜像仓库。

## 快速开始

### 前置条件

- kubectl v1.21+
- Kubernetes v1.21+ 集群
- Helm v3.8+（支持 OCI）
- 一个容器镜像仓库，供 Operator 推送它所构建的**修补后**镜像
  （控制器镜像和 Chart 本身是公开发布的 —— 详见下文）

### 已发布的镜像与 Chart

发布的制品都是公开的 —— 你无需构建任何东西即可安装：

| 制品 | 位置 |
| --- | --- |
| 控制器镜像 | `ghcr.io/pinclr/image-patcher-operator:<appVersion>`（同时有 `:latest`） |
| 控制器镜像（镜像源） | `quay.io/pinclr/image-patcher-operator:<appVersion>` |
| Helm Chart（OCI） | `oci://ghcr.io/pinclr/charts/image-patcher`，版本 `<version>` |

该 Chart 同时收录于 [Artifact Hub](https://artifacthub.io/packages/search?ts_query_web=image-patcher)。镜像 tag 跟随 `appVersion`，Chart 版本跟随 `version`，二者都定义在 `charts/image-patcher/Chart.yaml` 中。

### 1. 准备 values 文件

`image.registry` 是必填项（该 Chart 绝不会静默地从 docker.io 拉取镜像）。将其指向某个公开镜像仓库，并设置 Operator 应将其构建的镜像推送到何处：

```yaml
image:
  registry: ghcr.io/pinclr        # 或 quay.io/pinclr

config:
  defaultImageRegistry: registry.example.com/patched-images   # 构建镜像的目标地址；如果每个 ImagePatch 都设置了 spec.targetImage 则可选
```

`image.repository` 默认为 `image-patcher-operator`，`image.tag` 默认为 Chart 的 `appVersion`，因此上面的配置会解析为 `ghcr.io/pinclr/image-patcher-operator:<appVersion>`。完整且带注释的默认值集合位于 [`charts/image-patcher/examples/values-example.yaml`](../../charts/image-patcher/examples/values-example.yaml) —— 这是一份完整的私有仓库部署示例，你可以将其改造后直接用作自己的 values 文件。Chart 中没有根级的 `values.yaml`，因此不带 `-f` 的 `helm install` 会失败；请以示例文件为起点。

对于 Flux 用户，[`charts/image-patcher/examples/flux-helmrelease-example.yaml`](../../charts/image-patcher/examples/flux-helmrelease-example.yaml) 展示了一份完整的 `HelmRepository` + `HelmRelease` 清单，从集群内的 ChartMuseum 拉取该 Chart。

#### 仓库凭据

Operator 需要凭据来推送它所构建的修补镜像（以及拉取私有基础镜像）。在 `registryCredentials` 下列出每个仓库，Chart 会为你创建认证用的 Secret —— 无需手动执行 `kubectl create secret`：

```yaml
registryCredentials:
  - registry: registry.example.com
    username: pushbot
    password: ...            # 通过独立的 values 文件提供，见下文
  - registry: other-registry.example.com
    username: robot
    password: ...
```

Chart 会将这些凭据 base64 编码进单个 docker `config.json`（Secret 名为 `image-registry-secret`，key 为 `config.json`）。Kaniko（推送）和控制器的去重客户端都会读取这个 Secret，并按仓库选择匹配的条目，因此**不同的目标仓库可以使用不同的凭据**。当 `config.buildNamespace` 与 release 命名空间不同时，Chart 会将该 Secret 同时渲染到两个命名空间，使构建和控制器各自都能找到它。

请勿将密码提交到版本控制：将它们放入一个独立的、被 gitignore 忽略的 values 文件，在安装时叠加，这样只有密码来自该 secret 文件：

```sh
helm install image-patch ... -f my-values.yaml -f secret-creds.yaml
```

将 `registryCredentials` 留空（`[]`）以保留旧有行为 —— 由你自己在 release 命名空间（以及当构建命名空间不同时也在其中）创建一个名为 `image-registry-secret` 的 Secret（类型 `Opaque`，key 为 `config.json`）。可以通过 CR 的 `pushSecret`/`pullSecret` 进行按构建的覆盖（见 [Spec 字段](#spec-字段)）。

### 2. 安装 Chart

直接从已发布的 OCI Chart 安装：

```sh
helm install image-patch oci://ghcr.io/pinclr/charts/image-patcher \
  --version <version> \
  -n image-patch-system --create-namespace \
  -f my-values.yaml
```

> 该 Chart 在渲染时会断言 `--namespace image-patch-system` —— 安装到任何其他命名空间都会快速失败并给出可操作的错误信息。ClusterRoleBinding 的 subjects 和 leader-election 的 Role 都固定绑定到该命名空间，因此使用其他值会得到一个半损坏的 release。

验证：

```sh
kubectl -n image-patch-system get pods
```

### 从源码构建（备选方案）

如需运行自行构建的控制器镜像 —— 例如用于隔离网络（air-gapped）或私有仓库 —— 请自行构建并推送，然后安装本地 Chart 而非已发布的 Chart。镜像 tag 取自 `charts/image-patcher/Chart.yaml` 中的 `appVersion`，因此构建结果始终与 Chart 拉取的镜像一致：

```sh
make docker-build docker-push IMAGE_REGISTRIES="registry.example.com/myns"
helm install image-patch ./charts/image-patcher \
  -n image-patch-system --create-namespace \
  -f charts/image-patcher/examples/values-example.yaml \
  -f my-overrides.yaml
```

构建出的制品：`registry.example.com/myns/image-patcher-operator:<appVersion>`。`IMAGE_REGISTRIES` 以空格分隔，可一次发布到多个仓库；添加 `IMAGE_EXTRA_TAGS=latest` 可附加额外 tag，或覆盖 `IMAGE_REPOSITORY`/`PLATFORM` 以更改路径或目标架构。将 `my-overrides.yaml` 中的 `image.registry`（如果你覆盖了 `image.repository`，也包括它）设置为与你推送的位置一致。

### 升级

对于仅升级控制器（CRD schema 未变更）的情况：

```sh
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

如果 Chart 的 CRD 已变更（Helm 在升级时不会更新 CRD），请先应用它：

```sh
kubectl apply -f charts/image-patcher/crds/
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

### 卸载

```sh
helm uninstall image-patch -n image-patch-system
```

按照 Helm 惯例，CRD 会被保留，因此现有的 `ImagePatch` 资源不会被销毁。如有需要，可显式删除它们：

```sh
kubectl delete -f charts/image-patcher/crds/
```

### ImagePatch CRD

创建一个 `ImagePatch` 资源来构建修补后的镜像：

```yaml
apiVersion: oms.ogpu.cloud/v1alpha1
kind: ImagePatch
metadata:
  name: my-app
spec:
  baseImage: ubuntu:24.04
  targetImage: registry.example.com/my-app-patch:24.04  # 可选
  env:
    DEBIAN_FRONTEND: "noninteractive"
  apt:
    mirror: http://mirror.example.com/ubuntu  # 可选，codename 自动检测
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

#### Spec 字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `baseImage` | string | 是 | 待修补的基础镜像（例如 `ubuntu:24.04`） |
| `targetImage` | string | 否 | 目标镜像。若省略，自动生成为 `<registry>/<base-image-name>-patch:<base-tag>` |
| `pushSecret` | string | 否 | 构建命名空间中某个 Secret 的名称，其 docker config 会**替换** Chart 级别的 `image-registry-secret` 作为本次构建的推送凭据。当目标仓库需要 Chart 级 Secret 所不具备的凭据时使用。Secret 可以是 `kubernetes.io/dockerconfigjson` 类型，或携带 `config.json` key。 |
| `pullSecret` | string | 否 | 构建命名空间中某个 Secret 的名称，其 auths 会**叠加合并**到推送凭据之上，使本次构建能拉取私有基础镜像的同时仍推送到目标。Secret 形态与 `pushSecret` 相同。 |
| `env` | map[string]string | 否 | 环境变量（`ENV` 指令） |
| `apt.mirror` | string | 否 | 写入镜像 `/etc/apt/sources.list` 的 APT 镜像源 URL；Ubuntu codename 会从 `/etc/os-release` 自动检测 |
| `apt.install` | []string | 否 | 要安装的 APT 软件包 |
| `pip.install` | []string | 否 | 要安装的 pip 软件包 |
| `shell` | []ShellStep | 否 | 要执行的 shell 命令（每一项成为一个 `RUN` 层） |
| `shell[].name` | string | 否 | 步骤名称（用作 Dockerfile 注释） |
| `shell[].run` | string | 是 | shell 命令；多行命令以 `&&` 连接 |
| `shell[].workdir` | string | 否 | 本步骤的工作目录 |
| `shell[].user` | string | 否 | 以哪个用户运行本步骤 |
| `entrypoint` | []string | 否 | 容器 entrypoint |
| `cmd` | []string | 否 | 容器默认命令 |

#### 目标镜像解析

当未指定 `spec.targetImage` 时，控制器会通过解析 `spec.baseImage` 自动生成目标镜像名：

1. 若设置了 `config.defaultImageRegistry`：`<registry>/<base-image-name>-patch:<base-tag>`
2. 兜底：`<base-image-name>-patch:<base-tag>`

镜像名是 `spec.baseImage` 的最后一段（最后一个 `/` 之后），tag 是 `:` 之后的部分。若未指定 tag，则使用 `latest`。

#### 凭据优先级

默认情况下，每次构建的拉取和推送都使用 Chart 级别的 `image-registry-secret`（来自 `registryCredentials`）。每个 CR 的字段会针对单次构建覆盖这一行为：

- **`pushSecret` 是替换，而非合并。** 一旦设置，它的 docker config 会成为该次构建的全部基础，Chart 级别的 `image-registry-secret` 将被忽略 —— 因此 `pushSecret` 本身必须涵盖该构建涉及的每一个仓库（目标仓库加上基础镜像仓库，除非基础镜像是公开/镜像源提供的，或已由 `pullSecret` 覆盖）。
- **`pullSecret` 叠加合并在推送凭据之上**（推送凭据为 CR 的 `pushSecret`，若未设置则为 Chart 级默认），在按仓库冲突时以 pull 条目胜出。用它来添加私有基础镜像的凭据，而无需重述推送凭据。
- 引用一个不存在的 `pushSecret`/`pullSecret` 会使 reconcile 失败（该 Secret 必须已存在于构建命名空间），这与缺失的 Chart 级 `image-registry-secret` 不同 —— 后者只在构建 Pod 启动时才暴露问题。

> **每个仓库只有一份凭据，同时用于拉取和推送。** docker `config.json` 中每个仓库主机只有一个条目，Kaniko 对该主机的每次操作（基础镜像拉取、缓存、推送）都使用它。因此你无法为单个仓库分别提供只读和只写凭据 —— 合并后胜出的一方（冲突时为 `pullSecret` 条目）会成为该主机的**唯一**凭据。实践中这没有问题：具备推送权限的凭据也能拉取（仓库的 push scope 包含 pull），所以对于一个你既拉取又推送的仓库，使用一份具备推送权限的凭据（在 `registryCredentials` 或 `pushSecret` 中），不要为它再添加一个相互竞争的 `pullSecret` 条目。真正的读/写分离只在基础镜像来源和推送目标是**不同**仓库主机时才有效 —— 此时 `pushSecret`（目标）和 `pullSecret`（来源）落在不同的 `auths` key 上，不会冲突。

示例：`baseImage: registry.example.com/myns/ubuntu-22.04:latest` 配合 `config.defaultImageRegistry=registry.example.com/patched-images` 会生成 `registry.example.com/patched-images/ubuntu-22.04-patch:latest`。

### 测试清单

`test/k8s/` 下提供了示例清单：

- `test/k8s/sshd/` —— 简单的启用 SSH 的镜像修补
- `test/k8s/complicated/` —— 完整示例，包含 apt 镜像源、rootfs 覆盖层、supervisor、podman

```sh
kubectl apply -k test/k8s/sshd/
# 或
kubectl apply -k test/k8s/complicated/
```

## 高级功能

### 内容寻址的构建去重

控制器会为每个 `ImagePatch` spec 计算一个确定性哈希。在创建 Kaniko Job 之前，它会对仓库中的 `<repo>:dedup-<hash>` 发起 HEAD 请求；缓存命中时，它会将已有的 manifest 以用户 tag 重新打标（纯 manifest 拷贝，无需重建），并将 CR 标记为 `Succeeded`，同时设置 `Status.DedupHit=true`。未命中时，Kaniko 会一次性推送用户 tag 和 dedup tag。该功能默认启用；若你的仓库保留策略或配额规则无法容忍额外的 tag，可通过 `dedup.enabled: false` 禁用。

### Kaniko 构建缓存

设置 `kaniko.buildCache.enabled: true` 可将中间 `RUN` 层缓存到仓库中。缓存仓库会自动派生为 `<config.defaultImageRegistry>/kaniko-build-cache`。多节点安全。完整的 `kaniko.buildOptions` 调优项（`snapshotMode`、`singleSnapshot`、`ignorePaths`、`cacheTTL`）请参阅 [`charts/image-patcher/examples/values-example.yaml`](../../charts/image-patcher/examples/values-example.yaml)。

### 构建时镜像源（apt 与 PyPI）

`kaniko.buildAptMirror` 和 `kaniko.buildPypiMirror` 会在 Kaniko 构建期间将 `apt-get` 和 `pip install` 重定向到镜像源，**而不会把任何镜像源配置烘焙进产出的镜像**。适用于上游仓库缓慢或无法访问的集群：

```yaml
kaniko:
  buildAptMirror: http://mirrors.163.com/ubuntu        # 网易镜像源
  buildPypiMirror: https://pypi.tuna.tsinghua.edu.cn/simple  # 清华 TUNA 镜像源
```

它们与 `spec.apt.mirror`（后者会把镜像源 URL 烘焙进镜像的 `/etc/apt/sources.list`）互不相关。

### 仓库拉取穿透镜像

`kaniko.registryMap` 将上游仓库主机映射到拉取穿透镜像，用于基础镜像的拉取。适用于隔离网络集群或出站受阻的场景：

```yaml
kaniko:
  registryMap:
    docker.io: docker.mirror.example.com
```

仅影响基础镜像拉取；推送仍然发往真实的目标仓库。`docker.io` 在传给 Kaniko 之前会被静默规范化为 `index.docker.io`。

### 健康检查 CronJob

设置 `healthcheck.enabled: true` 可运行一个合成金丝雀 CronJob，按计划演练完整的构建流水线（基础镜像拉取 → Dockerfile 生成 → Kaniko 构建 → 仓库推送）和缓存机制。结果通过 `image_patcher_builds_total` 指标暴露。配置项请参阅 [`charts/image-patcher/examples/values-example.yaml`](../../charts/image-patcher/examples/values-example.yaml) 中的 `healthcheck.*`。

### Grafana 仪表盘

设置 `dashboards.enabled: true` 可将 Operator 的仪表盘作为 ConfigMap 打包，由 kube-prometheus-stack 的 Grafana sidecar 自动拾取。标签默认为 `grafana_dashboard=1`；若你的 sidecar 使用不同的选择器，可通过 `dashboards.label.*` 覆盖。

### Prometheus 指标

控制器在 `:8443/metrics` 上暴露 Prometheus 指标（HTTPS，通过 controller-runtime 进行认证/授权）。通过 `metrics.serviceMonitor.enabled: true` 启用 `ServiceMonitor` 自动发现。完整的指标参考请参阅 [`docs/design/metrics.md`](../../docs/design/metrics.md)。

## Chart 签名验证

已发布的 Chart 在 CI 中使用 [cosign](https://github.com/sigstore/cosign) 无密钥（keyless）方式签名 —— 签名通过 Sigstore 绑定到 GitHub Actions workflow 身份：

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/pinclr/image-patcher/.github/workflows/cd.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/pinclr/charts/image-patcher:<version>
```

## 开发

### 在本地运行控制器

用于无需重建控制器镜像的快速迭代：

```sh
make install   # 将 CRD 应用到当前 kubeconfig 上下文
make run       # 针对 ~/.kube/config 运行控制器
```

### 代码生成

在编辑 API 类型或添加 `+kubebuilder:rbac:` 标记之后：

```sh
make manifests generate    # 重新生成 CRD/RBAC 与 DeepCopy 方法
make sync-crds             # 将更新后的 CRD 同步到 Chart
```

### 测试

```sh
make test       # 单元测试 + envtest
make test-e2e   # 在 Kind 集群上进行端到端测试
make lint
```

### Chart 检查

```sh
make helm-lint       # lint chart
make helm-template   # 渲染到 /tmp/image-patcher.rendered.yaml
```

## 贡献

**注意：** 运行 `make help` 可获取所有可用 `make` target 的更多信息。

更多信息可参阅 [Kubebuilder 文档](https://book.kubebuilder.io/introduction.html)。

## 许可证

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
