package controller

import (
	"testing"

	v1alpha1 "image-patch-operator/api/v1alpha1"
)

// TestDockerfileGenIncludesOSCheck locks the contract that EVERY generated
// Dockerfile -- even for a CR with no apt / from-images / shell -- emits
// the built-in OS guard as its first RUN, sitting between the SHELL pin
// and whatever else the CR adds (apt mirror, copy-from, user shell, ...).
//
// This is the foothold non-Ubuntu and scratch bases hit before any
// downstream step, so they fail with the classifier-friendly
// "ImageOSNotSupported:" marker (sentinel match in classify.go) instead
// of producing a less specific kaniko error that the classifier can only
// catch via the fork/exec fallback. The ordering-with-other-RUNs case
// is already covered verbatim by TestDockerfileGenMirror and
// TestDockerfileGenFromImages -- their `want` strings would fail if
// the OS check were emitted in the wrong position.
func TestDockerfileGenIncludesOSCheck(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage: "ubuntu:24.04",
		},
	}

	got := GenerateDockerfile(cr)

	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

RUN ` + imageOSCheckCommand + `

`
	if got != want {
		t.Errorf("Dockerfile mismatch on minimal CR:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

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

	// The leading `RUN <imageOSCheckCommand>` is the built-in OS guard
	// emitted unconditionally after the SHELL pin; see imagepatch_controller.go.
	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

RUN ` + imageOSCheckCommand + `

RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources && \
    . /etc/os-release && printf "deb http://10.11.32.173/ubuntu $VERSION_CODENAME main restricted universe multiverse\n\
deb http://10.11.32.173/ubuntu $VERSION_CODENAME-updates main restricted universe multiverse\n\
deb http://10.11.32.173/ubuntu $VERSION_CODENAME-security main restricted universe multiverse\n\
deb http://10.11.32.173/ubuntu $VERSION_CODENAME-backports main restricted universe multiverse\n\
" > /etc/apt/sources.list

RUN apt-get -q update && apt-get -q install -y \
    -o Dpkg::Options::="--force-confdef" \
    -o Dpkg::Options::="--force-confold" \
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

	// `RUN <imageOSCheckCommand>` is the built-in OS guard emitted
	// unconditionally after the SHELL pin -- before COPY --from and any
	// user-supplied shell step. See imagepatch_controller.go.
	want := `FROM lunalabs-acr-registry.cn-guangzhou.cr.aliyuncs.com/luna/devpod-rootfs:0.1.0 AS rootfs

FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

RUN ` + imageOSCheckCommand + `

COPY --from=rootfs /rootfs /
COPY --from=rootfs /etc/pip.conf /etc/pip.conf

# noop
RUN true

`

	if got != want {
		t.Errorf("Dockerfile mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
