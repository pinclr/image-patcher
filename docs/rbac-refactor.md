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

### Step 2 — regenerate `config/rbac/role.yaml` locally

CI runs `make sync-crds` (`.github/workflows/ci.yml:128-134`) and
**fails** the build if the checked-in `config/rbac/role.yaml` drifts
from the controller-gen output — it does not auto-commit. So run
`make manifests` locally after Step 1 and stage the regenerated file
in the same commit; otherwise the first CI run will be red.

Expected diff: the single `role.yaml` splits into a slim ClusterRole
(only `imagepatches*` rules) plus a Role keyed to
`image-patch-system`.

### Step 3 — rewrite `charts/image-patcher/templates/rbac-manager.yaml`

Replace the current single ClusterRole/ClusterRoleBinding pair with:

- A ClusterRole + ClusterRoleBinding containing only the
  `imagepatches` rules (see "Proposed split #1").
- A Role + RoleBinding in `.Release.Namespace` containing the build
  resource rules (see "Proposed split #2").

Naming convention (so the ClusterRoleBinding upgrades in place
without orphans):

| Object             | Name                                                                                  |
| ------------------ | ------------------------------------------------------------------------------------- |
| ClusterRole        | `{{ include "image-patcher.fullname" . }}-manager-role` (unchanged)                   |
| ClusterRoleBinding | `{{ include "image-patcher.fullname" . }}-manager-rolebinding` (unchanged)            |
| Role               | `{{ include "image-patcher.fullname" . }}-manager-role` (same short name, namespaced) |
| RoleBinding        | `{{ include "image-patcher.fullname" . }}-manager-rolebinding`                        |

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

## Follow-up cleanup

Two buckets: small in-scope items we can land in a tiny follow-up
commit (no behavior change), and items parked because we are
**keeping** the `BUILD_NAMESPACE` / `config.buildNamespace` knob for
forward flexibility — even though it is locked in practice today.

### In scope (small follow-up)

1. **`.gitignore`** — add `/bin/setup-envtest*` so the envtest CLI
   binaries that `make setup-envtest` drops into `bin/` stop showing
   up as untracked.
2. **Stale comment in `internal/controller/imagepatch_controller.go`
   (~lines 295-301)** — the rationale "cross-namespace cache scopes,
   which we don't enable" became inaccurate the moment we added
   `cache.Options.ByObject` in `cmd/main.go`. The conclusion (still
   need fixed-cadence requeue to observe terminal Job state) is
   correct, but the cause is now "cross-ns Jobs carry no
   OwnerReference, so `Owns(&Job{})` cannot map the event back to a
   parent CR" — the informer itself **does** see the event. Update
   the comment to match.
3. **Orphan-RBAC inventory script** — see next section.

### Operational tool — orphan RBAC cleanup script

Save as `hack/cleanup-orphan-rbac.sh`, `chmod +x` it, then run with
no flags for a dry-run report or with `--apply` to delete. It
compares "RBAC carrying this release's instance label in the cluster"
against "RBAC currently in `helm get manifest`" and reports the
difference — useful after any chart refactor that splits or renames
RBAC objects, since Helm only deletes resources it tracked at the
*previous* upgrade, so anything that drifted across multiple upgrades
in divergent ways can linger.

```bash
#!/usr/bin/env bash
#
# cleanup-orphan-rbac.sh
#
# Find RBAC objects (ClusterRole / ClusterRoleBinding / Role / RoleBinding)
# in the cluster that are labeled as part of this Helm release but no
# longer appear in `helm get manifest`. Reports candidates by default;
# pass --apply to delete them.
#
# Requirements: kubectl, helm, awk, comm. (No yq / jq.)
#
# Usage:
#   hack/cleanup-orphan-rbac.sh                       # dry-run
#   hack/cleanup-orphan-rbac.sh --apply               # delete orphans
#   hack/cleanup-orphan-rbac.sh --release my-instance # different release
#

set -euo pipefail

RELEASE="${RELEASE:-image-patcher}"
NAMESPACE="${NAMESPACE:-image-patch-system}"
APPLY=false

usage() {
  cat >&2 <<EOF
Usage: $0 [--release NAME] [--namespace NS] [--apply]

  --release NAME     Helm release name (default: $RELEASE; env: RELEASE)
  --namespace NS     Helm release namespace (default: $NAMESPACE; env: NAMESPACE)
  --apply            Delete the orphan candidates. Without this, dry-run.
  -h, --help         Show this help.

Honors current kubectl context / KUBECONFIG.
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --release)   RELEASE="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --apply)     APPLY=true; shift ;;
    -h|--help)   usage 0 ;;
    *)           echo "unknown arg: $1" >&2; usage 1 ;;
  esac
done

for bin in kubectl helm awk comm; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing dependency: $bin" >&2; exit 1; }
done

# Extract (kind \t name \t namespace) triples from the multi-doc YAML
# Helm has on record for this release. Only RBAC kinds are kept; the
# parser keys off the top-level kind: line and the metadata.name /
# metadata.namespace lines (indented exactly two spaces), which keeps
# it immune to nested name:/namespace: fields inside roleRef/subjects.
expected() {
  helm get manifest "$RELEASE" -n "$NAMESPACE" 2>/dev/null \
    | awk '
      BEGIN { reset() }
      function reset() { kind=""; name=""; ns=""; in_meta=0 }
      function emit() {
        if (kind ~ /^(ClusterRole|ClusterRoleBinding|Role|RoleBinding)$/) {
          printf "%s\t%s\t%s\n", kind, name, ns
        }
      }
      /^---/             { emit(); reset(); next }
      /^kind: /          { kind=$2; next }
      /^metadata:/       { in_meta=1; next }
      in_meta && /^  name: /      { name=$2; next }
      in_meta && /^  namespace: / { ns=$2; next }
      /^[a-zA-Z]/        { in_meta=0 }
      END                { emit() }
    ' \
    | sort -u
}

# All RBAC objects in the cluster carrying this release's instance label.
actual() {
  local sel="app.kubernetes.io/instance=${RELEASE}"
  {
    kubectl get clusterrole        -l "$sel" \
      -o jsonpath='{range .items[*]}ClusterRole{"\t"}{.metadata.name}{"\t"}{"\n"}{end}'
    kubectl get clusterrolebinding -l "$sel" \
      -o jsonpath='{range .items[*]}ClusterRoleBinding{"\t"}{.metadata.name}{"\t"}{"\n"}{end}'
    kubectl get role          -A   -l "$sel" \
      -o jsonpath='{range .items[*]}Role{"\t"}{.metadata.name}{"\t"}{.metadata.namespace}{"\n"}{end}'
    kubectl get rolebinding   -A   -l "$sel" \
      -o jsonpath='{range .items[*]}RoleBinding{"\t"}{.metadata.name}{"\t"}{.metadata.namespace}{"\n"}{end}'
  } | sort -u
}

EXP=$(expected)
ACT=$(actual)

orphans=$(comm -23 <(printf '%s\n' "$ACT") <(printf '%s\n' "$EXP") | sed '/^$/d')

if [[ -z "$orphans" ]]; then
  echo "OK: no orphan RBAC objects for release=$RELEASE"
  exit 0
fi

echo "Orphan RBAC candidates (carry release=$RELEASE label, not in helm get manifest):"
printf '  %-22s  %-50s  %s\n' KIND NAME NAMESPACE
printf '  %-22s  %-50s  %s\n' ---------------------- -------------------------------------------------- ------------------
printf '%s\n' "$orphans" | awk -F'\t' '{ ns = ($3 == "" ? "(cluster-scope)" : $3); printf "  %-22s  %-50s  %s\n", $1, $2, ns }'

if ! $APPLY; then
  echo
  echo "(dry-run -- re-run with --apply to delete the above)"
  exit 0
fi

echo
echo "Deleting orphans..."
while IFS=$'\t' read -r kind name ns; do
  [[ -z "$kind" || -z "$name" ]] && continue
  if [[ -n "$ns" ]]; then
    kubectl delete "$kind" "$name" -n "$ns"
  else
    kubectl delete "$kind" "$name"
  fi
done <<< "$orphans"
```

**Caveat**: the script keys off `app.kubernetes.io/instance=<release>`,
so it will miss anything from a kustomize-era deployment that
never carried that label. For a one-time post-migration sweep, broaden
the selector (e.g. add a second pass with `app.kubernetes.io/name=image-patcher`)
or list with no selector at all and grep by name prefix.

### Deferred — depends on locking `BUILD_NAMESPACE`

We are **keeping** the `BUILD_NAMESPACE` env / `config.buildNamespace`
values field for now, even though every production deployment targets
`image-patch-system`. The knob preserves the option of redirecting
builds to a separate namespace later (e.g. a dedicated builder ns
isolated from the operator's own ns) without a chart-breaking change.
As a consequence, the following cleanups from earlier drafts of this
doc are **parked indefinitely**; revisit only if we decide to drop
the knob:

- **Drop the `BUILD_NAMESPACE` knob end-to-end** —
  `charts/image-patcher/values.yaml` (`config.buildNamespace`),
  `charts/image-patcher/templates/deployment.yaml:67-69` env block,
  `cmd/main.go` env reads, `r.BuildNamespace` field and
  `buildNamespaceFor` method in
  `internal/controller/imagepatch_controller.go`,
  `BUILD_NAMESPACE` wiring in
  `charts/image-patcher/templates/healthcheck-cronjob.yaml`.
- **Drop the cross-namespace block in
  `charts/image-patcher/templates/healthcheck-rbac.yaml:65-98`** —
  the conditional cross-ns Job-read Role/RoleBinding becomes
  unreachable only after the knob is gone; today it is dormant in
  the default render but real for any operator that does override
  `config.buildNamespace`.

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
