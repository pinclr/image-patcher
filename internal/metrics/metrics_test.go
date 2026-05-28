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

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		ref          string
		wantRegistry string
		wantImage    string
	}{
		{"registry.luna.ogpu.cloud/patched-images/ubuntu-22.04-patch:latest", "registry.luna.ogpu.cloud", "patched-images/ubuntu-22.04-patch:latest"},
		{"localhost:5000/foo:bar", "localhost:5000", "foo:bar"},
		{"localhost/foo:bar", "localhost", "foo:bar"},
		{"ubuntu-22.04-patch:latest", "docker.io", "ubuntu-22.04-patch:latest"},
		{"library/ubuntu:24.04", "docker.io", "library/ubuntu:24.04"},
		{"", "docker.io", ""},
	}
	for _, tc := range cases {
		gotR, gotI := SplitImageRef(tc.ref)
		if gotR != tc.wantRegistry || gotI != tc.wantImage {
			t.Errorf("SplitImageRef(%q) = (%q, %q), want (%q, %q)",
				tc.ref, gotR, gotI, tc.wantRegistry, tc.wantImage)
		}
	}
}

func TestRecordBuildResult_IncrementsCounter(t *testing.T) {
	buildsTotal.Reset()
	buildDurationSeconds.Reset()
	e2eSeconds.Reset()

	crCreated := time.Now().Add(-2 * time.Minute)
	jobStart := time.Now().Add(-90 * time.Second)
	jobEnd := jobStart.Add(90 * time.Second)
	RecordBuildResult(ResultSucceeded, "registry.example.com/app:v1", "ubuntu:22.04", FailureReasonNone,
		false /*buildCacheHit*/, true /*layerCacheHit*/, false /*canary*/,
		crCreated, jobStart, jobEnd)

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultSucceeded, "registry.example.com", "app:v1", "ubuntu:22.04", FailureReasonNone, "false", "true", "false",
	))
	if got != 1 {
		t.Errorf("builds_total{...,build_cache_hit=false,build_layer_cache_hit=true,canary=false} = %v, want 1", got)
	}

	if n := testutil.CollectAndCount(buildDurationSeconds); n != 1 {
		t.Errorf("build_duration_seconds series count = %d, want 1", n)
	}
	if n := testutil.CollectAndCount(e2eSeconds); n != 1 {
		t.Errorf("e2e_seconds series count = %d, want 1", n)
	}
}

func TestRecordBuildResult_SkipsBuildDurationOnZeroJobStart(t *testing.T) {
	buildsTotal.Reset()
	buildDurationSeconds.Reset()
	e2eSeconds.Reset()

	crCreated := time.Now().Add(-30 * time.Second)
	RecordBuildResult(ResultFailed, "registry.example.com/app:v1", "ubuntu:22.04", FailureReasonBuild,
		false, true, false,
		crCreated, time.Time{}, time.Now())

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultFailed, "registry.example.com", "app:v1", "ubuntu:22.04", FailureReasonBuild, "false", "true", "false",
	))
	if got != 1 {
		t.Errorf("builds_total counter not incremented when jobStart is zero: got %v", got)
	}
	if n := testutil.CollectAndCount(buildDurationSeconds); n != 0 {
		t.Errorf("build_duration_seconds observed despite zero jobStart: series count = %d", n)
	}
	// e2e is independent of jobStart.
	if n := testutil.CollectAndCount(e2eSeconds); n != 1 {
		t.Errorf("e2e_seconds should be recorded even when jobStart is zero; series count = %d", n)
	}
}

func TestRecordBuildResult_BuildCacheHitObservesZeroAndE2E(t *testing.T) {
	buildsTotal.Reset()
	buildDurationSeconds.Reset()
	e2eSeconds.Reset()

	crCreated := time.Now().Add(-3 * time.Second)
	RecordBuildResult(ResultSucceeded, "registry.example.com/app:v1", "ubuntu:22.04", FailureReasonNone,
		true /*buildCacheHit*/, true, false,
		crCreated, time.Time{}, time.Time{})

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultSucceeded, "registry.example.com", "app:v1", "ubuntu:22.04", FailureReasonNone, "true", "true", "false",
	))
	if got != 1 {
		t.Errorf("builds_total{build_cache_hit=true} = %v, want 1", got)
	}
	// build_duration is observed as 0 on hit so dashboards see a flat
	// line at the hit cadence (vs a hole when we skipped entirely).
	if n := testutil.CollectAndCount(buildDurationSeconds); n != 1 {
		t.Errorf("build_duration_seconds should observe 0 on build cache hit; series count = %d, want 1", n)
	}
	if n := testutil.CollectAndCount(e2eSeconds); n != 1 {
		t.Errorf("e2e_seconds should be recorded on build cache hit (fast path); series count = %d, want 1", n)
	}
}

func TestRecordBuildResult_LayerCacheHitFalseLabel(t *testing.T) {
	buildsTotal.Reset()
	buildDurationSeconds.Reset()

	jobStart := time.Now().Add(-30 * time.Second)
	RecordBuildResult(ResultSucceeded, "registry.example.com/app:v1", "ubuntu:22.04", FailureReasonNone,
		false, false /*layerCacheHit=false -> CR opted out*/, false,
		jobStart.Add(-time.Second), jobStart, time.Now())

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultSucceeded, "registry.example.com", "app:v1", "ubuntu:22.04", FailureReasonNone, "false", "false", "false",
	))
	if got != 1 {
		t.Errorf("builds_total{build_layer_cache_hit=false} = %v, want 1", got)
	}
}

func TestRecordBuildResult_CanaryLabel(t *testing.T) {
	buildsTotal.Reset()

	jobStart := time.Now().Add(-30 * time.Second)
	RecordBuildResult(ResultSucceeded, "registry.example.com/app:v1", "ubuntu:22.04", FailureReasonNone,
		false, true, true /*canary*/,
		jobStart.Add(-time.Second), jobStart, time.Now())

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultSucceeded, "registry.example.com", "app:v1", "ubuntu:22.04", FailureReasonNone, "false", "true", "true",
	))
	if got != 1 {
		t.Errorf("builds_total{canary=true} = %v, want 1", got)
	}
}

func TestRecordReconcileFailure(t *testing.T) {
	reconcileFailuresTotal.Reset()

	RecordReconcileFailure(ReasonJobCreate)
	RecordReconcileFailure(ReasonJobCreate)
	RecordReconcileFailure(ReasonStatusUpdate)

	if got := testutil.ToFloat64(reconcileFailuresTotal.WithLabelValues(ReasonJobCreate)); got != 2 {
		t.Errorf("reconcile_failures_total{reason=job_create} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(reconcileFailuresTotal.WithLabelValues(ReasonStatusUpdate)); got != 1 {
		t.Errorf("reconcile_failures_total{reason=status_update} = %v, want 1", got)
	}
}
