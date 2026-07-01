---
title: 高级功能
description: 构建去重、Kaniko 缓存、构建时镜像源、仓库拉取穿透、健康检查 CronJob、Grafana 仪表盘、Prometheus 指标和 Chart 签名验证。
---

## 内容寻址的构建去重

控制器会为每个 `ImagePatch` spec 计算一个确定性哈希。在创建 Kaniko Job 之前，它会对仓库中的 `<repo>:dedup-<hash>` 发起 HEAD 请求：

- **缓存命中：** 将已有的 manifest 以用户 tag 重新打标（纯 manifest 拷贝，无需重建），并将 CR 标记为 `Succeeded`，同时设置 `Status.DedupHit=true`。
- **缓存未命中：** Kaniko 会一次性推送用户 tag 和 dedup tag。

该功能默认启用。若你的仓库保留策略或配额规则无法容忍额外的 tag，可通过 `dedup.enabled: false` 禁用。

## Kaniko 构建缓存

设置 `kaniko.buildCache.enabled: true` 可将中间 `RUN` 层缓存到仓库中。缓存仓库会自动派生为 `<config.defaultImageRegistry>/kaniko-build-cache`。多节点安全。

完整的 `kaniko.buildOptions` 调优项（`snapshotMode`、`singleSnapshot`、`ignorePaths`、`cacheTTL`）请参阅 `charts/image-patcher/examples/values-example.yaml`。

## 构建时镜像源（apt 与 PyPI）

`kaniko.buildAptMirror` 和 `kaniko.buildPypiMirror` 会在 Kaniko 构建期间将 `apt-get` 和 `pip install` 重定向到镜像源，**而不会把任何镜像源配置烘焙进产出的镜像**。适用于上游仓库缓慢或无法访问的集群：

```yaml
kaniko:
  buildAptMirror: http://mirrors.163.com/ubuntu        # 网易镜像源
  buildPypiMirror: https://pypi.tuna.tsinghua.edu.cn/simple  # 清华 TUNA 镜像源
```

:::note
它们与 `spec.apt.mirror`（后者会把镜像源 URL 永久烘焙进镜像的 `/etc/apt/sources.list`）互不相关。
:::

## 仓库拉取穿透镜像

`kaniko.registryMap` 将上游仓库主机映射到拉取穿透镜像，用于基础镜像的拉取。适用于隔离网络集群或出站受阻的场景：

```yaml
kaniko:
  registryMap:
    docker.io: docker.mirror.example.com
```

仅影响基础镜像拉取；推送仍然发往真实的目标仓库。`docker.io` 在传给 Kaniko 之前会被静默规范化为 `index.docker.io`。

## 健康检查 CronJob

设置 `healthcheck.enabled: true` 可运行一个合成金丝雀 CronJob，按计划演练完整的构建流水线（基础镜像拉取 → Dockerfile 生成 → Kaniko 构建 → 仓库推送）和缓存机制。结果通过 `image_patcher_builds_total` 指标暴露。

配置项请参阅 `charts/image-patcher/examples/values-example.yaml` 中的 `healthcheck.*`。

## Grafana 仪表盘

设置 `dashboards.enabled: true` 可将 Operator 的仪表盘作为 ConfigMap 打包，由 kube-prometheus-stack 的 Grafana sidecar 自动拾取。标签默认为 `grafana_dashboard=1`；若你的 sidecar 使用不同的选择器，可通过 `dashboards.label.*` 覆盖。

## Prometheus 指标

控制器在 `:8443/metrics` 上暴露 Prometheus 指标（HTTPS，通过 controller-runtime 进行认证/授权）。通过 `metrics.serviceMonitor.enabled: true` 启用 `ServiceMonitor` 自动发现。

完整的指标参考请参阅 `docs/design/metrics.md`。

## Chart 签名验证

已发布的 Chart 在 CI 中使用 [cosign](https://github.com/sigstore/cosign) 无密钥（keyless）方式签名 —— 签名通过 Sigstore 绑定到 GitHub Actions workflow 身份：

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/pinclr/image-patcher/.github/workflows/cd.yml@.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/pinclr/charts/image-patcher:<version>
```
