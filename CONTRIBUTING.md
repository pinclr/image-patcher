# Contributing to image-patcher

Thanks for your interest in contributing! This document covers how to build,
test, and submit changes to image-patch-operator and its Helm chart.

By participating in this project you agree to abide by our
[Code of Conduct](./CODE_OF_CONDUCT.md).

## Ways to contribute

- Report bugs and request features via [GitHub Issues](https://github.com/pinclr/image-patcher/issues).
- Improve documentation (the repo `README.md` and the chart `charts/image-patcher/README.md`).
- Submit code via pull requests (see below).

For security vulnerabilities, **do not** open a public issue — follow
[SECURITY.md](./SECURITY.md) instead.

## Development environment

Prerequisites:

- Go (version per `go.mod`)
- Docker (or another `CONTAINER_TOOL`)
- `kubectl`, and access to a cluster (kind/minikube is fine) for end-to-end runs
- `make` — run `make help` to list all targets

This is a [Kubebuilder](https://book.kubebuilder.io/) project; the API types
live under `api/`, the controller under `internal/controller/`, and the Helm
chart under `charts/image-patcher/`.

### Common tasks

```sh
make build          # compile the manager binary
make test           # run unit tests (envtest binaries are fetched automatically)
make lint           # golangci-lint
make manifests      # regenerate CRDs/RBAC after changing +kubebuilder markers
make generate       # regenerate deepcopy code after changing API types
```

If you change API types or RBAC markers, run `make manifests generate` and
commit the regenerated output — CI enforces that generated files are in sync
(CRD-drift check).

### Working on the Helm chart

```sh
helm lint charts/image-patcher -n image-patch-system --set image.registry=example.com
helm template t charts/image-patcher -n image-patch-system --set image.registry=example.com
```

The chart asserts the `image-patch-system` namespace at render time, so pass
`-n image-patch-system` when linting/templating. Bump
`charts/image-patcher/Chart.yaml` `version` for any chart change; bump
`appVersion` only when the controller image changes.

## Pull request process

1. Fork and create a topic branch.
2. Make your change, with tests where it makes sense.
3. Ensure `make test lint` and `helm lint` pass locally.
4. Keep the PR focused; describe the what and why in the description.
5. All commits must be **signed off** (DCO, see below).
6. A maintainer will review; address feedback by pushing follow-up commits.

### Developer Certificate of Origin (DCO)

We require a [DCO](https://developercertificate.org/) sign-off on every
commit, certifying you have the right to submit the contribution under the
project's license. Add it with `-s`:

```sh
git commit -s -m "your message"
```

This appends a `Signed-off-by: Your Name <your@email>` trailer. Configure
`git config user.name` / `user.email` to match.

### Commit messages

- Use a concise, imperative subject line (e.g. "fix cross-namespace cleanup").
- Reference issues/tickets where relevant.
- Explain *why* in the body when the change isn't obvious.

## License

By contributing, you agree that your contributions will be licensed under the
[Apache License 2.0](./LICENSE).
