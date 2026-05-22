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
// the problem unambiguous: the first three blame user input, the fourth
// is the "contact support" bucket.
//
// Deliberately a small fixed set -- false positives here either mis-blame
// the user for our outages or tell our ops team to ignore something they
// should be paged for. Anything we can't confidently pin on user input
// falls through to FailureLabelControllerInternalError.
const (
	FailureLabelBaseImageNotFound       = "BaseImageNotFound"
	FailureLabelAuthorizationNeeded     = "AuthorizationNeeded"
	FailureLabelNetworkError            = "NetworkError"
	FailureLabelControllerInternalError = "ControllerInternalError"
)

// IsKnownFailureLabel reports whether s is one of the FailureLabel*
// constants -- i.e. a message that was already produced by a run of
// classifyBuildFailure and shouldn't be re-classified. The Job's build
// Pod is eventually garbage-collected, after which classifyBuildFailure
// can no longer read its logs and would *downgrade* an accurate label
// to ControllerInternalError. Treating known labels as sticky keeps
// classifications stable across re-reconciles.
//
// Empty strings, the legacy hard-coded "Build failed" message, and any
// other free-form text all return false so they get (re-)classified on
// the next reconcile -- which is also what backfills CRs that were
// written by an older version of this controller.
func IsKnownFailureLabel(s string) bool {
	switch s {
	case FailureLabelBaseImageNotFound,
		FailureLabelAuthorizationNeeded,
		FailureLabelNetworkError,
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
//  1. Auth is checked first. Private-registry pulls typically surface
//     BOTH a 401 AND a MANIFEST_UNKNOWN line; the 401 is the actionable
//     one ("your secret can't reach this registry") and BaseImageNotFound
//     would mis-blame the user for a tag they got right.
//  2. BaseImageNotFound next -- by this point auth has been ruled out so
//     a manifest miss really does mean "no such image".
//  3. NetworkError last among the positive matches; some Kaniko network
//     failures (e.g. proxy 407) contain "unauthorized" but the real
//     fix is operator-side network config, not user creds -- we accept
//     that mis-classification as the rare case.
//  4. Everything else -> ControllerInternalError.
func classifyLogTail(logTail string) string {
	low := strings.ToLower(logTail)

	// Auth-boundary failures. Covers private base-image pulls without
	// creds AND patched-image push into a registry whose Secret the
	// controller can't reach.
	for _, kw := range []string{
		"unauthorized",
		"authentication required",
		"denied: requested access",
		"no basic auth credentials",
		"401 unauthorized",
	} {
		if strings.Contains(low, kw) {
			return FailureLabelAuthorizationNeeded
		}
	}

	// Image-not-in-registry. The "manifest for X not found" form is
	// split-line in some registry implementations, so we check the two
	// halves separately.
	if strings.Contains(low, "manifest unknown") ||
		strings.Contains(low, "manifest_unknown") ||
		strings.Contains(low, "not found: manifest") ||
		(strings.Contains(low, "manifest for ") && strings.Contains(low, "not found")) {
		return FailureLabelBaseImageNotFound
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
