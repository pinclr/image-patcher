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
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
	"image-patch-operator/internal/metrics"
	"image-patch-operator/internal/registry"
)

// ImagePatchReconciler reconciles a ImagePatch object
type ImagePatchReconciler struct {
	client.Client
	// Kubernetes is a typed clientset held alongside the controller-runtime
	// Client because the latter cannot reach subresources like Pods/log.
	// classifyBuildFailure uses it to tail the Kaniko build Pod's stdout
	// for failure-cause classification; when nil (e.g. in unit tests that
	// don't wire an API server) failure classification degrades to
	// FailureLabelControllerInternalError -- the safe default.
	Kubernetes      kubernetes.Interface
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
	// KanikoBuildCacheRepo, when set, is passed to Kaniko as --cache-repo
	// for the build/layer cache (intermediate RUN steps cached in a
	// container registry).
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
	// DedupEnabled controls whether Kaniko gets a second --destination
	// pointing at the content-addressed dedup tag. Default true; the
	// flag exists as an ops kill switch for registries whose retention
	// or quota rules can't yet cope with the extra tags. Read from
	// DEDUP_ENABLED env (anything other than "false" treated as on, so
	// missing env defaults to on).
	DedupEnabled bool
	// Registry is the OCI client used to short-circuit the build: HEAD
	// the dedup ref before creating a Kaniko Job, and retag the
	// existing manifest under the user destination on a hit. Nil
	// disables the short-circuit (Kaniko still runs and writes the
	// dedup tag as before); main wires nil when the docker config
	// secret isn't mounted, so dedup-write keeps working even without
	// controller-side registry auth.
	Registry *registry.Client
}

// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=oms.ogpu.cloud,resources=imagepatches/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

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
		// Job already exists. Whatever decision allowed it (a prior
		// pre-flight pass, or a controller version that didn't run
		// pre-flight at all) is locked in -- classify via the Job's
		// status. Do NOT re-run pre-flight here; it would otherwise
		// fight handleExistingJob's "Build is running" message update
		// every reconcile and re-bill the registry for nothing.
		return r.handleExistingJob(ctx, &imagePatch, &job)
	}
	if !errors.IsNotFound(err) {
		metrics.RecordReconcileFailure(metrics.ReasonGetJob)
		return ctrl.Result{}, err
	}

	// No Job yet -- pre-flight is the gate to creating one.
	//   - reject:    mark Failed/ImageOSNotSupported, no Job, no ConfigMap
	//   - accept:    drop through to Job/ConfigMap creation
	//   - fail-open: same as accept (registry blip / private image
	//                we can't read with anonymous auth -- kaniko handles
	//                via its own mounted credentials)
	if perr := registry.RejectIfPlatformMismatch(ctx, imagePatch.Spec.BaseImage,
		defaultBuildOS, defaultBuildArch); perr != nil {
		l.Info("pre-flight rejected: base image does not include build target platform",
			"image", imagePatch.Spec.BaseImage, "detail", perr.Error())
		imagePatch.Status.Phase = "Failed"
		imagePatch.Status.Message = FailureLabelImageOSNotSupported
		if updateErr := r.Status().Update(ctx, &imagePatch); updateErr != nil {
			metrics.RecordReconcileFailure(metrics.ReasonStatusUpdate)
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
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

		// Compute the content-addressed dedup ref: same repo as the
		// user destination, tag = dedup-<spec hash>. Kaniko pushes to
		// both destinations in one build; the second tag is a manifest
		// reference reusing the first's blobs (same repo = blob scope
		// shared, so no MANIFEST_BLOB_UNKNOWN and no extra upload).
		// Gated on both the cluster kill switch (DedupEnabled) and
		// the per-CR opt-out (DisableBuildCache). Per-CR opt-out
		// skips BOTH read (short-circuit) and write (second
		// --destination) so the build is a true bypass that does not
		// pollute the dedup cache for future identical specs.
		var specHash, dedupRef string
		if r.DedupEnabled && !boolPtrTrue(buildOpts.DisableBuildCache) {
			specHash, dedupRef = computeDedupRef(&imagePatch.Spec, destination)
		}

		// Short-circuit: if a prior build with the same spec already
		// produced the dedup tag, retag it under the user destination
		// and skip the Job entirely. Fail-open: any registry error
		// (HEAD/PUT) just falls through to the normal build path -- a
		// flaky registry must not block builds, and the Kaniko Job will
		// re-attempt the retag side-effect anyway via multi-destination
		// push.
		if dedupRef != "" && r.Registry != nil {
			if done, err := r.tryDedupShortCircuit(ctx, &imagePatch, destination, dedupRef, specHash); err != nil {
				return ctrl.Result{}, err
			} else if done {
				return ctrl.Result{}, nil
			}
		}

		j := constructJob(&imagePatch, jobName, cmName, buildNs, destination, r.KanikoImage,
			r.KanikoBuildCacheRepo, r.KanikoResources, buildOpts, dedupRef)
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

// defaultBuildOS / defaultBuildArch are the (os, arch) the pre-flight
// platform inspect rejects mismatches against. Hard-coded today
// because every build runner the project ships with is amd64; promote
// to env-overridable (KANIKO_BUILD_PLATFORM or similar) the moment a
// non-amd64 build pool exists.
const (
	defaultBuildOS   = "linux"
	defaultBuildArch = "amd64"
)

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
		// Replace the previous hard-coded "Build failed" with a
		// classification label drawn from the build Pod's log tail. The
		// downstream oms-controller renders this as `failed: <label>`
		// to the end user, so they can tell at a glance whether the
		// fix is on their side (BaseImageNotFound, AuthorizationNeeded,
		// NetworkError) or ours (ControllerInternalError).
		//
		// Already-classified messages are sticky: re-running the
		// classifier after the build Pod has been GC'd would always
		// return ControllerInternalError (logs unreachable) and would
		// silently downgrade an accurate label. Anything else --
		// empty string, the legacy "Build failed", or free-form text
		// -- gets (re-)classified, which is also how we backfill CRs
		// left in Phase=Failed by an older version of this controller.
		if IsKnownFailureLabel(imagePatch.Status.Message) {
			message = imagePatch.Status.Message
		} else {
			message = classifyBuildFailure(ctx, r.Kubernetes, job)
		}
	} else if job.Status.Active > 0 {
		newPhase = "Running"
		message = "Build is running"
	} else {
		// Job exists but no status yet, still pending. Requeue so we keep
		// observing -- same cross-namespace caveat as the freshly-created
		// case.
		return ctrl.Result{RequeueAfter: runningPhaseRequeueAfter}, nil
	}

	// Update on either a phase transition OR a stale message. The latter
	// covers the backfill case: a CR may already sit in Phase=Failed with
	// message "Build failed" written by the pre-classification controller;
	// without the message check the guard would short-circuit and the
	// user-facing string would never get refreshed. recordTerminalBuild
	// is gated on the phase transition specifically so message-only
	// rewrites don't double-count the build metric.
	phaseChanged := imagePatch.Status.Phase != newPhase
	messageChanged := imagePatch.Status.Message != message
	if phaseChanged || messageChanged {
		imagePatch.Status.Phase = newPhase
		imagePatch.Status.Message = message
		if err := r.Status().Update(ctx, imagePatch); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonStatusUpdate)
			l.Error(err, "Failed to update ImagePatch status", "phase", newPhase)
			return ctrl.Result{}, err
		}
		if phaseChanged {
			recordTerminalBuild(imagePatch, newPhase, imagePatch.Status.Image, job)
		}
		l.Info("Updated ImagePatch status", "phase", newPhase, "message", message)
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

// tryDedupShortCircuit attempts to skip the Kaniko build by retagging
// an existing dedup manifest under the user destination. Returns
// (true, nil) when the short-circuit fired and the CR has been marked
// Succeeded -- the caller must NOT proceed to create a Job. Returns
// (false, nil) on a miss OR on any registry error (fail-open: a
// flaky registry must not block builds, and the Kaniko build path
// will write the dedup tag itself via multi-destination push). The
// only path that returns a non-nil err is a failed Status.Update --
// the retag has happened by then, so requeueing is safe (Status set
// is idempotent; retag is, too, since the dst tag already points at
// the same manifest).
func (r *ImagePatchReconciler) tryDedupShortCircuit(ctx context.Context, cr *omsv1alpha1.ImagePatch, destination, dedupRef, specHash string) (bool, error) {
	l := log.FromContext(ctx)

	exists, err := r.Registry.Exists(ctx, dedupRef)
	if err != nil {
		l.Info("dedup HEAD failed; falling through to build", "dedupRef", dedupRef, "err", err.Error())
		return false, nil
	}
	if !exists {
		return false, nil
	}

	if err := r.Registry.Retag(ctx, dedupRef, destination); err != nil {
		l.Info("dedup retag failed; falling through to build", "src", dedupRef, "dst", destination, "err", err.Error())
		return false, nil
	}

	cr.Status.Phase = "Succeeded"
	cr.Status.Message = "build skipped: content match in registry"
	cr.Status.Image = destination
	cr.Status.SpecHash = specHash
	cr.Status.DedupHit = true
	cr.Status.JobName = ""
	if err := r.Status().Update(ctx, cr); err != nil {
		metrics.RecordReconcileFailure(metrics.ReasonStatusUpdate)
		l.Error(err, "dedup hit: status update failed; will retry", "dedupRef", dedupRef)
		return false, err
	}
	metrics.RecordBuildResult(metrics.ResultSucceeded, destination, cr.Spec.BaseImage, metrics.FailureReasonNone,
		true /*buildCacheHit*/, buildLayerCacheHit(cr), isCanary(cr),
		cr.CreationTimestamp.Time, time.Time{} /*no Job*/, time.Time{})
	l.Info("dedup hit: skipped build", "imagepatch", cr.Name, "dedupRef", dedupRef, "destination", destination, "specHash", specHash)
	return true, nil
}

// recordTerminalBuild emits build metrics for a Succeeded/Failed transition.
// Called only after the status update has succeeded, so a failed Status.Update
// followed by a retry does not double-count. Non-terminal phases (Running) are
// ignored.
func recordTerminalBuild(cr *omsv1alpha1.ImagePatch, newPhase, targetImage string, job *batchv1.Job) {
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
	metrics.RecordBuildResult(result, targetImage, cr.Spec.BaseImage, failureReason,
		false /*buildCacheHit*/, buildLayerCacheHit(cr), isCanary(cr),
		cr.CreationTimestamp.Time, start, end)
}

// buildLayerCacheHit reports whether the CR allowed Kaniko's RUN-layer
// cache (i.e. did NOT set spec.buildOptions.disableBuildLayerCache).
// Tracks per-CR intent rather than whether Kaniko actually pulled a
// cached layer from the registry -- that needs a Kaniko-side metric we
// don't have. Naming as "hit" lets the metric label read symmetrically
// with buildCacheHit (the controller dedup short-circuit indicator).
// Chart-level defaults (kaniko.buildCache.enabled) are intentionally
// ignored here: this label is per-CR, not effective config.
func buildLayerCacheHit(cr *omsv1alpha1.ImagePatch) bool {
	return !(cr.Spec.BuildOptions != nil && boolPtrTrue(cr.Spec.BuildOptions.DisableBuildLayerCache))
}

// isCanary returns whether the CR was submitted by the chart's
// healthcheck CronJob. Driven by the image-patcher.healthcheck/canary
// label which the probe script stamps on every CR it creates.
// Dashboards filter canary=false by default so the synthetic per-tick
// load doesn't dominate production graphs.
func isCanary(cr *omsv1alpha1.ImagePatch) bool {
	return cr.Labels["image-patcher.healthcheck/canary"] == "true"
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

func constructJob(cr *omsv1alpha1.ImagePatch, jobName, cmName, namespace, destination, kanikoImage, buildCacheRepo string, resources corev1.ResourceRequirements, buildOpts omsv1alpha1.BuildOptions, dedupDestination string) *batchv1.Job {

	backoffLimit := int32(0)
	secretDefaultMode := int32(0664)

	args := []string{
		"--dockerfile=/workspace/Dockerfile",
		"--context=/workspace/context",
		"--destination=" + destination,
	}
	if dedupDestination != "" && dedupDestination != destination {
		// Multi-destination push: Kaniko computes layers once and
		// uploads blobs once; the second --destination just adds a tag
		// reference. Lets dedup observe future builds without doubling
		// upload time.
		args = append(args, "--destination="+dedupDestination)
	}
	if buildCacheRepo != "" && !boolPtrTrue(buildOpts.DisableBuildLayerCache) {
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

// imageOSCheckCommand is emitted as the VERY FIRST RUN in every generated
// Dockerfile so a non-Ubuntu (or unsupported-Ubuntu) base fails with a
// classifiable signal instead of producing a downstream "VERSION_CODENAME
// is empty" / "apt-get not found" error that the log classifier can't pin
// on the user.
//
// Signal is the exit code 42 (not a printed marker): kaniko echoes every
// RUN's command body verbatim into INFO log lines (`No cached layer
// found for cmd RUN ...`, `RUN ...`, `Args: [-c ...]`, `Running:
// [/bin/sh -c ...]`), so anything we echo here would also appear in
// every successful build's logs and false-trigger the classifier.
// Instead the guard exits 42 silently, and classify.go matches the
// `waiting for process to exit: exit status 42` line that kaniko itself
// emits on RUN failure -- structural emission, not a substring of the
// command body. Trade-off: `kubectl logs <build-pod>` no longer shows
// which branch fired (ID vs. VERSION_ID, with the offending value);
// the user-facing label is just "ImageOSNotSupported" and the operator
// can re-check the base image's /etc/os-release manually.
//
// Single-line shell on purpose: the run line goes straight into a
// Dockerfile `RUN`, so an embedded newline would either need backslash
// continuation (verbose) or break case/esac.
//
// Supported Ubuntu set: 20.04 / 22.04 / 24.04 / 26.04. Add new LTS
// versions here as we adopt them.
//
// image-patcher is fully Ubuntu-coupled today (APT block uses apt-get
// and $VERSION_CODENAME from /etc/os-release), so emitting this check
// unconditionally doesn't reduce capability -- it just surfaces an
// assumption that was already baked in.
const imageOSCheckCommand = `[ -r /etc/os-release ] || exit 42; ` +
	`. /etc/os-release; ` +
	`[ "$ID" = "ubuntu" ] || exit 42; ` +
	`case "$VERSION_ID" in ` +
	`20.04|22.04|24.04|26.04) ;; ` +
	`*) exit 42 ;; ` +
	`esac`

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

	// Image-OS guard. Must be the first RUN -- before APT mirror,
	// COPY --from, or any user-defined shell step -- so that
	// non-Ubuntu / unsupported-Ubuntu bases fail with the
	// "ImageOSNotSupported:" marker rather than producing a downstream
	// "VERSION_CODENAME is empty" / "apt-get not found" error the
	// log classifier can't pin on the user. See imageOSCheckCommand
	// above for the supported-version contract and case-sensitivity
	// rationale.
	sb.WriteString("RUN " + imageOSCheckCommand + "\n\n")

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

	// APT - replace Ubuntu's apt sources if a mirror is configured.
	// Wipes BOTH the legacy /etc/apt/sources.list (jammy and earlier)
	// AND the deb822 /etc/apt/sources.list.d/ubuntu.sources (noble and
	// later) before writing the mirror as legacy format. Without the
	// deb822 wipe, noble's pre-existing ubuntu.sources still points at
	// archive.ubuntu.com / security.ubuntu.com and apt reads BOTH --
	// builds fetch from upstream alongside the internal mirror,
	// defeating the point and adding an external dependency. Legacy
	// `deb` syntax is parsed by every apt version including noble, so
	// writing it remains universal. Third-party `.list` / `.sources`
	// files (NVIDIA, podman, etc.) in sources.list.d/ are intentionally
	// preserved -- those are usually opt-in from fromImages or the
	// base image and not what aptConfig.mirror is replacing.
	if cr.Spec.APT != nil && cr.Spec.APT.Mirror != "" {
		mirror := cr.Spec.APT.Mirror
		// printf, not echo: /bin/sh is dash and POSIX echo writes `\n`
		// literally, which collapses sources.list into one line and makes
		// apt parse junk components like `multiverse\ndeb`. printf always
		// interprets `\n`.
		sb.WriteString("RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources && \\\n")
		sb.WriteString(fmt.Sprintf("    . /etc/os-release && printf \"deb %s $VERSION_CODENAME main restricted universe multiverse\\n\\\n", mirror))
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

// boolPtrTrue returns true iff p points at a true value. Used to gate
// BuildOptions disable-flags whose nil-vs-false distinction matters
// only at merge time -- once merged, "set and false" and "unset" both
// mean "feature stays on".
func boolPtrTrue(p *bool) bool {
	return p != nil && *p
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
	if override.DisableBuildCache != nil {
		v := *override.DisableBuildCache
		out.DisableBuildCache = &v
	}
	if override.DisableBuildLayerCache != nil {
		v := *override.DisableBuildLayerCache
		out.DisableBuildLayerCache = &v
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
// computeDedupRef derives the content-addressed dedup reference from
// the resolved user destination. Same repo as destination, tag =
// "dedup-<spec hash>" -- purely content-addressed, the user tag is
// NOT part of the dedup tag. This matters when consumers generate a
// new user tag per build (e.g. one tag per devbox instance: tag like
// "latest-<instanceID>"): the spec content is identical across all
// of them, so all builds must converge on a single dedup tag,
// otherwise the short-circuit never fires for non-stable user tags.
// healthcheck happens to use a stable :latest tag and would work
// even with the old <userTag>-dedup-<hash> scheme; dev55-style CRs
// with per-instance suffixes would not.
//
// Same-repo is still load-bearing: Kaniko multi-destination push
// uploads blobs once into the first destination's repo, and the
// second destination only writes a manifest. Cross-repo would fail
// with MANIFEST_BLOB_UNKNOWN.
//
// Returns ("", "") when destination is empty or the hash can't be
// computed. Returns the hash but no ref when destination has no
// repo:tag shape to split -- defensive; resolveDestination always
// emits "<repo>:<tag>", so in practice this branch is unreachable.
//
// NOTE: today the hash uses Spec.BaseImage verbatim. If the base
// reference is a mutable tag, content changes upstream produce a
// stale hash. A follow-up will resolve the base to a digest via a
// registry client before hashing -- that path is gated on figuring
// out controller-side registry credentials.
func computeDedupRef(spec *omsv1alpha1.ImagePatchSpec, destination string) (specHash, dedupRef string) {
	h := ComputeSpecHash(spec, "")
	if h == "" {
		return "", ""
	}
	idx := strings.LastIndex(destination, ":")
	if idx == -1 || idx < strings.LastIndex(destination, "/") {
		// No repo:tag shape (digest ref or bare repo). Skip the second
		// destination rather than guess.
		return h, ""
	}
	return h, destination[:idx] + ":dedup-" + h
}

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
