package controller

import (
	"testing"

	v1alpha1 "image-patch-operator/api/v1alpha1"
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

	got := GenerateDockerfile(cr)

	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

RUN . /etc/os-release && echo "deb http://10.11.32.173/ubuntu $VERSION_CODENAME main restricted universe multiverse\n\
deb http://10.11.32.173/ubuntu $VERSION_CODENAME-updates main restricted universe multiverse\n\
deb http://10.11.32.173/ubuntu $VERSION_CODENAME-security main restricted universe multiverse\n\
deb http://10.11.32.173/ubuntu $VERSION_CODENAME-backports main restricted universe multiverse\n\
" > /etc/apt/sources.list

RUN apt-get update && apt-get install -y \
    tini \
    podman \
    && rm -rf /var/lib/apt/lists/*

`

	if got != want {
		t.Errorf("Dockerfile mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestDockerfileGenFromImages(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage: "ubuntu:24.04",
			FromImages: []v1alpha1.FromImage{
				{
					Image: "lunalabs-acr-registry.cn-guangzhou.cr.aliyuncs.com/luna/devpod-rootfs:0.1.0",
					Name:  "rootfs",
					Copy: []v1alpha1.CopySpec{
						{Src: "/rootfs", Dst: "/"},
						{Src: "/etc/pip.conf"}, // Dst defaults to Src
					},
				},
			},
			Shell: []v1alpha1.ShellStep{
				{Name: "noop", Run: "true"},
			},
		},
	}

	got := GenerateDockerfile(cr)

	want := `FROM lunalabs-acr-registry.cn-guangzhou.cr.aliyuncs.com/luna/devpod-rootfs:0.1.0 AS rootfs

FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

# noop
RUN true

COPY --from=rootfs /rootfs /
COPY --from=rootfs /etc/pip.conf /etc/pip.conf

`

	if got != want {
		t.Errorf("Dockerfile mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
