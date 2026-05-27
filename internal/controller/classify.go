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
	"io"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// FailureLabel* are the user-facing strings written verbatim into
// ImagePatch.Status.Message when a build fails. Downstream (oms-controller
// /devpod/list) surfaces them as `failed: <label>` to the end user, so the
// names are chosen to read well in that context and to make ownership of
// the problem unambiguous: the first two blame user input, the third is
// the "contact support" bucket.
//
// Deliberately a small fixed set -- false positives here either mis-blame
// the user for our outages or tell our ops team to ignore something they
// should be paged for. Anything we can't confidently pin on user input
// falls through to FailureLabelControllerInternalError.
//
// "image-not-in-registry" and "auth required" deliberately collapse onto
// the single FailureLabelInvalidImage: in practice many registries (Harbor
// included) return 404 for unauthorized requests to avoid leaking which
// repos exist, so the two cases are not reliably distinguishable from
// log text alone. The actionable advice in either case is the same --
// "check the image name and your credentials" -- so a single label is
// both more honest and more useful than guessing.
const (
	FailureLabelInvalidImage            = "InvalidImage"
	FailureLabelNetworkError            = "NetworkError"
	FailureLabelImageOSNotSupported     = "ImageOSNotSupported"
	FailureLabelControllerInternalError = "ControllerInternalError"
)

// imageOSExitMarker matches the kaniko terminal-error line emitted when
// the check-os RUN (see imageOSCheckCommand in imagepatch_controller.go)
// exits with our reserved sentinel code 42.
//
// We match on kaniko's structural emission rather than any string the
// guard itself prints. kaniko echoes every RUN's command body verbatim
// into FOUR INFO log lines (`No cached layer found for cmd RUN ...`,
// `RUN ...`, `Args: [-c ...]`, `Running: [/bin/sh -c ...]`), so any
// echo arg in the guard would show up in EVERY build's logs and
// false-trigger the classifier even when check-os silently passed.
// `waiting for process to exit: exit status 42` is written by kaniko's
// own error path on RUN failure with that exit code, not from the
// command body, so the same INFO-echo trap doesn't apply.
//
// 42 is reserved for image-patcher's check-os guard. apt-get returns
// 100, curl 6/7/28, go binaries 0/1/2 -- 42 is essentially unused by
// the toolchain we run during a patch build, so an `exit status 42`
// line in the log can only have come from the guard.
//
// `\b` boundaries prevent matching "exit status 420" / "4242".
var imageOSExitMarker = regexp.MustCompile(`waiting for process to exit: exit status 42\b`)

// IsKnownFailureLabel reports whether s is one of the FailureLabel*
// constants -- i.e. a message produced by a previous run of
// classifyBuildFailure that should be preserved across re-reconciles
// rather than re-derived. Two motivations:
//
//  1. The build Pod is eventually garbage-collected. After that,
//     classifyBuildFailure can no longer read logs and would always
//     return FailureLabelControllerInternalError -- silently downgrading
//     any accurate label sitting on the CR.
//  2. Re-running the matcher on a CR that is already FailureLabelControllerInternalError
//     either returns CIE again (no signal, just wasted log fetches) or
//     "upgrades" it to a different label that the previous run also
//     should have produced -- which is a bug in the matcher, not a
//     runtime concern. The right place to address mis-classification
//     is to widen the high-priority matchers (cf. the Harbor
//     NOT_FOUND: repository pattern added alongside this change), not
//     to keep churning the classifier at runtime hoping for a better
//     answer.
//
// Empty strings, the legacy hard-coded "Build failed" message, and any
// other free-form text fall through to (re-)classification, which is
// how handleExistingJob backfills CRs left by older controllers.
//
// Knock-on: a CR mis-classified by an older release of this code will
// keep its stale label after a controller upgrade. The recovery path
// is operator-side: `kubectl patch imagepatch ... --subresource=status
// --type=merge -p '{"status":{"message":""}}'` (or just delete + recreate
// the devpod).
func IsKnownFailureLabel(s string) bool {
	switch s {
	case FailureLabelInvalidImage,
		FailureLabelNetworkError,
		FailureLabelImageOSNotSupported,
		FailureLabelControllerInternalError:
		return true
	}
	return false
}

// buildPodLogTailLines bounds how much of the Kaniko pod's stdout we pull
// when classifying a failure. Kaniko's terminal error block sits at the
// very end of the log; 200 lines is plenty to capture it without hauling
// down the apt/curl progress chatter from earlier steps.
const buildPodLogTailLines int64 = 200

// classifyBuildFailure inspects the failed Job's most-recent Pod and
// returns one of the FailureLabel* constants. It never errors -- any
// problem reaching the Pod or its logs collapses to
// FailureLabelControllerInternalError, since that's the bucket that
// already means "contact us, we'll look".
//
// kube is allowed to be nil; that path also collapses to
// ControllerInternalError. This keeps the test fixture in
// imagepatch_controller_test.go (which doesn't wire a clientset) working
// unchanged and means a misconfigured deploy degrades to the pre-change
// behavior rather than panicking.
func classifyBuildFailure(ctx context.Context, kube kubernetes.Interface, job *batchv1.Job) string {
	l := log.FromContext(ctx)
	if kube == nil {
		l.Info("classify: no clientset wired, defaulting to ControllerInternalError")
		return FailureLabelControllerInternalError
	}

	// job-name is added by the Job controller to every Pod it spawns and
	// is namespaced to this Job specifically -- safer than the
	// imagepatch=<crname> label the build template also carries, which
	// would also match pods from a previous incarnation of the same CR.
	selector := "job-name=" + job.Name
	pods, err := kube.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		l.Info("classify: no build pod to inspect", "err", err, "selector", selector, "namespace", job.Namespace)
		return FailureLabelControllerInternalError
	}

	// With BackoffLimit=0 (set in constructJob) there is exactly one Pod
	// per Job. Defensive scan anyway -- if anyone ever bumps the backoff
	// we want to classify the *last* attempt, not the first.
	pod := pods.Items[0]
	for i := 1; i < len(pods.Items); i++ {
		if pods.Items[i].CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = pods.Items[i]
		}
	}

	tail := buildPodLogTailLines
	req := kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tail})
	rc, err := req.Stream(ctx)
	if err != nil {
		l.Info("classify: cannot stream pod logs", "err", err, "pod", pod.Name)
		return FailureLabelControllerInternalError
	}
	defer func() { _ = rc.Close() }()

	b, err := io.ReadAll(rc)
	if err != nil {
		l.Info("classify: cannot read pod logs", "err", err, "pod", pod.Name)
		return FailureLabelControllerInternalError
	}

	label := classifyLogTail(string(b))
	l.Info("classify: classified build failure", "label", label, "pod", pod.Name)
	return label
}

// classifyLogTail is the pure-function half of classifyBuildFailure --
// split out so the keyword matrix can be unit-tested without an API
// server. Order matters:
//
//  1. ImageOSNotSupported, exit-42 form: kaniko's structural
//     `waiting for process to exit: exit status 42` line, which only
//     fires when our check-os guard's `exit 42` lands. Highest priority
//     because 42 is reserved for this guard and the line is emitted by
//     kaniko (not by our RUN body), so it can't false-trigger from
//     kaniko's own RUN-body INFO echoes the way a printed marker would.
//  2. ImageOSNotSupported, shell-missing form: kaniko's `fork/exec
//     /bin/sh: no such file or directory` (and variants). Folded into
//     the same label because the user-facing remediation is identical
//     to (1) -- "feed us a base image with a working shell" -- and
//     because for scratch / distroless bases the failure beats our
//     prepended check-os to the punch: there's no /bin/sh, kaniko
//     can't start the guard RUN at all, so we never see exit 42.
//  3. InvalidImage -- union of "auth required at the registry" and
//     "image doesn't exist at the registry". Both share keywords that
//     registries often interchange (Harbor returns 404 for unauthorized
//     requests to avoid info-leak), and the actionable advice is the
//     same on either branch.
//  4. NetworkError -- DNS / TCP / proxy failures. Kept distinct because
//     these are operator-side (proxy / mirror config) and the user has
//     no lever to pull. Order-wise: a rare Kaniko proxy 407 would
//     contain "unauthorized" and get mis-classed as InvalidImage --
//     acceptable, since in our environment 407 is essentially never
//     seen and the surrounding signal is usually clear enough that the
//     operator can still triage from the logs.
//  5. Everything else -> ControllerInternalError.
func classifyLogTail(logTail string) string {
	// Structural exit-code match (see imageOSExitMarker for why we
	// match the kaniko terminal line rather than a substring of the
	// guard's command body). Pre-empts every later rule so an
	// unsupported-OS build doesn't get downgraded into "network error"
	// by an incidental apt-mirror DNS failure that happened earlier
	// in the same log tail.
	if imageOSExitMarker.MatchString(logTail) {
		return FailureLabelImageOSNotSupported
	}

	low := strings.ToLower(logTail)

	// Shell-unrunnable form. GenerateDockerfile pins SHELL to /bin/sh
	// (see imagepatch_controller.go), so every kaniko RUN goes through
	// /bin/sh -- if execve on /bin/sh fails for any reason the build is
	// dead. Anchored on kaniko's full "starting command" error prefix
	// rather than a bare `fork/exec /bin/sh:` substring: the longer
	// form is the structural wording kaniko uses on its exec-failure
	// path (vs. `waiting for process to exit:` for runtime exits), so
	// the match is harder to false-trigger on incidental log content
	// while still covering every trailing reason:
	//
	//   "...fork/exec /bin/sh: no such file or directory"  -- scratch / distroless
	//   "...fork/exec /bin/sh: exec format error"          -- wrong arch
	//   "...fork/exec /bin/sh: <future kaniko wording>"    -- defensive coverage
	//
	// Goes before InvalidImage so it isn't shadowed by an incidental
	// "unauthorized" elsewhere in the tail.
	if strings.Contains(low, "failed to execute command: starting command: fork/exec /bin/sh:") {
		return FailureLabelImageOSNotSupported
	}

	// InvalidImage: registry returned either an auth challenge or a
	// not-found for the requested repo/tag. We treat these as one bucket
	// because:
	//   - Harbor returns 404 (NOT_FOUND) for unauthorized pulls to avoid
	//     leaking which repos exist, so the two cases are not reliably
	//     distinguishable from log text.
	//   - The user-facing remediation is identical: check the image
	//     name AND that your pull credentials cover it.
	//
	// Keywords covered:
	//   - auth-boundary failures (private pull without creds, push
	//     denied):
	//       "unauthorized" / "401 unauthorized"
	//       "authentication required"
	//       "denied: requested access"
	//       "no basic auth credentials"
	//   - not-in-registry across registry implementations:
	//       "MANIFEST_UNKNOWN: manifest unknown"  (OCI standard manifest miss)
	//       "manifest for <ref> not found"        (Docker Hub textual)
	//       "name unknown" / "name_unknown"       (OCI NAME_UNKNOWN)
	//       "NOT_FOUND: repository <name> ..."    (Harbor / distribution 404)
	for _, kw := range []string{
		"unauthorized",
		"authentication required",
		"denied: requested access",
		"no basic auth credentials",
		"401 unauthorized",
		"manifest unknown",
		"manifest_unknown",
		"not found: manifest",
		"name unknown",
		"name_unknown",
		"not_found: repository",
	} {
		if strings.Contains(low, kw) {
			return FailureLabelInvalidImage
		}
	}
	// "manifest for <ref> not found" is split-line in some registry
	// implementations, so we check the two halves separately.
	if strings.Contains(low, "manifest for ") && strings.Contains(low, "not found") {
		return FailureLabelInvalidImage
	}

	// Network / DNS issues -- apt mirror unreachable, curl timing out,
	// kaniko's HTTP client giving up. These are always "our side" to
	// fix (file-server / proxy / mirror config) -- the user has no lever
	// to pull, so we surface them distinctly rather than rolling them
	// into ControllerInternalError.
	for _, kw := range []string{
		"i/o timeout",
		"connection refused",
		"no route to host",
		"temporary failure in name resolution",
		"could not resolve host",
		"could not connect to",
		"curl: (6",
		"curl: (7",
		"curl: (28",
	} {
		if strings.Contains(low, kw) {
			return FailureLabelNetworkError
		}
	}

	return FailureLabelControllerInternalError
}
