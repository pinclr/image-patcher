---
title: CRD 参考
description: ImagePatch 自定义资源定义的完整参考，包括 spec 字段和凭据行为。
---

## ImagePatch CRD

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

## Spec 字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `baseImage` | string | 是 | 待修补的基础镜像（例如 `ubuntu:24.04`） |
| `targetImage` | string | 否 | 目标镜像。若省略，自动生成为 `<registry>/<base-image-name>-patch:<base-tag>` |
| `pushSecret` | string | 否 | 构建命名空间中某个 Secret 的名称，其 docker config 会**替换** Chart 级别的 `image-registry-secret` 作为本次构建的推送凭据。Secret 可以是 `kubernetes.io/dockerconfigjson` 类型，或携带 `config.json` key。 |
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

## 目标镜像解析

当未指定 `spec.targetImage` 时，控制器会通过解析 `spec.baseImage` 自动生成目标镜像名：

1. 若设置了 `config.defaultImageRegistry`：`<registry>/<base-image-name>-patch:<base-tag>`
2. 兜底：`<base-image-name>-patch:<base-tag>`

镜像名是 `spec.baseImage` 的最后一段（最后一个 `/` 之后），tag 是 `:` 之后的部分。若未指定 tag，则使用 `latest`。

**示例：** `baseImage: registry.example.com/myns/ubuntu-22.04:latest` 配合 `config.defaultImageRegistry=registry.example.com/patched-images` 会生成 `registry.example.com/patched-images/ubuntu-22.04-patch:latest`。

## 凭据优先级

默认情况下，每次构建的拉取和推送都使用 Chart 级别的 `image-registry-secret`（来自 `registryCredentials`）。每个 CR 的字段会针对单次构建覆盖这一行为：

- **`pushSecret` 是替换，而非合并。** 一旦设置，它的 docker config 会成为该次构建的全部基础，Chart 级别的 `image-registry-secret` 将被忽略 —— 因此 `pushSecret` 本身必须涵盖该构建涉及的每一个仓库。
- **`pullSecret` 叠加合并在推送凭据之上**（推送凭据为 CR 的 `pushSecret`，若未设置则为 Chart 级默认），在按仓库冲突时以 pull 条目胜出。
- 引用一个不存在的 `pushSecret`/`pullSecret` 会使 reconcile 立即失败（该 Secret 必须已存在于构建命名空间）。

:::caution[每个仓库只有一份凭据]
docker `config.json` 中每个仓库主机只有一个条目，Kaniko 对该主机的每次操作（基础镜像拉取、缓存、推送）都使用它。因此你无法为单个仓库分别提供只读和只写凭据。

在实践中，具备推送权限的凭据也能拉取。真正的读/写分离只在基础镜像来源和推送目标是**不同**仓库主机时才有效。
:::
