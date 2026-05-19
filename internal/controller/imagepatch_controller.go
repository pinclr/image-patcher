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

	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: imagePatch.Namespace}, &job)
	if err == nil {
		return r.handleExistingJob(ctx, &imagePatch, &job)
	}

	if errors.IsNotFound(err) {
		l.Info("Creating new build resources", "ImagePatch", imagePatch.Name)

		dockerfileContent := GenerateDockerfile(&imagePatch)
		l.V(1).Info("Generated Dockerfile", "content", dockerfileContent)

		if err := r.createOrUpdateConfigMap(ctx, &imagePatch, cmName, dockerfileContent); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonConfigMapApply)
			return ctrl.Result{}, err
		}

		destination := r.resolveDestination(&imagePatch)
		j := constructJob(&imagePatch, jobName, cmName, destination, r.KanikoImage,
			r.KanikoPullCachePVC, r.KanikoPullCacheMountPath, r.KanikoBuildCacheRepo)
		if err := controllerutil.SetControllerReference(&imagePatch, j, r.Scheme); err != nil {
			metrics.RecordReconcileFailure(metrics.ReasonOwnerRef)
			return ctrl.Result{}, err
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
		return ctrl.Result{}, nil
	}

	metrics.RecordReconcileFailure(metrics.ReasonGetJob)
	return ctrl.Result{}, err
}

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
		// Job exists but no status yet, still pending
		return ctrl.Result{}, nil
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

	return ctrl.Result{}, nil
}

// recordTerminalBuild emits build metrics for a Succeeded/Failed transition.
// Called only after the status update has succeeded, so a failed Status.Update
// followed by a retry does not double-count. Non-terminal phases (Running) are
// ignored.
func recordTerminalBuild(newPhase, targetImage string, job *batchv1.Job) {
	var result string
	switch newPhase {
	case "Succeeded":
		result = metrics.ResultSucceeded
	case "Failed":
		result = metrics.ResultFailed
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
	metrics.RecordBuildResult(result, targetImage, start, end)
}

// createOrUpdateConfigMap creates the ConfigMap if it doesn't exist, or updates it if it does
func (r *ImagePatchReconciler) createOrUpdateConfigMap(ctx context.Context, imagePatch *omsv1alpha1.ImagePatch, cmName, dockerfileContent string) error {
	l := log.FromContext(ctx)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: imagePatch.Namespace,
		},
		Data: map[string]string{
			"Dockerfile": dockerfileContent,
		},
	}

	// set OwnerReference
	if err := controllerutil.SetControllerReference(imagePatch, cm, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, cm); err != nil {
		if errors.IsAlreadyExists(err) {
			// update ConfigMap
			existingCM := &corev1.ConfigMap{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: imagePatch.Namespace}, existingCM); getErr != nil {
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

func constructJob(cr *omsv1alpha1.ImagePatch, jobName, cmName, destination, kanikoImage, pullCachePVC, pullCacheMountPath, buildCacheRepo string) *batchv1.Job {

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
			Namespace: cr.Namespace,
			Labels:    map[string]string{"app": "imagepatch", "imagepatch": cr.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "imagepatch", "imagepatch": cr.Name},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:         "kaniko",
							Image:        kanikoImage,
							Args:         args,
							VolumeMounts: volumeMounts,
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

	// FROM - base image
	sb.WriteString(fmt.Sprintf("FROM %s\n\n", cr.Spec.BaseImage))

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

	// APT - install packages
	if cr.Spec.APT != nil && len(cr.Spec.APT.Install) > 0 {
		sb.WriteString("RUN apt-get update && apt-get install -y \\\n")
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
