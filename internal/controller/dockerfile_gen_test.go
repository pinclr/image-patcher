package controller

import (
	"fmt"
	"testing"

	v1alpha1 "image-builder/api/v1alpha1"
)

func TestDockerfileGenMirror(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage: "ubuntu:24.04",
			APT: &v1alpha1.AptConfig{
				Mirror:  "http://10.11.32.173/ubuntu",
				Install: []string{"tini", "podman"},
			},
		},
	}
	fmt.Println(GenerateDockerfile(cr))
}
