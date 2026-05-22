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
	"encoding/json"
	"os"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
)

// KanikoResourcesFromEnv reads the per-Kaniko-build container resource
// requirements from the KANIKO_RESOURCES env variable, populated by the
// chart's deployment template. The value is a JSON-encoded
// corev1.ResourceRequirements (e.g. {"requests":{"ephemeral-storage":"10Gi"}}).
//
// An empty / unset env returns the zero value, which leaves Resources
// unset on the build container -- backwards-compatible with releases
// that didn't carry this knob.
//
// A malformed value is logged and treated as empty; we never block
// controller startup on a parse failure because losing scheduling
// hints is preferable to halting reconciliation.
func KanikoResourcesFromEnv(l logr.Logger) corev1.ResourceRequirements {
	raw := os.Getenv("KANIKO_RESOURCES")
	if raw == "" {
		return corev1.ResourceRequirements{}
	}
	var out corev1.ResourceRequirements
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		l.Error(err, "invalid KANIKO_RESOURCES JSON; ignoring", "raw", raw)
		return corev1.ResourceRequirements{}
	}
	return out
}
