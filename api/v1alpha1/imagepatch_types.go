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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ImagePatchSpec defines the desired state of ImagePatch
type ImagePatchSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// BaseImage is the base image to use for the patch
	// +kubebuilder:validation:Required
	BaseImage string `json:"baseImage"`

	// TargetImage is the destination image to push the built image to.
	// If not specified, the controller uses DEFAULT_IMAGE_REGISTRY env var to generate one.
	// +optional
	TargetImage string `json:"targetImage,omitempty"`

	// Env
	ENV map[string]string `json:"env,omitempty"`

	// Apt
	APT *AptConfig `json:"apt,omitempty"`

	// Pip
	PIP *PipConfig `json:"pip,omitempty"`

	// Shell
	Shell []ShellStep `json:"shell,omitempty"`

	// FromImages declares additional source images for multi-stage builds.
	// Each entry becomes a `FROM <image> AS <name>` stage at the top of the
	// generated Dockerfile. Use Copy to emit `COPY --from=<name> <src> <dst>`
	// lines that pull files into the final image.
	// +optional
	FromImages []FromImage `json:"fromImages,omitempty"`

	// User
	User *UserConfig `json:"user,omitempty"`

	Entrypoint []string `json:"entrypoint,omitempty"`
	CMD        []string `json:"cmd,omitempty"`
}

// FromImage declares a multi-stage build source. The image is pulled
// only to satisfy COPY --from references; nothing from it survives in
// the final image unless an explicit Copy entry references it.
type FromImage struct {
	// Image is the OCI reference used as a build stage source.
	// Emitted as `FROM <image> AS <name>`.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// Name is the stage alias referenced by COPY --from=<name>.
	// Must be a valid Dockerfile stage identifier.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Copy lists files or directories to pull from this stage into
	// the final image. Each entry is emitted as
	// `COPY --from=<name> <src> <dst>` after the patch's RUN steps.
	// +optional
	Copy []CopySpec `json:"copy,omitempty"`
}

// CopySpec describes a single COPY --from operation.
type CopySpec struct {
	// Src is the source path inside the FromImage's filesystem.
	// +kubebuilder:validation:Required
	Src string `json:"src"`

	// Dst is the destination path in the final image. Defaults to Src
	// when omitted.
	// +optional
	Dst string `json:"dst,omitempty"`
}

type AptConfig struct {
	// Mirror is the apt mirror URL (e.g. http://10.11.32.173/ubuntu).
	// If set, replaces /etc/apt/sources.list before apt-get update.
	// The Ubuntu codename is auto-detected from /etc/os-release in the base image.
	// +optional
	Mirror string `json:"mirror,omitempty"`

	Install []string `json:"install,omitempty"`
}

type PipConfig struct {
	Install []string `json:"install,omitempty"`
}

type ShellStep struct {
	Name    string `json:"name,omitempty"`
	Run     string `json:"run"`
	Workdir string `json:"workdir,omitempty"`
	User    string `json:"user,omitempty"`
}

type UserConfig struct {
	Name string `json:"name,omitempty"`
	UID  int64  `json:"uid,omitempty"`
	GID  int64  `json:"gid,omitempty"`
	Sudo bool   `json:"sudo,omitempty"`
}

// ImagePatchStatus defines the observed state of ImagePatch.
type ImagePatchStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the ImagePatch resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	JobName string `json:"jobName,omitempty"`

	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ImagePatch is the Schema for the imagepatches API
type ImagePatch struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ImagePatch
	// +required
	Spec ImagePatchSpec `json:"spec"`

	// status defines the observed state of ImagePatch
	// +optional
	Status ImagePatchStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ImagePatchList contains a list of ImagePatch
type ImagePatchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ImagePatch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImagePatch{}, &ImagePatchList{})
}
