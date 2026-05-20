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
			Help:      "Image builds that reached a terminal state, by result and target image.",
		},
		[]string{"result", "registry", "image"},
	)

	buildDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "build_duration_seconds",
			Help:      "Wall time from Kaniko Job startTime to the terminal transition observed by the reconciler.",
			Buckets:   buildDurationBuckets,
		},
		[]string{"result", "registry", "image"},
	)

	reconcileFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_failures_total",
			Help:      "Reconciler-side errors broken down by the operation that failed.",
		},
		[]string{"reason"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		buildsTotal,
		buildDurationSeconds,
		reconcileFailuresTotal,
	)
}

// RecordBuildResult records one terminal build transition. Callers must only
// invoke this on the *transition* into a terminal phase (the reconciler's
// existing "phase changed" guard), so requeues never double-count.
//
// If startTime is zero (the kubelet has not stamped Job.Status.StartTime
// yet), the duration observation is skipped to avoid recording a misleading
// near-zero value; the counter still increments.
func RecordBuildResult(result, targetImage string, startTime, endTime time.Time) {
	registry, image := SplitImageRef(targetImage)
	labels := prometheus.Labels{"result": result, "registry": registry, "image": image}

	buildsTotal.With(labels).Inc()

	if startTime.IsZero() {
		return
	}
	if endTime.IsZero() {
		endTime = time.Now()
	}
	buildDurationSeconds.With(labels).Observe(endTime.Sub(startTime).Seconds())
}

// RecordReconcileFailure increments the reconciler failure counter for a
// given step. The reason argument must be one of the Reason* constants in
// this package — free-form strings would unbound label cardinality.
func RecordReconcileFailure(reason string) {
	reconcileFailuresTotal.WithLabelValues(reason).Inc()
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
