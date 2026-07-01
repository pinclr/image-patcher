---
title: 开发指南
description: 在本地运行控制器、重新生成代码、运行测试、检查 Chart 并贡献代码。
---

## 在本地运行控制器

用于无需重建控制器镜像的快速迭代：

```sh
make install   # 将 CRD 应用到当前 kubeconfig 上下文
make run       # 针对 ~/.kube/config 运行控制器
```

## 代码生成

在编辑 API 类型或添加 `+kubebuilder:rbac:` 标记之后：

```sh
make manifests generate    # 重新生成 CRD/RBAC 与 DeepCopy 方法
make sync-crds             # 将更新后的 CRD 同步到 Chart
```

## 测试

```sh
make test       # 单元测试 + envtest
make test-e2e   # 在 Kind 集群上进行端到端测试
make lint
```

## Chart 检查

```sh
make helm-lint       # lint chart
make helm-template   # 渲染到 /tmp/image-patcher.rendered.yaml
```

## 贡献

运行 `make help` 可获取所有可用 `make` target 的更多信息。

更多信息可参阅 [Kubebuilder 文档](https://book.kubebuilder.io/introduction.html)。

## 许可证

Copyright 2026.

根据 Apache 许可证 2.0 版本（"许可证"）授权；未经遵守许可证，你不得使用本文件。你可以在 http://www.apache.org/licenses/LICENSE-2.0 获取许可证副本。
