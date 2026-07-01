---
title: CRD Reference
description: Full reference for the ImagePatch custom resource definition, spec fields, and credential behaviour.
---

## ImagePatch CRD

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

## Spec Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `baseImage` | string | yes | Base image to patch (e.g. `ubuntu:24.04`) |
| `targetImage` | string | no | Destination image. If omitted, auto-generated as `<registry>/<base-image-name>-patch:<base-tag>` |
| `pushSecret` | string | no | Name of a Secret in the build namespace whose docker config **replaces** the chart-level `image-registry-secret` as the push creds for this build. Secret may be `kubernetes.io/dockerconfigjson` or carry a `config.json` key. |
| `pullSecret` | string | no | Name of a Secret in the build namespace whose auths are **merged on top** of the push creds, so this build can pull a private base image while still pushing to the target. Same Secret shapes as `pushSecret`. |
| `env` | map[string]string | no | Environment variables (`ENV` directives) |
| `apt.mirror` | string | no | APT mirror URL baked into the image's `/etc/apt/sources.list`; Ubuntu codename is auto-detected from `/etc/os-release` |
| `apt.install` | []string | no | APT packages to install |
| `pip.install` | []string | no | pip packages to install |
| `shell` | []ShellStep | no | Shell commands to run (each becomes a `RUN` layer) |
| `shell[].name` | string | no | Step name (used as Dockerfile comment) |
| `shell[].run` | string | yes | Shell commands; multi-line commands are joined with `&&` |
| `shell[].workdir` | string | no | Working directory for this step |
| `shell[].user` | string | no | User to run this step as |
| `entrypoint` | []string | no | Container entrypoint |
| `cmd` | []string | no | Container default command |

## Target Image Resolution

When `spec.targetImage` is not specified, the controller auto-generates the target image name by parsing `spec.baseImage`:

1. If `config.defaultImageRegistry` is set: `<registry>/<base-image-name>-patch:<base-tag>`
2. Fallback: `<base-image-name>-patch:<base-tag>`

The image name is the last segment of `spec.baseImage` (after the last `/`), and the tag is extracted after `:`. If no tag is specified, `latest` is used.

**Example:** `baseImage: registry.example.com/myns/ubuntu-22.04:latest` with `config.defaultImageRegistry=registry.example.com/patched-images` produces `registry.example.com/patched-images/ubuntu-22.04-patch:latest`.

## Credential Precedence

By default every build uses the chart-level `image-registry-secret` (from `registryCredentials`) for both pull and push. The per-CR fields override this for a single build:

- **`pushSecret` replaces, it does not merge.** When set, its docker config becomes the entire base for that build and the chart-level `image-registry-secret` is ignored — so `pushSecret` must itself carry every registry the build touches.
- **`pullSecret` merges on top** of the push creds (CR `pushSecret` if set, else the chart-level default), with the pull entry winning on per-registry conflicts. Use it to add private base-image creds without restating the push creds.
- Referencing a missing `pushSecret`/`pullSecret` fails the reconcile immediately (the Secret must already exist in the build namespace).

:::caution[One credential per registry]
A docker `config.json` has a single entry per registry host, and Kaniko uses it for every operation on that host (base-image pull, cache, and push). You cannot give a single registry separate read-only and write-only credentials — whichever wins the merge becomes *the* credential for that host.

In practice, a push-capable credential can also pull. Genuine read/write separation only works when the base-image source and the push target are **different** registry hosts.
:::
