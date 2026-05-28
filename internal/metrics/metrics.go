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
// the high end. The leading 0.1 bucket captures build-cache hits (which
// record 0 because no Kaniko Job ran) and dedup-retag paths -- without it
// "no work" lands in the same le=30 bucket as actual 30-second builds and
// percentiles get smeared. Reserved for re-use by deferred phase histograms
// so they stay comparable to build_duration_seconds.
var buildDurationBuckets = []float64{0.1, 30, 60, 120, 300, 600, 1800, 3600}

var (
	buildsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "builds_total",
			Help:      "Image builds that reached a terminal state. Labels: result, registry/image (target), base_image (CR spec.baseImage -- lets dashboards split by Ubuntu version etc.), failure_reason (none for successes), build_cache_hit (controller dedup short-circuit), build_layer_cache_hit (CR allowed Kaniko --cache=true), canary (sourced from image-patcher.healthcheck/canary CR label).",
		},
		[]string{"result", "registry", "image", "base_image", "failure_reason", "build_cache_hit", "build_layer_cache_hit", "canary"},
	)

	buildDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "build_duration_seconds",
			Help:      "Wall time from Kaniko Job startTime to the terminal transition observed by the reconciler. Excludes reconciler queue + Pod scheduling time; for the full CR-creation-to-terminal duration use image_patcher_e2e_seconds. base_image label lets dashboards spot which base is regressing.",
			Buckets:   buildDurationBuckets,
		},
		[]string{"result", "registry", "image", "base_image", "build_layer_cache_hit", "canary"},
	)

	e2eSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "e2e_seconds",
			Help:      "Wall time from ImagePatch CR creation timestamp to the terminal transition observed by the reconciler. Includes reconciler queue wait, ConfigMap+Job creation, Pod scheduling, Kaniko build, push, and Status.Update. Always observed -- including build cache hits (which record 0 for build_duration_seconds but a real value here).",
			Buckets:   buildDurationBuckets,
		},
		[]string{"result", "registry", "image", "base_image", "build_cache_hit", "build_layer_cache_hit", "canary"},
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
// The two histograms use DIFFERENT end timestamps on purpose:
//
//   - e2e_seconds: crCreated -> time.Now() (the moment the reconciler
//     observed the terminal phase and is now recording). Captures
//     reconciler queue wait, Pod scheduling, Kaniko, push, controller
//     polling latency, and status update. Includes everything a user
//     experiences from `kubectl apply` to `Status.Phase=Succeeded`.
//
//   - build_duration_seconds: jobStarted -> jobEnded (Kaniko-only). Pure
//     wall time of the build container. Excludes reconcile / scheduling
//     / controller-observation lag. Skipped when build cache hit (no Job).
//
// Earlier the two used a common endTime (job.CompletionTime), which made
// e2e under-report by up to runningPhaseRequeueAfter (15s) for non-hit
// builds AND made the two curves look nearly identical on dashboards.
//
// failureReason is a bounded enum (FailureReason* constants); pass
// FailureReasonNone for successes. buildCacheHit / buildLayerCacheHit /
// canary become labels for dashboard splits / canary exclusion.
// buildLayerCacheHit is true when the CR allowed Kaniko's --cache=true
// (i.e. did NOT set spec.buildOptions.disableBuildLayerCache).
func RecordBuildResult(result, targetImage, baseImage, failureReason string, buildCacheHit, buildLayerCacheHit, canary bool, crCreated, jobStarted, jobEnded time.Time) {
	registry, image := SplitImageRef(targetImage)
	bcacheLabel := strconv.FormatBool(buildCacheHit)
	blayerLabel := strconv.FormatBool(buildLayerCacheHit)
	canaryLabel := strconv.FormatBool(canary)
	observed := time.Now()

	buildsTotal.With(prometheus.Labels{
		"result":                result,
		"registry":              registry,
		"image":                 image,
		"base_image":            baseImage,
		"failure_reason":        failureReason,
		"build_cache_hit":       bcacheLabel,
		"build_layer_cache_hit": blayerLabel,
		"canary":                canaryLabel,
	}).Inc()

	if !crCreated.IsZero() {
		e2eSeconds.With(prometheus.Labels{
			"result":                result,
			"registry":              registry,
			"image":                 image,
			"base_image":            baseImage,
			"build_cache_hit":       bcacheLabel,
			"build_layer_cache_hit": blayerLabel,
			"canary":                canaryLabel,
		}).Observe(observed.Sub(crCreated).Seconds())
	}

	// Kaniko wall time. When no Job ran (build cache hit), observe 0
	// instead of skipping -- dashboards then see a continuous series
	// pinned at 0 for hit terminals and the actual Kaniko time for
	// fresh builds, making "did the cache save time" directly visible
	// on the same line as the build-time spikes. The 0.1 leading bucket
	// in buildDurationBuckets keeps the 0 samples from smearing
	// percentiles with real 30s builds.
	//
	// When jobStarted is zero (early terminal observation; rare) we
	// still skip -- we don't have a duration to report.
	switch {
	case buildCacheHit:
		buildDurationSeconds.With(prometheus.Labels{
			"result":                result,
			"registry":              registry,
			"image":                 image,
			"base_image":            baseImage,
			"build_layer_cache_hit": blayerLabel,
			"canary":                canaryLabel,
		}).Observe(0)
	case !jobStarted.IsZero():
		end := jobEnded
		if end.IsZero() {
			end = observed
		}
		buildDurationSeconds.With(prometheus.Labels{
			"result":                result,
			"registry":              registry,
			"image":                 image,
			"base_image":            baseImage,
			"build_layer_cache_hit": blayerLabel,
			"canary":                canaryLabel,
		}).Observe(end.Sub(jobStarted).Seconds())
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
