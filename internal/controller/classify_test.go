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

import "testing"

func TestClassifyLogTail(t *testing.T) {
	// Inputs are abbreviated log tails representative of the real Kaniko /
	// apt / curl error blocks we want to distinguish. Each row should
	// exercise exactly one rule of the classifier. Both the auth-required
	// and the manifest/repo-not-found families collapse onto
	// FailureLabelInvalidImage by design -- see classify.go for the
	// rationale.
	cases := []struct {
		name string
		log  string
		want string
	}{
		// --- InvalidImage: registry-side "no such image OR not authorized" ---
		{
			name: "kaniko manifest unknown",
			log:  `error building image: GET https://registry.example.com/v2/foo/manifests/nosuch: MANIFEST_UNKNOWN: manifest unknown`,
			want: FailureLabelInvalidImage,
		},
		{
			name: "docker hub manifest not found",
			log:  `error: manifest for ubuntu:nosuchtag not found`,
			want: FailureLabelInvalidImage,
		},
		{
			// Real Kaniko log captured against an internal Harbor when
			// the user typoed a base-image path. The decisive line is
			// "NOT_FOUND: repository <name> not found" -- a Harbor /
			// OCI distribution code, distinct from the docker-hub
			// "manifest for X not found" form.
			name: "harbor repository not_found (real kaniko output)",
			log: `INFO[0000] Retrieving image manifest registry.luna.ogpu.cloud/luna/ubuntu-25.04:latest
INFO[0000] Retrieving image registry.luna.ogpu.cloud/luna/ubuntu-25.04:latest from registry registry.luna.ogpu.cloud
ERRO[0000] Error while retrieving image from cache: registry.luna.ogpu.cloud/luna/ubuntu-25.04:latest unable to complete operation after 0 attempts, last error: GET https://registry.luna.ogpu.cloud/v2/luna/ubuntu-25.04/manifests/latest: NOT_FOUND: repository luna/ubuntu-25.04 not found
error building image: unable to complete operation after 0 attempts, last error: GET https://registry.luna.ogpu.cloud/v2/luna/ubuntu-25.04/manifests/latest: NOT_FOUND: repository luna/ubuntu-25.04 not found`,
			want: FailureLabelInvalidImage,
		},
		{
			name: "oci NAME_UNKNOWN repository miss",
			log:  `error pulling image: NAME_UNKNOWN: repository name not known to registry`,
			want: FailureLabelInvalidImage,
		},
		{
			name: "registry 401 unauthorized",
			log:  `unauthorized: authentication required`,
			want: FailureLabelInvalidImage,
		},
		{
			name: "registry push denied",
			log:  `denied: requested access to the resource is denied`,
			want: FailureLabelInvalidImage,
		},
		{
			name: "no basic auth creds",
			log:  `no basic auth credentials`,
			want: FailureLabelInvalidImage,
		},
		{
			// Pre-merge this case verified Auth-wins-over-NotFound
			// priority; post-merge both halves collapse to InvalidImage
			// so it's just a sanity check that mixed signals still
			// land on the right label.
			name: "private registry surfaces both auth and manifest",
			log: `error checking push permissions -- HEAD https://private.io/v2/x: UNAUTHORIZED: authentication required
GET https://private.io/v2/x/manifests/latest: MANIFEST_UNKNOWN: manifest unknown`,
			want: FailureLabelInvalidImage,
		},

		// --- NetworkError: DNS / TCP / proxy failures (operator-side) ---
		{
			name: "apt mirror unreachable",
			log:  `E: Failed to fetch http://mirror.example.com/ubuntu/dists/jammy/InRelease  Temporary failure in name resolution`,
			want: FailureLabelNetworkError,
		},
		{
			name: "curl dns failure",
			log:  `curl: (6) Could not resolve host: files.internal`,
			want: FailureLabelNetworkError,
		},
		{
			name: "curl connection refused",
			log:  `curl: (7) Failed to connect to files.internal port 80: Connection refused`,
			want: FailureLabelNetworkError,
		},
		{
			name: "curl timeout",
			log:  `curl: (28) Operation timed out after 30000 milliseconds`,
			want: FailureLabelNetworkError,
		},
		{
			name: "i/o timeout in go http client",
			log:  `dial tcp 10.0.0.1:443: i/o timeout`,
			want: FailureLabelNetworkError,
		},

		// --- ImageOSNotSupported: exit-42 from the built-in check-os guard ---
		{
			// The shape kaniko prints on real check-os failure: its
			// terminal `error building image:` line carries the
			// structural `waiting for process to exit: exit status 42`
			// suffix from the failed RUN.
			name: "check-os exit 42 (real kaniko terminal line)",
			log:  `error building image: error building stage: failed to execute command: waiting for process to exit: exit status 42`,
			want: FailureLabelImageOSNotSupported,
		},
		{
			// Priority: exit 42 outranks every other rule. Even if a
			// 401 also sits in the same log tail (won't happen in
			// practice, but tail length isn't strictly bounded), the
			// exit-42 line wins.
			name: "check-os exit 42 outranks unauthorized",
			log: `unauthorized: authentication required
error building image: error building stage: failed to execute command: waiting for process to exit: exit status 42`,
			want: FailureLabelImageOSNotSupported,
		},
		{
			// Boundary: kaniko's `\b` boundaries reject "exit status
			// 420" / "exit status 4242" -- otherwise an unrelated
			// failure that happens to print a 4-digit code starting
			// with 42 would mis-label.
			name: "exit status 420 must not match exit-42 rule",
			log:  `error building image: error building stage: failed to execute command: waiting for process to exit: exit status 420`,
			want: FailureLabelControllerInternalError,
		},
		{
			// Real kaniko log captured against a Noble (24.04, supported)
			// base where check-os silently passed but the downstream
			// apt-get install hit held broken packages. The exit-status
			// suffix is 100 (apt-get's "unable to correct"), not 42, so
			// the classifier falls through to ControllerInternalError --
			// which is what apt-conflicts deserve (users can't fix
			// upstream mirror state; needs ops attention).
			//
			// This is the exact regression that motivated dropping the
			// printed marker: with the old `Contains("ImageOSNotSupported:")`
			// rule the four INFO lines below all carried that substring
			// (kaniko echoes every RUN's command body verbatim) and
			// false-triggered the label.
			name: "kaniko INFO echoing check-os body must not false-trigger when downstream RUN failed",
			log: `INFO[0000] No cached layer found for cmd RUN [ -r /etc/os-release ] || exit 42; . /etc/os-release; [ "$ID" = "ubuntu" ] || exit 42; case "$VERSION_ID" in 20.04|22.04|24.04|26.04) ;; *) exit 42 ;; esac
INFO[0299] RUN [ -r /etc/os-release ] || exit 42; . /etc/os-release; [ "$ID" = "ubuntu" ] || exit 42; esac
INFO[0330] Args: [-c [ -r /etc/os-release ] || exit 42]
INFO[0330] Running: [/bin/sh -c [ -r /etc/os-release ] || exit 42]
INFO[0336] RUN apt-get -q update && apt-get -q install -y tini fuse-overlayfs crun openssh-server supervisor curl ca-certificates
The following packages have unmet dependencies:
 openssh-server : Depends: openssh-client (= 1:9.6p1-3ubuntu13) but 1:9.6p1-3ubuntu13.12 is to be installed
E: Unable to correct problems, you have held broken packages.
error building image: error building stage: failed to execute command: waiting for process to exit: exit status 100`,
			want: FailureLabelControllerInternalError,
		},
		{
			// Real kaniko log captured against a from-scratch base
			// (registry.luna.ogpu.cloud/luna/test-scratch:latest).
			// The scratch image has no /bin/sh, so the FIRST RUN
			// kaniko tries -- image-patcher's APT mirror rewrite,
			// emitted before any user-defined shell step -- fails
			// before oms-controller's check-os guard ever runs. The
			// sentinel string is therefore absent and we fall back
			// to matching kaniko's fork/exec error verbatim.
			name: "kaniko fork/exec on scratch base (real log)",
			log: `INFO[0000] RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources && . /etc/os-release && echo "deb http://mirror ..." > /etc/apt/sources.list
INFO[0000] Cmd: /bin/sh
INFO[0000] Args: [-c rm -f /etc/apt/sources.list ...]
INFO[0000] Running: [/bin/sh -c rm -f /etc/apt/sources.list ...]
error building image: error building stage: failed to execute command: starting command: fork/exec /bin/sh: no such file or directory`,
			want: FailureLabelImageOSNotSupported,
		},
		{
			// If anyone pins SHELL to bash and the base still lacks
			// it (or has /bin/sh but not /bin/bash), the same family
			// of error fires with /bin/bash. Same bucket.
			name: "kaniko fork/exec on missing bash",
			log:  `error building image: ... fork/exec /bin/bash: no such file or directory`,
			want: FailureLabelImageOSNotSupported,
		},
		{
			// Runtime-layer form. Today kaniko exec'es directly via
			// Go, so this string doesn't fire from real kaniko logs;
			// kept as a forward-compat match in case the runner ever
			// delegates to runc / containerd, which use this wording.
			name: "runc-style executable not found",
			log:  `OCI runtime create failed: ... executable file not found in $PATH: unknown`,
			want: FailureLabelImageOSNotSupported,
		},

		// --- ControllerInternalError: no usable signal -> contact-support bucket ---
		{
			name: "oom killed -> unknown",
			log:  `command terminated with exit code 137`,
			want: FailureLabelControllerInternalError,
		},
		{
			name: "empty log -> unknown",
			log:  ``,
			want: FailureLabelControllerInternalError,
		},
		{
			name: "garbage log -> unknown",
			log:  `the quick brown fox`,
			want: FailureLabelControllerInternalError,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyLogTail(c.log)
			if got != c.want {
				t.Errorf("classifyLogTail(%q) = %q, want %q", c.log, got, c.want)
			}
		})
	}
}

// TestIsKnownFailureLabel locks down the sticky-classification contract:
// the three FailureLabel* values are sticky so re-reconciles don't
// re-derive (and possibly downgrade) them, while legacy / empty /
// free-form messages fall through to fresh classification -- the
// backfill path for CRs left by an older controller. The legacy
// pre-merge labels "BaseImageNotFound" and "AuthorizationNeeded" land
// in the not-known bucket on purpose so CRs that were classified
// under the old (split) scheme auto-upgrade to InvalidImage on the
// next reconcile after this release rolls out.
func TestIsKnownFailureLabel(t *testing.T) {
	known := []string{
		FailureLabelInvalidImage,
		FailureLabelNetworkError,
		FailureLabelImageOSNotSupported,
		FailureLabelControllerInternalError,
	}
	for _, s := range known {
		if !IsKnownFailureLabel(s) {
			t.Errorf("IsKnownFailureLabel(%q) = false, want true", s)
		}
	}
	unknown := []string{
		"",
		"Build failed",
		"Build completed successfully",
		"some random text",
		"invalidimage", // case-sensitive on purpose
		// Legacy labels from before the InvalidImage merge; must read
		// as not-known so handleExistingJob re-runs the classifier and
		// migrates the CR forward.
		"BaseImageNotFound",
		"AuthorizationNeeded",
	}
	for _, s := range unknown {
		if IsKnownFailureLabel(s) {
			t.Errorf("IsKnownFailureLabel(%q) = true, want false", s)
		}
	}
}
