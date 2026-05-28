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

// Package metrics defines the Prometheus collectors exposed by the
// image-patch-operator. See docs/design/metrics.md for the design rationale.
package metrics

import (
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	namespace = "image_patcher"

	ResultSucceeded = "succeeded"
	ResultFailed    = "failed"

	ReasonConfigMapApply = "configmap_apply"
	ReasonJobCreate      = "job_create"
	ReasonStatusUpdate   = "status_update"
	ReasonOwnerRef       = "owner_ref"
	ReasonGetJob         = "get_job"

	// FailureReason* are the bounded set of values for the failure_reason
	// label on image_patcher_builds_total. Succeeded transitions always
	// record FailureReasonNone so the label cardinality stays predictable.
	// Anything not matching a specific Job condition reason falls into
	// FailureReasonBuildError -- the bucket where actual Kaniko build
	// failures end up.
	FailureReasonNone     = "none"
	FailureReasonDeadline = "deadline_exceeded"
	FailureReasonBackoff  = "backoff_limit_exceeded"
	FailureReasonBuild    = "build_error"

	// PhaseLabel* are the canonical phase strings used as label values on
	// image_patcher_imagepatches. Mirrors what the controller writes into
	// ImagePatch.Status.Phase, with the empty / unset case normalized to
	// "Pending" so consumers don't need a special-case query for "".
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
)

// buildDurationBuckets is tuned for image-build workloads: apt installs in
// tens of seconds at the low end, large CUDA layers past twenty minutes at
// the high end. Reserved for re-use by deferred phase histograms so they
// stay comparable to build_duration_seconds.
var buildDurationBuckets = []float64{30, 60, 120, 300, 600, 1800, 3600}

var (
	buildsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "builds_total",
			Help:      "Image builds that reached a terminal state, by result, target image, failure reason (none for successes), whether the build was short-circuited via content-addressed dedup (dedup_hit=true means no Kaniko Job ran), whether the CR opted out of Kaniko's RUN-layer cache (build_layer_cache_disabled), and whether the CR is a healthcheck canary (canary=true; sourced from the image-patcher.healthcheck/canary CR label, lets dashboards filter the synthetic load out of production graphs).",
		},
		[]string{"result", "registry", "image", "failure_reason", "dedup_hit", "build_layer_cache_disabled", "canary"},
	)

	buildDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "build_duration_seconds",
			Help:      "Wall time from Kaniko Job startTime to the terminal transition observed by the reconciler. Excludes reconciler queue + Pod scheduling time; for the full CR-creation-to-terminal duration use image_patcher_e2e_seconds.",
			Buckets:   buildDurationBuckets,
		},
		[]string{"result", "registry", "image", "build_layer_cache_disabled", "canary"},
	)

	e2eSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "e2e_seconds",
			Help:      "Wall time from ImagePatch CR creation timestamp to the terminal transition observed by the reconciler. Includes reconciler queue wait, ConfigMap+Job creation, Pod scheduling, Kaniko build, push, and Status.Update. Always observed -- including dedup hits, which typically land in the smallest bucket (registry HEAD+PUT only).",
			Buckets:   buildDurationBuckets,
		},
		[]string{"result", "registry", "image", "dedup_hit", "build_layer_cache_disabled", "canary"},
	)

	reconcileFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_failures_total",
			Help:      "Reconciler-side errors broken down by the operation that failed.",
		},
		[]string{"reason"},
	)

	activeBuilds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "active_builds",
			Help:      "ImagePatch CRs currently in the Running phase (build in flight).",
		},
	)

	imagePatches = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "imagepatches",
			Help:      "ImagePatch CRs in the cluster, broken down by phase (snapshot, refreshed periodically).",
		},
		[]string{"phase"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		buildsTotal,
		buildDurationSeconds,
		e2eSeconds,
		reconcileFailuresTotal,
		activeBuilds,
		imagePatches,
	)
}

// RecordBuildResult records one terminal build transition. Callers must only
// invoke this on the *transition* into a terminal phase (the reconciler's
// existing "phase changed" guard), so requeues never double-count.
//
// Three timestamps drive different histograms:
//
//   - crCreated  -> endTime  : observed on e2e_seconds. Always; covers
//     reconciler queue, ConfigMap+Job creation, Pod scheduling, Kaniko,
//     push, and status update. Dedup hits typically land in the smallest
//     bucket (registry HEAD+PUT only -- no Job).
//
//   - jobStarted -> endTime  : observed on build_duration_seconds. Only
//     when a Kaniko Job actually ran (dedupHit=false AND jobStarted not
//     zero). Pure Kaniko wall time.
//
// failureReason is a bounded enum (FailureReason* constants); pass
// FailureReasonNone for successes. dedupHit / buildLayerCacheDisabled
// / canary become labels so dashboards can split per cache state and
// filter out synthetic canary load.
func RecordBuildResult(result, targetImage, failureReason string, dedupHit, buildLayerCacheDisabled, canary bool, crCreated, jobStarted, endTime time.Time) {
	registry, image := SplitImageRef(targetImage)
	dedupHitLabel := strconv.FormatBool(dedupHit)
	bldLayerLabel := strconv.FormatBool(buildLayerCacheDisabled)
	canaryLabel := strconv.FormatBool(canary)

	buildsTotal.With(prometheus.Labels{
		"result":                     result,
		"registry":                   registry,
		"image":                      image,
		"failure_reason":             failureReason,
		"dedup_hit":                  dedupHitLabel,
		"build_layer_cache_disabled": bldLayerLabel,
		"canary":                     canaryLabel,
	}).Inc()

	if endTime.IsZero() {
		endTime = time.Now()
	}

	if !crCreated.IsZero() {
		e2eSeconds.With(prometheus.Labels{
			"result":                     result,
			"registry":                   registry,
			"image":                      image,
			"dedup_hit":                  dedupHitLabel,
			"build_layer_cache_disabled": bldLayerLabel,
			"canary":                     canaryLabel,
		}).Observe(endTime.Sub(crCreated).Seconds())
	}

	// Kaniko wall time only when a Job actually ran. Skip on dedup hit
	// (no Job) and when the kubelet hasn't stamped startTime yet (early
	// terminal observation; rare).
	if !dedupHit && !jobStarted.IsZero() {
		buildDurationSeconds.With(prometheus.Labels{
			"result":                     result,
			"registry":                   registry,
			"image":                      image,
			"build_layer_cache_disabled": bldLayerLabel,
			"canary":                     canaryLabel,
		}).Observe(endTime.Sub(jobStarted).Seconds())
	}
}

// RecordReconcileFailure increments the reconciler failure counter for a
// given step. The reason argument must be one of the Reason* constants in
// this package — free-form strings would unbound label cardinality.
func RecordReconcileFailure(reason string) {
	reconcileFailuresTotal.WithLabelValues(reason).Inc()
}

// SetImagePatchesByPhase publishes a snapshot of CR counts grouped by
// Status.Phase. Zero-valued phases are still written so a phase that
// drained to zero (e.g. no more Failed CRs) doesn't keep stale samples.
func SetImagePatchesByPhase(counts map[string]int) {
	for _, p := range []string{PhasePending, PhaseRunning, PhaseSucceeded, PhaseFailed} {
		imagePatches.WithLabelValues(p).Set(float64(counts[p]))
	}
	activeBuilds.Set(float64(counts[PhaseRunning]))
}

// SplitImageRef parses a fully-qualified image reference into (registry,
// image) for use as metric labels. The image component includes the tag and
// excludes the registry host, matching the spec in docs/design/metrics.md.
//
// A reference is considered to have an explicit registry host when its first
// path segment contains '.', ':', or equals "localhost"; otherwise it is
// treated as a Docker Hub short name and the registry defaults to
// "docker.io".
func SplitImageRef(ref string) (registry, image string) {
	if ref == "" {
		return "docker.io", ""
	}
	slash := strings.Index(ref, "/")
	if slash == -1 {
		return "docker.io", ref
	}
	head := ref[:slash]
	if head == "localhost" || strings.ContainsAny(head, ".:") {
		return head, ref[slash+1:]
	}
	return "docker.io", ref
}
