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

USER root

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
	// emitted unconditionally after the SHELL pin and `USER root`; see
	// imagepatch_controller.go.
	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

USER root

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

// TestDockerfileGenBuildMirrorOnly locks the contract that the chart-wide
// build-time apt mirror (KANIKO_BUILD_APT_MIRROR / generateDockerfile's
// buildMirror argument), when set without the per-CR user mirror,
// installs an /etc/apt/apt.conf.d/99-build-mirror drop-in pointing at
// /tmp/build-sources.list early in the Dockerfile, and cleans both files
// up before ENTRYPOINT/CMD. Because the drop-in lives in apt's config
// dir, every apt-get during build (including apt-get inside
// cr.Spec.Shell, covered by TestDockerfileGenBuildMirrorCoversShell)
// picks it up transparently. SourceParts is pinned to /dev/null so
// noble's deb822 ubuntu.sources isn't read in parallel.
func TestDockerfileGenBuildMirrorOnly(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage: "ubuntu:24.04",
			APT: &v1alpha1.AptConfig{
				Install: []string{"tini", "podman"},
			},
		},
	}

	got := generateDockerfile(cr, "http://mirrors.163.com/ubuntu")

	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

USER root

RUN ` + imageOSCheckCommand + `

RUN . /etc/os-release && printf "deb http://mirrors.163.com/ubuntu $VERSION_CODENAME main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-updates main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-security main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-backports main restricted universe multiverse\n\
" > /tmp/build-sources.list && \
    printf 'Dir::Etc::SourceList "/tmp/build-sources.list";\nDir::Etc::SourceParts "/dev/null";\n' > /etc/apt/apt.conf.d/99-build-mirror

RUN apt-get -q update && apt-get -q install -y \
    -o Dpkg::Options::="--force-confdef" \
    -o Dpkg::Options::="--force-confold" \
    tini \
    podman \
    && rm -rf /var/lib/apt/lists/*

RUN rm -f /etc/apt/apt.conf.d/99-build-mirror /tmp/build-sources.list

`

	if got != want {
		t.Errorf("Dockerfile mismatch with build mirror only:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDockerfileGenBuildMirrorAndUserMirror locks the contract that when
// BOTH knobs are set, the user mirror still writes to /etc/apt (so it
// bakes into the final image), and the build-mirror apt.conf.d drop-in
// redirects all apt-get traffic during build to the admin mirror via
// the temp /tmp/build-sources.list. The cleanup RUN removes the drop-in
// and the temp list, so the produced image's /etc/apt/sources.list
// reflects the user's mirror only; the build itself fetched via the
// admin mirror.
func TestDockerfileGenBuildMirrorAndUserMirror(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage: "ubuntu:24.04",
			APT: &v1alpha1.AptConfig{
				Mirror:  "http://10.11.32.173/ubuntu",
				Install: []string{"tini", "podman"},
			},
		},
	}

	got := generateDockerfile(cr, "http://mirrors.163.com/ubuntu")

	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

USER root

RUN ` + imageOSCheckCommand + `

RUN . /etc/os-release && printf "deb http://mirrors.163.com/ubuntu $VERSION_CODENAME main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-updates main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-security main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-backports main restricted universe multiverse\n\
" > /tmp/build-sources.list && \
    printf 'Dir::Etc::SourceList "/tmp/build-sources.list";\nDir::Etc::SourceParts "/dev/null";\n' > /etc/apt/apt.conf.d/99-build-mirror

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

RUN rm -f /etc/apt/apt.conf.d/99-build-mirror /tmp/build-sources.list

`

	if got != want {
		t.Errorf("Dockerfile mismatch with build mirror + user mirror:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDockerfileGenBuildMirrorCoversShell locks the contract that the
// build-time apt mirror covers apt-get invocations the user wrote
// directly in cr.Spec.Shell (the realistic case where someone bypasses
// cr.Spec.APT.Install). Previously the mirror was only injected via
// per-command flags in the apt-install branch, leaving spec.shell apt-get
// to fall back to /etc/apt/sources.list (the base default or
// cr.Spec.APT.Mirror's rewrite). The apt.conf.d drop-in installed by the
// early setup RUN is global, so the shell-emitted RUN below sees no
// extra plumbing and apt picks the mirror up transparently.
func TestDockerfileGenBuildMirrorCoversShell(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage: "ubuntu:24.04",
			Shell: []v1alpha1.ShellStep{
				{Run: "apt-get update && apt-get install -y curl"},
			},
		},
	}

	got := generateDockerfile(cr, "http://mirrors.163.com/ubuntu")

	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

USER root

RUN ` + imageOSCheckCommand + `

RUN . /etc/os-release && printf "deb http://mirrors.163.com/ubuntu $VERSION_CODENAME main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-updates main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-security main restricted universe multiverse\n\
deb http://mirrors.163.com/ubuntu $VERSION_CODENAME-backports main restricted universe multiverse\n\
" > /tmp/build-sources.list && \
    printf 'Dir::Etc::SourceList "/tmp/build-sources.list";\nDir::Etc::SourceParts "/dev/null";\n' > /etc/apt/apt.conf.d/99-build-mirror

RUN apt-get update && apt-get install -y curl

RUN rm -f /etc/apt/apt.conf.d/99-build-mirror /tmp/build-sources.list

`

	if got != want {
		t.Errorf("Dockerfile mismatch with build mirror covering spec.shell:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDockerfileGenRunAsUser locks the contract that RunAsUser emits
// exactly two lines -- a getent-passwd existence check that fails
// with the user-facing "User <name> does not exist!" message, then
// USER <name> -- positioned after every build step and immediately
// before ENTRYPOINT/CMD. The build itself stays on root (the
// post-SHELL `USER root` pin); only the final image's runtime user
// changes.
func TestDockerfileGenRunAsUser(t *testing.T) {
	cr := &v1alpha1.ImagePatch{
		Spec: v1alpha1.ImagePatchSpec{
			BaseImage:  "ubuntu:24.04",
			RunAsUser:  "jovyan",
			Entrypoint: []string{"/usr/local/bin/start.sh"},
		},
	}

	got := GenerateDockerfile(cr)

	want := `FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

USER root

RUN ` + imageOSCheckCommand + `

RUN getent passwd jovyan >/dev/null 2>&1 || { echo "User jovyan does not exist!" >&2; exit 1; }
USER jovyan

ENTRYPOINT ["/usr/local/bin/start.sh"]
`
	if got != want {
		t.Errorf("Dockerfile mismatch with RunAsUser:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
	// unconditionally after the SHELL pin and `USER root` -- before
	// COPY --from and any user-supplied shell step. See
	// imagepatch_controller.go.
	want := `FROM lunalabs-acr-registry.cn-guangzhou.cr.aliyuncs.com/luna/devpod-rootfs:0.1.0 AS rootfs

FROM ubuntu:24.04

SHELL ["/bin/sh", "-c"]

USER root

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
