/*
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
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
	"image-patch-operator/internal/metrics"
)

// ImagePatchReconciler reconciles a ImagePatch object
type ImagePatchReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	DefaultRegistry string
	KanikoImage     string
	// BuildNamespace, when set, is where Kaniko build Jobs and their
	// supporting ConfigMaps are created — regardless of which namespace the
	// ImagePatch CR itself lives in. Empty preserves the legacy
	// same-namespace behavior. Cross-namespace OwnerReferences are not
	// allowed by Kubernetes, so when BuildNamespace differs from the CR's
	// namespace the controller skips SetControllerReference and instead
	// stamps the Job / ConfigMap with labels:
	//   imagepatch.source.name
	//   imagepatch.source.namespace
	// for traceability. Cleanup on CR deletion becomes the user's
	// responsibility in that mode.
	BuildNamespace string
	// KanikoPullCachePVC, when set, is the name of a PVC (in the same
	// namespace as the ImagePatch) the controller mounts into every Kaniko
	// build Job at KanikoPullCacheMountPath. Kaniko is then invoked with
	// --cache-dir=<mountPath> so pulled base-image layers persist across
	// builds. The PVC is managed by the chart (or pre-provisioned by an
	// admin) — its lifecycle is independent of any single ImagePatch.
	KanikoPullCachePVC       string
	KanikoPullCacheMountPath string
	// KanikoBuildCacheRepo, when set, is passed to Kaniko as --cache-repo
	// for the build/layer cache (intermediate RUN steps cached in a
	// container registry). Independent from the local pull cache PVC above;
	// both can be enabled at once.
	KanikoBuildCacheRepo string
	// DefaultBuildOptions are chart-wide defaults for Kaniko snapshot /
	// cache tuning. Each field on the CR's spec.buildOptions overrides
	// the matching field here; if both are empty the corresponding flag
	// is omitted entirely and Kaniko applies its own default.
	DefaultBuildOptions omsv1alpha1.BuildOptions
	// KanikoResources is applied to every Kaniko build container. The
	// critical field is requests.ephemeral-storage: large base
	// extractions can use 15-25 GiB on the node's container rootfs,
	// and without a request the scheduler is blind to that demand and
	// may stack builds on a disk-tight node, triggering node-level
	// disk-pressure eviction. Sourced from KANIKO_RESOURCES env (JSON
	// of corev1.ResourceRequirements) emitted by the chart deployment.
	KanikoResources corev1.ResourceRequirements
}

// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.0/pkg/reconcile
func (r *ImagePatchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var imagePatch omsv1alpha1.ImagePatch
	if err := r.Get(ctx, req.NamespacedName, &imagePatch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	jobName := imagePatch.Name + "-build-job"
	cmName := imagePatch.Name + "-dockerfile"
	buildNs := r.buildNamespaceFor(&imagePatch)
	crossNamespace := buildNs != imagePatch.Namespace

	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: buildNs}, &job)
	if err == nil {
		return r.handleExistingJob(ctx, &imagePatch, &job)
	}

	if errors.IsNotFound(err) {
		l.Info("Creating new build resources", "ImagePatch", imagePatch.Name, "buildNamespace", buildNs)

		dockerfileContent := GenerateDockerfile(&imagePatch)
		l.V(1).Info("Generated Dockerfile", "content", dockerfileContent)

		if err := r.createOrUpdateConfigMap(ctx, &imagePatch, cmName, buildNs, dockerfileContent); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonConfigMapApply)
			return ctrl.Result{}, err
		}

		destination := r.resolveDestination(&imagePatch)
		buildOpts := mergeBuildOptions(r.DefaultBuildOptions, imagePatch.Spec.BuildOptions)
		j := constructJob(&imagePatch, jobName, cmName, buildNs, destination, r.KanikoImage,
			r.KanikoPullCachePVC, r.KanikoPullCacheMountPath, r.KanikoBuildCacheRepo,
			r.KanikoResources, buildOpts)
		// Owner references must live in the same namespace as the dependent
		// (Kubernetes GC rejects cross-namespace ownership). When the build
		// namespace matches the CR's namespace, set the controller reference
		// for automatic cleanup; otherwise rely on the source labels for
		// traceability and leave cleanup to the user.
		if !crossNamespace {
			if err := controllerutil.SetControllerReference(&imagePatch, j, r.Scheme); err != nil {
				metrics.RecordReconcileFailure(metrics.ReasonOwnerRef)
				return ctrl.Result{}, err
			}
		}
		if err := r.Create(ctx, j); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonJobCreate)
			return ctrl.Result{}, err
		}

		imagePatch.Status.Phase = "Running"
		imagePatch.Status.JobName = jobName
		imagePatch.Status.Image = destination
		if err := r.Status().Update(ctx, &imagePatch); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonStatusUpdate)
			l.Error(err, "Failed to update ImagePatch status to Running")
			return ctrl.Result{}, err
		}

		l.Info("Job created successfully", "job", jobName)
		// The newly created Job is in another namespace whenever
		// BuildNamespace differs from the CR's namespace. Owns(&Job{}) in
		// SetupWithManager only delivers events for Jobs in namespaces the
		// manager watches with cross-namespace cache scopes, which we
		// don't enable -- so the controller would otherwise never see the
		// terminal Job transition. Requeue at a fixed cadence to poll the
		// Job's status. Cheap: cached client List, no API round-trip.
		return ctrl.Result{RequeueAfter: runningPhaseRequeueAfter}, nil
	}

	metrics.RecordReconcileFailure(metrics.ReasonGetJob)
	return ctrl.Result{}, err
}

// runningPhaseRequeueAfter is the period at which the reconciler re-checks
// an in-flight build when its Job lives in a different namespace than the
// CR. Short enough that the canary's 10-minute timeout has plenty of room
// to observe completion; long enough that idle Pods don't churn through
// the workqueue. Same cadence applies to the Pending case (Job created,
// no status counters yet).
const runningPhaseRequeueAfter = 15 * time.Second

// handleExistingJob handles the case when the Job already exists
func (r *ImagePatchReconciler) handleExistingJob(ctx context.Context, imagePatch *omsv1alpha1.ImagePatch, job *batchv1.Job) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var newPhase string
	var message string

	if job.Status.Succeeded > 0 {
		newPhase = "Succeeded"
		message = "Build completed successfully"
	} else if job.Status.Failed > 0 {
		newPhase = "Failed"
		message = "Build failed"
	} else if job.Status.Active > 0 {
		newPhase = "Running"
		message = "Build is running"
	} else {
		// Job exists but no status yet, still pending. Requeue so we keep
		// observing -- same cross-namespace caveat as the freshly-created
		// case.
		return ctrl.Result{RequeueAfter: runningPhaseRequeueAfter}, nil
	}

	if imagePatch.Status.Phase != newPhase {
		imagePatch.Status.Phase = newPhase
		imagePatch.Status.Message = message
		if err := r.Status().Update(ctx, imagePatch); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonStatusUpdate)
			l.Error(err, "Failed to update ImagePatch status", "phase", newPhase)
			return ctrl.Result{}, err
		}
		recordTerminalBuild(newPhase, imagePatch.Status.Image, job)
		l.Info("Updated ImagePatch status", "phase", newPhase)
	}

	// Terminal phases need no requeue; the CR is done. While Running, we
	// keep polling -- with same-namespace Owns(&Job{}), the watch usually
	// fires first and short-circuits the wait, so this is mostly a safety
	// net. For cross-namespace builds it's the only signal we have.
	if newPhase == "Running" {
		return ctrl.Result{RequeueAfter: runningPhaseRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// recordTerminalBuild emits build metrics for a Succeeded/Failed transition.
// Called only after the status update has succeeded, so a failed Status.Update
// followed by a retry does not double-count. Non-terminal phases (Running) are
// ignored.
func recordTerminalBuild(newPhase, targetImage string, job *batchv1.Job) {
	var result, failureReason string
	switch newPhase {
	case "Succeeded":
		result = metrics.ResultSucceeded
		failureReason = metrics.FailureReasonNone
	case "Failed":
		result = metrics.ResultFailed
		failureReason = jobFailureReason(job)
	default:
		return
	}

	var start, end time.Time
	if job.Status.StartTime != nil {
		start = job.Status.StartTime.Time
	}
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	}
	metrics.RecordBuildResult(result, targetImage, failureReason, start, end)
}

// jobFailureReason classifies a failed build into one of the bounded
// metrics.FailureReason* values by inspecting Job conditions. We don't
// currently look at Pod-level termination state (e.g. OOMKilled) because
// it would require watching Pods in the manager cache; everything that
// isn't a Job-level reason (DeadlineExceeded / BackoffLimitExceeded)
// rolls up into FailureReasonBuild — i.e. "the build itself failed".
func jobFailureReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case "DeadlineExceeded":
			return metrics.FailureReasonDeadline
		case "BackoffLimitExceeded":
			return metrics.FailureReasonBackoff
		}
	}
	return metrics.FailureReasonBuild
}

// buildNamespaceFor returns the namespace in which build resources (Job +
// ConfigMap) should be created for a given ImagePatch. When r.BuildNamespace
// is empty the controller preserves legacy behavior and uses the CR's own
// namespace.
func (r *ImagePatchReconciler) buildNamespaceFor(cr *omsv1alpha1.ImagePatch) string {
	if r.BuildNamespace != "" {
		return r.BuildNamespace
	}
	return cr.Namespace
}

// sourceLabels marks build resources with the originating ImagePatch CR's
// name and namespace. Required when build resources live in a different
// namespace than the CR (no cross-namespace OwnerReference) and useful as
// traceability metadata even when they don't.
func sourceLabels(cr *omsv1alpha1.ImagePatch) map[string]string {
	return map[string]string{
		"app":                          "imagepatch",
		"imagepatch":                   cr.Name,
		"imagepatch.source.name":       cr.Name,
		"imagepatch.source.namespace":  cr.Namespace,
	}
}

// createOrUpdateConfigMap creates the ConfigMap if it doesn't exist, or updates it if it does
func (r *ImagePatchReconciler) createOrUpdateConfigMap(ctx context.Context, imagePatch *omsv1alpha1.ImagePatch, cmName, namespace, dockerfileContent string) error {
	l := log.FromContext(ctx)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
			Labels:    sourceLabels(imagePatch),
		},
		Data: map[string]string{
			"Dockerfile": dockerfileContent,
		},
	}

	// Owner refs only work within a single namespace; skip when the
	// ConfigMap lives elsewhere than the CR.
	if namespace == imagePatch.Namespace {
		if err := controllerutil.SetControllerReference(imagePatch, cm, r.Scheme); err != nil {
			return err
		}
	}

	if err := r.Create(ctx, cm); err != nil {
		if errors.IsAlreadyExists(err) {
			// update ConfigMap
			existingCM := &corev1.ConfigMap{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: namespace}, existingCM); getErr != nil {
				return getErr
			}
			existingCM.Data = cm.Data
			if updateErr := r.Update(ctx, existingCM); updateErr != nil {
				return updateErr
			}
			l.Info("Updated existing ConfigMap", "configmap", cmName)
			return nil
		}
		return err
	}

	l.Info("Created ConfigMap", "configmap", cmName)
	return nil
}

func constructJob(cr *omsv1alpha1.ImagePatch, jobName, cmName, namespace, destination, kanikoImage, pullCachePVC, pullCacheMountPath, buildCacheRepo string, resources corev1.ResourceRequirements, buildOpts omsv1alpha1.BuildOptions) *batchv1.Job {

	backoffLimit := int32(0)
	secretDefaultMode := int32(0664)

	args := []string{
		"--dockerfile=/workspace/Dockerfile",
		"--context=/workspace/context",
		"--destination=" + destination,
	}
	if pullCachePVC != "" {
		args = append(args, "--cache-dir="+pullCacheMountPath)
	}
	if buildCacheRepo != "" {
		args = append(args, "--cache=true", "--cache-repo="+buildCacheRepo)
	}
	args = append(args, buildOptionsArgs(buildOpts)...)

	volumeMounts := []corev1.VolumeMount{
		{Name: "dockerfile", MountPath: "/workspace/Dockerfile", SubPath: "Dockerfile"},
		{Name: "context", MountPath: "/workspace/context"},
		{Name: "docker-auth", MountPath: "/kaniko/.docker/config.json", SubPath: "config.json"},
	}
	volumes := []corev1.Volume{
		{
			Name: "dockerfile",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		},
		{
			Name: "context",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "docker-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  "image-registry-secret",
					DefaultMode: &secretDefaultMode,
				},
			},
		},
	}
	if pullCachePVC != "" {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "kaniko-pull-cache",
			MountPath: pullCacheMountPath,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "kaniko-pull-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pullCachePVC,
				},
			},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels:    sourceLabels(cr),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: sourceLabels(cr),
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:         "kaniko",
							Image:        kanikoImage,
							Args:         args,
							VolumeMounts: volumeMounts,
							Resources:    resources,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
}

// GenerateDockerfile generates a Dockerfile from the ImagePatch CR spec.
// GenerateDockerfile generates a Dockerfile from the ImagePatch CR spec.
func GenerateDockerfile(cr *omsv1alpha1.ImagePatch) string {
	var sb strings.Builder

	// FROM (extra stages) - emitted before the base image so kaniko parses
	// them as named stages that the final stage's COPY --from can resolve.
	for _, src := range cr.Spec.FromImages {
		sb.WriteString(fmt.Sprintf("FROM %s AS %s\n\n", src.Image, src.Name))
	}

	// FROM - base image
	sb.WriteString(fmt.Sprintf("FROM %s\n\n", cr.Spec.BaseImage))

	// SHELL - pin to absolute /bin/sh so subsequent RUNs work even when the
	// base image set SHELL to something kaniko can't resolve via PATH (e.g.
	// ["sh", "-lc"] on a minimal image without /bin in PATH).
	sb.WriteString("SHELL [\"/bin/sh\", \"-c\"]\n\n")

	// COPY --from - pull files from the multi-stage sources declared at
	// the top of the file. Emitted immediately after the base FROM so the
	// copied tree behaves like an extension of the base image: every
	// subsequent RUN (apt install, chmod, etc.) sees the files in place.
	// Order within the COPY block follows the user-supplied order of
	// FromImages and their Copy entries.
	for _, src := range cr.Spec.FromImages {
		for _, c := range src.Copy {
			dst := c.Dst
			if dst == "" {
				dst = c.Src
			}
			sb.WriteString(fmt.Sprintf("COPY --from=%s %s %s\n", src.Name, c.Src, dst))
		}
	}
	if hasAnyCopy(cr.Spec.FromImages) {
		sb.WriteString("\n")
	}

	// ENV - environment variables
	if len(cr.Spec.ENV) > 0 {
		for k, v := range cr.Spec.ENV {
			sb.WriteString(fmt.Sprintf("ENV %s=%s\n", k, v))
		}
		sb.WriteString("\n")
	}

	// APT - replace sources.list if mirror is configured in the CR
	// Auto-detects Ubuntu codename from /etc/os-release in the base image
	if cr.Spec.APT != nil && cr.Spec.APT.Mirror != "" {
		mirror := cr.Spec.APT.Mirror
		sb.WriteString(fmt.Sprintf("RUN . /etc/os-release && echo \"deb %s $VERSION_CODENAME main restricted universe multiverse\\n\\\n", mirror))
		for _, suffix := range []string{"-updates", "-security", "-backports"} {
			sb.WriteString(fmt.Sprintf("deb %s $VERSION_CODENAME%s main restricted universe multiverse\\n\\\n", mirror, suffix))
		}
		sb.WriteString("\" > /etc/apt/sources.list\n\n")
	}

	// APT - install packages.
	//
	// The two Dpkg::Options pin conffile-conflict resolution to a
	// non-interactive default: when a package ships a config file that
	// already exists on disk (e.g. /etc/containers/registries.conf
	// provided by a fromImages overlay), dpkg keeps the on-disk version
	// (--force-confold) for files the user touched and accepts the
	// package's default (--force-confdef) for ones they didn't. Without
	// these, dpkg prompts on stdin, fails with "end of file on stdin
	// at conffile prompt" under kaniko (no TTY), and the install — plus
	// every dependent package — aborts. DEBIAN_FRONTEND=noninteractive
	// alone does not cover conffile conflicts.
	if cr.Spec.APT != nil && len(cr.Spec.APT.Install) > 0 {
		// -q silences the per-line Get:/Hit:/Reading… progress chatter
		// from both update and install. We don't go to -qq because that
		// also implies --yes and we like the explicit -y signal.
		sb.WriteString("RUN apt-get -q update && apt-get -q install -y \\\n")
		sb.WriteString("    -o Dpkg::Options::=\"--force-confdef\" \\\n")
		sb.WriteString("    -o Dpkg::Options::=\"--force-confold\" \\\n")
		for _, pkg := range cr.Spec.APT.Install {
			sb.WriteString(fmt.Sprintf("    %s \\\n", pkg))
		}
		sb.WriteString("    && rm -rf /var/lib/apt/lists/*\n\n")
	}

	// PIP - install Python packages
	if cr.Spec.PIP != nil && len(cr.Spec.PIP.Install) > 0 {
		sb.WriteString(fmt.Sprintf("RUN pip install --no-cache-dir %s\n\n",
			strings.Join(cr.Spec.PIP.Install, " ")))
	}

	// USER - create and configure user
	if cr.Spec.User != nil && cr.Spec.User.Name != "" {
		// Create group if GID is specified
		if cr.Spec.User.GID > 0 {
			sb.WriteString(fmt.Sprintf("RUN groupadd -g %d %s\n",
				cr.Spec.User.GID, cr.Spec.User.Name))
		}

		// Create user
		if cr.Spec.User.UID > 0 {
			if cr.Spec.User.GID > 0 {
				sb.WriteString(fmt.Sprintf("RUN useradd -m -u %d -g %d %s\n",
					cr.Spec.User.UID, cr.Spec.User.GID, cr.Spec.User.Name))
			} else {
				sb.WriteString(fmt.Sprintf("RUN useradd -m -u %d %s\n",
					cr.Spec.User.UID, cr.Spec.User.Name))
			}
		} else {
			sb.WriteString(fmt.Sprintf("RUN useradd -m %s\n", cr.Spec.User.Name))
		}

		// Add sudo permissions if requested
		if cr.Spec.User.Sudo {
			sb.WriteString(fmt.Sprintf("RUN echo '%s ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers\n",
				cr.Spec.User.Name))
		}

		sb.WriteString(fmt.Sprintf("USER %s\n\n", cr.Spec.User.Name))
	}

	// SHELL - execute shell steps
	for _, step := range cr.Spec.Shell {
		// Add comment if step has a name
		if step.Name != "" {
			sb.WriteString(fmt.Sprintf("# %s\n", step.Name))
		}

		// Set workdir if specified
		if step.Workdir != "" {
			sb.WriteString(fmt.Sprintf("WORKDIR %s\n", step.Workdir))
		}

		// Set user if specified
		if step.User != "" {
			sb.WriteString(fmt.Sprintf("USER %s\n", step.User))
		}

		// Run the command directly — Dockerfile RUN already uses /bin/sh -c
		run := strings.TrimSpace(step.Run)
		run = strings.ReplaceAll(run, "\n", " && \\\n    ")
		sb.WriteString(fmt.Sprintf("RUN %s\n\n", run))
	}

	// ENTRYPOINT
	if len(cr.Spec.Entrypoint) > 0 {
		sb.WriteString(fmt.Sprintf("ENTRYPOINT %s\n", formatCmdArray(cr.Spec.Entrypoint)))
	}

	// CMD
	if len(cr.Spec.CMD) > 0 {
		sb.WriteString(fmt.Sprintf("CMD %s\n", formatCmdArray(cr.Spec.CMD)))
	}

	return sb.String()
}

// BuildOptionsFromEnv reads the chart-wide build option defaults from
// environment variables emitted by the Helm chart's deployment template.
// Each variable corresponds 1:1 to a BuildOptions field so the chart can
// surface them without growing a sidecar config format.
//
//	KANIKO_SNAPSHOT_MODE     -> SnapshotMode (full / redo / time)
//	KANIKO_SINGLE_SNAPSHOT   -> SingleSnapshot ("true" / "false")
//	KANIKO_IGNORE_PATHS      -> IgnorePaths (comma-separated)
//	KANIKO_CACHE_TTL         -> CacheTTL (Go duration)
func BuildOptionsFromEnv() omsv1alpha1.BuildOptions {
	opts := omsv1alpha1.BuildOptions{
		SnapshotMode: os.Getenv("KANIKO_SNAPSHOT_MODE"),
		CacheTTL:     os.Getenv("KANIKO_CACHE_TTL"),
	}
	if v := os.Getenv("KANIKO_SINGLE_SNAPSHOT"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		opts.SingleSnapshot = &b
	}
	if v := os.Getenv("KANIKO_IGNORE_PATHS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				opts.IgnorePaths = append(opts.IgnorePaths, p)
			}
		}
	}
	return opts
}

// mergeBuildOptions returns the effective BuildOptions for a build:
// CR-supplied fields win, otherwise the chart-wide default applies.
// Designed to be order-independent and field-granular so a CR can
// override one knob without restating the rest.
func mergeBuildOptions(defaults omsv1alpha1.BuildOptions, override *omsv1alpha1.BuildOptions) omsv1alpha1.BuildOptions {
	out := defaults
	if override == nil {
		return out
	}
	if override.SnapshotMode != "" {
		out.SnapshotMode = override.SnapshotMode
	}
	if override.SingleSnapshot != nil {
		v := *override.SingleSnapshot
		out.SingleSnapshot = &v
	}
	if len(override.IgnorePaths) > 0 {
		out.IgnorePaths = append([]string(nil), override.IgnorePaths...)
	}
	if override.CacheTTL != "" {
		out.CacheTTL = override.CacheTTL
	}
	return out
}

// buildOptionsArgs renders the BuildOptions into Kaniko CLI flags.
// Each zero-valued field is omitted so Kaniko keeps its own default
// rather than receiving an empty-string flag.
func buildOptionsArgs(opts omsv1alpha1.BuildOptions) []string {
	var args []string
	if opts.SnapshotMode != "" {
		args = append(args, "--snapshot-mode="+opts.SnapshotMode)
	}
	if opts.SingleSnapshot != nil && *opts.SingleSnapshot {
		args = append(args, "--single-snapshot")
	}
	for _, p := range opts.IgnorePaths {
		if p != "" {
			args = append(args, "--ignore-path="+p)
		}
	}
	if opts.CacheTTL != "" {
		args = append(args, "--cache-ttl="+opts.CacheTTL)
	}
	return args
}

// resolveDestination determines the target image for the build.
// Priority: CR spec.targetImage > DEFAULT_IMAGE_REGISTRY/<base-name>-patch:<base-tag>
// The image name and tag are parsed from spec.baseImage.
// e.g. registry.luna.ogpu.cloud/luna/ubuntu-22.04:latest -> ubuntu-22.04-patch:latest
func (r *ImagePatchReconciler) resolveDestination(cr *omsv1alpha1.ImagePatch) string {
	if cr.Spec.TargetImage != "" {
		return cr.Spec.TargetImage
	}
	// Parse name and tag from baseImage
	ref := cr.Spec.BaseImage
	tag := "latest"
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		tag = ref[idx+1:]
		ref = ref[:idx]
	}
	// Extract the image name (last segment after /)
	name := ref
	if idx := strings.LastIndex(ref, "/"); idx != -1 {
		name = ref[idx+1:]
	}
	if r.DefaultRegistry != "" {
		return fmt.Sprintf("%s/%s-patch:%s", strings.TrimRight(r.DefaultRegistry, "/"), name, tag)
	}
	return fmt.Sprintf("%s-patch:%s", name, tag)
}

// hasAnyCopy reports whether any FromImage entry has at least one Copy
// directive, so the generator can decide whether to emit the trailing
// blank line after the COPY --from block.
func hasAnyCopy(srcs []omsv1alpha1.FromImage) bool {
	for _, s := range srcs {
		if len(s.Copy) > 0 {
			return true
		}
	}
	return false
}

// formatCmdArray formats a command array for Dockerfile ENTRYPOINT/CMD
func formatCmdArray(cmd []string) string {
	quoted := make([]string, len(cmd))
	for i, c := range cmd {
		quoted[i] = fmt.Sprintf("\"%s\"", c)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImagePatchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&omsv1alpha1.ImagePatch{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Named("imagepatch").
		Complete(r)
}
