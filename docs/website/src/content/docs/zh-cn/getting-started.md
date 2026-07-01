---
title: 快速开始
description: 使用 Helm 在 Kubernetes 集群上安装 image-patch-operator。
---

## 前置条件

- kubectl v1.21+
- Kubernetes v1.21+ 集群
- Helm v3.8+（支持 OCI）
- 一个容器镜像仓库，供 Operator 推送它所构建的**修补后**镜像
  （控制器镜像和 Chart 本身是公开发布的）

## 1. 准备 Values 文件

`image.registry` 是必填项（该 Chart 绝不会静默地从 docker.io 拉取镜像）。将其指向某个公开镜像仓库，并设置 Operator 应将其构建的镜像推送到何处：

```yaml
image:
  registry: ghcr.io/pinclr        # 或 quay.io/pinclr

config:
  defaultImageRegistry: registry.example.com/patched-images   # 构建镜像的目标地址；如果每个 ImagePatch 都设置了 spec.targetImage 则可选
```

`image.repository` 默认为 `image-patcher-operator`，`image.tag` 默认为 Chart 的 `appVersion`，因此上面的配置会解析为 `ghcr.io/pinclr/image-patcher-operator:<appVersion>`。完整且带注释的默认值集合位于 `charts/image-patcher/examples/values-example.yaml`。Chart 中没有根级的 `values.yaml`，因此不带 `-f` 的 `helm install` 会失败；请以示例文件为起点。

对于 Flux 用户，`charts/image-patcher/examples/flux-helmrelease-example.yaml` 展示了一份完整的 `HelmRepository` + `HelmRelease` 清单，从集群内的 ChartMuseum 拉取该 Chart。

### 仓库凭据

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

Chart 会将这些凭据 base64 编码进单个 docker `config.json`（Secret 名为 `image-registry-secret`，key 为 `config.json`）。Kaniko（推送）和控制器的去重客户端都会读取这个 Secret，并按仓库选择匹配的条目，因此**不同的目标仓库可以使用不同的凭据**。

请勿将密码提交到版本控制 —— 将它们放入一个独立的、被 gitignore 忽略的 values 文件，在安装时叠加：

```sh
helm install image-patch ... -f my-values.yaml -f secret-creds.yaml
```

将 `registryCredentials` 留空（`[]`）以保留旧有行为，由你自己创建 Secret。可以通过 CR 的 `pushSecret`/`pullSecret` 进行按构建的覆盖（见 [CRD 参考](/zh-cn/crd-reference/)）。

## 2. 安装 Chart

直接从已发布的 OCI Chart 安装：

```sh
helm install image-patch oci://ghcr.io/pinclr/charts/image-patcher \
  --version <version> \
  -n image-patch-system --create-namespace \
  -f my-values.yaml
```

:::note
该 Chart 在渲染时会断言 `--namespace image-patch-system` —— 安装到任何其他命名空间都会快速失败并给出可操作的错误信息。ClusterRoleBinding 的 subjects 和 leader-election 的 Role 都固定绑定到该命名空间。
:::

验证：

```sh
kubectl -n image-patch-system get pods
```

## 从源码构建（备选方案）

如需运行自行构建的控制器镜像 —— 例如用于隔离网络（air-gapped）或私有仓库 —— 请自行构建并推送，然后安装本地 Chart：

```sh
make docker-build docker-push IMAGE_REGISTRIES="registry.example.com/myns"
helm install image-patch ./charts/image-patcher \
  -n image-patch-system --create-namespace \
  -f charts/image-patcher/examples/values-example.yaml \
  -f my-overrides.yaml
```

构建出的制品：`registry.example.com/myns/image-patcher-operator:<appVersion>`。`IMAGE_REGISTRIES` 以空格分隔，可一次发布到多个仓库。将 `my-overrides.yaml` 中的 `image.registry` 设置为与你推送的位置一致。

## 升级

对于仅升级控制器（CRD schema 未变更）的情况：

```sh
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

如果 Chart 的 CRD 已变更（Helm 在升级时不会更新 CRD），请先应用它：

```sh
kubectl apply -f charts/image-patcher/crds/
helm upgrade image-patch ./charts/image-patcher -n image-patch-system -f my-values.yaml
```

## 卸载

```sh
helm uninstall image-patch -n image-patch-system
```

按照 Helm 惯例，CRD 会被保留，因此现有的 `ImagePatch` 资源不会被销毁。如有需要，可显式删除它们：

```sh
kubectl delete -f charts/image-patcher/crds/
```

## 测试清单

`test/k8s/` 下提供了示例清单：

- `test/k8s/sshd/` —— 简单的启用 SSH 的镜像修补
- `test/k8s/complicated/` —— 完整示例，包含 apt 镜像源、rootfs 覆盖层、supervisor、podman

```sh
kubectl apply -k test/k8s/sshd/
# 或
kubectl apply -k test/k8s/complicated/
```
