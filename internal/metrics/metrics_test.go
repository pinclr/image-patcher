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

	start := time.Now().Add(-90 * time.Second)
	end := start.Add(90 * time.Second)
	RecordBuildResult(ResultSucceeded, "registry.example.com/app:v1", FailureReasonNone, false, start, end)

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultSucceeded, "registry.example.com", "app:v1", FailureReasonNone, "false",
	))
	if got != 1 {
		t.Errorf("builds_total{result=succeeded,registry=registry.example.com,image=app:v1,failure_reason=none,dedup_hit=false} = %v, want 1", got)
	}

	if n := testutil.CollectAndCount(buildDurationSeconds); n != 1 {
		t.Errorf("build_duration_seconds series count = %d, want 1", n)
	}
}

func TestRecordBuildResult_SkipsHistogramOnZeroStart(t *testing.T) {
	buildsTotal.Reset()
	buildDurationSeconds.Reset()

	RecordBuildResult(ResultFailed, "registry.example.com/app:v1", FailureReasonBuild, false, time.Time{}, time.Now())

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultFailed, "registry.example.com", "app:v1", FailureReasonBuild, "false",
	))
	if got != 1 {
		t.Errorf("builds_total counter not incremented when StartTime is zero: got %v", got)
	}
	if n := testutil.CollectAndCount(buildDurationSeconds); n != 0 {
		t.Errorf("build_duration_seconds observed despite zero StartTime: series count = %d", n)
	}
}

func TestRecordBuildResult_DedupHitSkipsHistogram(t *testing.T) {
	buildsTotal.Reset()
	buildDurationSeconds.Reset()

	RecordBuildResult(ResultSucceeded, "registry.example.com/app:v1", FailureReasonNone, true, time.Now().Add(-time.Minute), time.Now())

	got := testutil.ToFloat64(buildsTotal.WithLabelValues(
		ResultSucceeded, "registry.example.com", "app:v1", FailureReasonNone, "true",
	))
	if got != 1 {
		t.Errorf("builds_total{dedup_hit=true} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(buildDurationSeconds); n != 0 {
		t.Errorf("build_duration_seconds observed on dedup hit: series count = %d, want 0", n)
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
