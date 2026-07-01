---
title: Development Guide
description: Run the controller locally, regenerate code, run tests, inspect the chart, and contribute.
---

## Run the Controller Locally

For quick iteration without rebuilding the controller image:

```sh
make install   # apply CRDs to your current kubeconfig context
make run       # run the controller against ~/.kube/config
```

## Code Generation

After editing API types or adding `+kubebuilder:rbac:` markers:

```sh
make manifests generate    # regenerate CRDs/RBAC and DeepCopy methods
make sync-crds             # propagate updated CRDs to the chart
```

## Tests

```sh
make test       # unit + envtest
make test-e2e   # end-to-end on a Kind cluster
make lint
```

## Chart Inspection

```sh
make helm-lint       # lint chart
make helm-template   # render to /tmp/image-patcher.rendered.yaml
```

## Contributing

Run `make help` for more information on all potential `make` targets.

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html).

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0.
