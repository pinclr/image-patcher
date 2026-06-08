# RBAC Refactor — Minimize ClusterRoleBindings, Scope to `image-patch-system`

## Goal

Reduce the controller's cluster-wide permission surface. Pull every
permission that is only ever exercised in `image-patch-system` (the
build namespace) out of the ClusterRole and into a namespace-scoped
Role/RoleBinding in that namespace.

## Confirmed scope

- **ImagePatch CRs** may be created in any namespace → manager keeps
  cluster-wide watch on the CR.
- **Kaniko build resources** (Job + ConfigMap + synthesized
  docker-auth Secret + pull/push Secret reads + Pod / Pod log reads)
  always live in `image-patch-system` and there is no foreseeable
  requirement to expand beyond it.

This collapses the design: the build namespace is no longer a moving
target, so the chart does not need the cross-namespace branch that
`healthcheck-rbac.yaml` carries. A single Role/RoleBinding pair in
the release namespace is enough.

Follow-up worth confirming separately: if `config.buildNamespace` is
now effectively locked, consider either removing the value from
`values.yaml` (and the `BUILD_NAMESPACE` env wiring) or pinning it via
the values schema. Out of scope for this refactor but cheap to bundle.

## Current state

Files under `charts/image-patcher/templates/`:

| File | Kind | Resources | Reducible? |
|---|---|---|---|
| `rbac-manager.yaml` | ClusterRole + **ClusterRoleBinding** | imagepatches CR + configmaps/secrets/pods/pods\_log/jobs | **Partially** |
| `rbac-metrics-auth.yaml` | ClusterRole + **ClusterRoleBinding** | tokenreviews / subjectaccessreviews | No — cluster-scoped resources; API only accepts ClusterRoleBinding |
| `rbac-metrics-reader.yaml` | ClusterRole (bound only when Prometheus enabled) | nonResourceURL `/metrics` | No — nonResourceURL requires ClusterRole |
| `rbac-prometheus-metrics-reader.yaml` | **ClusterRoleBinding** | Binds Prometheus SA to the metrics-reader ClusterRole | No — Prometheus SA usually lives in another ns and target CR has nonResourceURL |
| `rbac-imagepatch-aggregated.yaml` | 3 ClusterRoles (no binding) | admin/editor/viewer aggregation for ImagePatch | Unrelated to bindings, keep |
| `rbac-leader-election.yaml` | Role + RoleBinding (release ns) | configmaps / leases / events | Already namespace-scoped |
| `healthcheck-rbac.yaml` | Role + RoleBinding | imagepatches CR + jobs read | Already namespace-scoped |

Only `rbac-manager.yaml` is reducible.

## Where the controller actually touches each resource

Traced through `internal/controller/imagepatch_controller.go`:

- **ConfigMap / Job / synthesized docker-auth Secret writes** —
  always created in `buildNs` (= `r.BuildNamespace`, default
  `image-patch-system`). The CR's own namespace is never written.
  See `createOrUpdateConfigMap` (584), `createOrUpdateAuthSecret`
  (729), Job creation path.
- **pullSecret / pushSecret reads** — read from `buildNs`, not the
  CR's namespace (`pullDockerConfig` at 707, `createOrUpdateAuthSecret`
  at 737/745).
- **pods + pods/log reads** — used by `classifyBuildFailure`; the
  build Pod is always a child of a Job in `buildNs`, so reads are
  confined to `buildNs`.
- **ImagePatch CR** — users create CRs in arbitrary namespaces, and
  the manager watches them cluster-wide. **Must** stay in a
  ClusterRole+ClusterRoleBinding.

Conclusion: the current cluster-wide grants on
`configmaps/secrets/pods/pods/log/jobs` are over-broad — every actual
call site is inside `buildNamespace`.

## Proposed split

### 1. A slim ClusterRole + ClusterRoleBinding — CR only

```yaml
rules:
  - apiGroups: ["oms.ogpu.cloud"]
    resources: ["imagepatches"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["oms.ogpu.cloud"]
    resources: ["imagepatches/finalizers"]
    verbs: ["update"]
  - apiGroups: ["oms.ogpu.cloud"]
    resources: ["imagepatches/status"]
    verbs: ["get", "patch", "update"]
```

Note: `create` and `delete` on `imagepatches` are in the current
kubebuilder marker scaffold but the controller does not exercise them
(it only `Get`/`Update`/patches status + finalizer). Drop them as
part of the tighten-up; keep the kubebuilder markers in sync.

### 2. A Role + RoleBinding in the release namespace

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "image-patcher.fullname" . }}-manager-role
  namespace: {{ .Release.Namespace }}
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "image-patcher.fullname" . }}-manager-rolebinding
  namespace: {{ .Release.Namespace }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "image-patcher.fullname" . }}-manager-role
subjects:
  - kind: ServiceAccount
    name: {{ include "image-patcher.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
```

No cross-namespace conditional — build namespace is fixed to the
release namespace by design.

## Required code change — manager cache scoping

`SetupWithManager` already declares:

```go
.Owns(&batchv1.Job{}).
.Owns(&corev1.ConfigMap{}).
.Owns(&corev1.Secret{}).
```

`Owns(...)` unconditionally starts a cluster-wide informer at manager
boot. The existing comment in `rbac-manager.yaml` documents the
failure mode: without cluster-wide list/watch, "informer 403s, cache
sync times out, and the controller exits before reconciling
anything."

So the Helm changes alone are not safe. `cmd/main.go` must restrict
the cache for ConfigMap / Secret / Job to the build namespace:

```go
const buildNs = "image-patch-system"

mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    Scheme:                 scheme,
    Metrics:                metricsServerOptions,
    WebhookServer:          webhookServer,
    HealthProbeBindAddress: probeAddr,
    LeaderElection:         enableLeaderElection,
    LeaderElectionID:       "da16e050.oms.ogpu.cloud",
    Cache: cache.Options{
        ByObject: map[client.Object]cache.ByObject{
            &corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{buildNs: {}}},
            &corev1.Secret{}:    {Namespaces: map[string]cache.Config{buildNs: {}}},
            &batchv1.Job{}:      {Namespaces: map[string]cache.Config{buildNs: {}}},
        },
    },
})
```

`ImagePatch` stays cluster-wide (no entry in `ByObject` ⇒ default
behaviour). `pods/log` goes through the typed `kubernetes.Interface`
clientset, not the cached client, so no cache change is needed there.

If we keep the `BUILD_NAMESPACE` env for now (lower-blast-radius
option), source `buildNs` from the env with `image-patch-system` as the
default — the cache scoping shape is identical.

## Net effect

- **ClusterRoleBinding count**: 3 → 3 (the metrics ones cannot be
  removed; the manager one shrinks but stays). Cannot go lower without
  giving up Prometheus auth or the metrics endpoint.
- **Manager cluster-wide permission surface**: 6 resource families
  (configmaps, secrets, pods, pods/log, jobs, imagepatches) →
  1 (imagepatches). Everything else is confined to
  `image-patch-system`.

## Implementation steps

These are the manual edits a human makes on the branch. `make
manifests` / `make test` / chart render / cluster smoke test all run
under the existing GitHub Actions workflow once the branch is pushed —
do not run them locally.

Land the code and manifest changes in one PR — applying the chart
change without the manager cache change will brick the controller at
boot, so they cannot ship separately.

### Step 1 — update kubebuilder RBAC markers

In `internal/controller/imagepatch_controller.go` (around line 104),
replace the current block with a CR-only cluster marker plus
namespaced markers for the build resources:

```go
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches/finalizers,verbs=update

// +kubebuilder:rbac:groups=batch,resources=jobs,namespace=image-patch-system,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups="",resources=configmaps,namespace=image-patch-system,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups="",resources=secrets,namespace=image-patch-system,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups="",resources=pods,namespace=image-patch-system,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/log,namespace=image-patch-system,verbs=get
```

Drop `create` / `delete` on `imagepatches`; the controller never
calls them.

### Step 2 — let CI regenerate `config/rbac/role.yaml`

The repo's workflow runs `make manifests` and commits the result back
(or fails the build if the checked-in file drifts). After pushing
Step 1, expect the regenerated `config/rbac/role.yaml` to split into
a slim ClusterRole + a Role keyed to `image-patch-system`. Sanity-
check that diff in the PR; if `make manifests` is *not* wired into
CI for this repo, regenerate locally before pushing the follow-up
commit.

### Step 3 — rewrite `charts/image-patcher/templates/rbac-manager.yaml`

Replace the current single ClusterRole/ClusterRoleBinding pair with:

- A ClusterRole + ClusterRoleBinding containing only the
  `imagepatches` rules (see "Proposed split #1").
- A Role + RoleBinding in `.Release.Namespace` containing the build
  resource rules (see "Proposed split #2").

Naming convention (so the ClusterRoleBinding upgrades in place
without orphans):

| Object             | Name                                                                       |
| ------------------ | -------------------------------------------------------------------------- |
| ClusterRole        | `{{ include "image-patcher.fullname" . }}-manager-role` (unchanged)        |
| ClusterRoleBinding | `{{ include "image-patcher.fullname" . }}-manager-rolebinding` (unchanged) |
| Role               | `{{ include "image-patcher.fullname" . }}-manager-role` (same short name, namespaced) |
| RoleBinding        | `{{ include "image-patcher.fullname" . }}-manager-rolebinding`             |

Refresh the in-file comments — the long secrets/pods rationale in the
current ClusterRole no longer fits the cluster-scoped one (it now
applies to the namespaced Role).

### Step 4 — restrict the manager cache in `cmd/main.go`

Add the cache scoping to `ctrl.NewManager` as shown in "Required code
change — manager cache scoping" above. New imports needed:

```go
batchv1 "k8s.io/api/batch/v1"
corev1 "k8s.io/api/core/v1"
"sigs.k8s.io/controller-runtime/pkg/cache"
"sigs.k8s.io/controller-runtime/pkg/client"
```

Keep `BuildNamespace: os.Getenv("BUILD_NAMESPACE")` on the reconciler
struct in this PR — controller-side cleanup is the follow-up.

### Step 5 — push and review CI output

After pushing the branch, the existing GitHub Actions workflow runs
unit tests, envtest, chart lint, and any e2e job. Things to check on
the PR before merge:

- envtest passes — the suite already creates `image-patch-system`
  (`internal/controller/suite_test.go:94`), so the namespaced Role is
  sufficient for the test SA.
- The chart-rendered diff in CI shows: the existing ClusterRole
  shrinks (verbs only on `imagepatches*`); the existing
  ClusterRoleBinding name stays; a new Role + RoleBinding appear in
  the release namespace.
- The e2e / install job (if present) brings the controller up
  cleanly — a failure here is the early-warning that the cache scope
  and the Role rules don't agree.

If a kind-based smoke run is not part of the workflow, manually
create an `ImagePatch` in a non-`image-patch-system` namespace
against the PR's preview deploy and confirm the Kaniko Job lands in
`image-patch-system` and the CR reaches `Succeeded`.

### Step 6 — upgrade safety

For existing deployments running the old chart, `helm upgrade` will:

- Shrink the existing ClusterRole's rules in place (same name).
- Leave the existing ClusterRoleBinding alone (same name, same
  roleRef target).
- Create the new Role + RoleBinding alongside.

No orphaned RBAC objects; rollback is a chart re-install of the prior
version.

## Follow-up cleanup (separate PR)

Once the new RBAC is in production and stable, the following are
dead-weight and can be removed:

### A. Drop the `BUILD_NAMESPACE` knob end-to-end

The build namespace is now locked to `image-patch-system`. The env
variable, values field, and reconciler field exist only to express a
configurability we no longer offer.

- `charts/image-patcher/values.yaml` — remove the `config.buildNamespace`
  field and its comment block (lines 21-42).
- `charts/image-patcher/templates/deployment.yaml:67-69` — remove
  the `BUILD_NAMESPACE` env block.
- `cmd/main.go:240` — remove the `BuildNamespace` field assignment.
- `internal/controller/imagepatch_controller.go` —
  - delete `r.BuildNamespace` field (57-69) and the godoc above it;
  - delete `buildNamespaceFor` (559-568);
  - replace call sites (line 132) with `defaultBuildNamespace`.
- `internal/controller/imagepatch_controller.go:516` —
  `defaultBuildNamespace` becomes the single source of truth. Consider
  promoting it to a package-level export so `cmd/main.go` can reuse it
  for the cache config instead of repeating the string literal.

### B. Drop the cross-namespace branch in `healthcheck-rbac.yaml`

`charts/image-patcher/templates/healthcheck-rbac.yaml:65-98` is the
cross-ns Job-read pair guarded by
`{{- if and .Values.config.buildNamespace (ne .Values.config.buildNamespace .Release.Namespace) }}`.
Once `config.buildNamespace` is gone, that branch is unreachable —
delete it and the surrounding `{{- end }}`.

### C. Tests — drop the cross-ns Job-read coverage if any

Search `internal/controller/*_test.go` and `test/e2e/` for cases that
exercise `BuildNamespace != CR.Namespace` *with* a third namespace
involved (i.e. not just "CR in user ns, build in image-patch-system",
which is the common case and **still applies** because the CR can
live anywhere). If the only divergence is CR-ns vs build-ns, the
crossNamespace code path in the reconciler is unchanged and the tests
stay.

### Not cleanup-able

- The `crossNamespace` branches inside `imagepatch_controller.go`
  (lines 133, 290, 386, 600, 767, etc.) — these handle the case
  where the CR is in a user namespace but the build is in
  `image-patch-system`, which is now the *default* path, not the
  exception. They stay.
- The reconciler's `Owns(&corev1.Secret{} / ConfigMap / Job)` —
  these are still correct; the cache.Options scoping makes them only
  observe events in `image-patch-system`, which matches the
  controller-created objects' namespace.
