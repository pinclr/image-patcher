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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
)

// specHashLen is the prefix length we surface as the dedup key. 12 hex
// chars = 48 bits. Birthday-paradox collision needs ~16M distinct specs
// before a 50% chance; well above any realistic patch fleet.
const specHashLen = 12

// ComputeSpecHash returns a deterministic short hex hash over every
// ImagePatchSpec field that affects the produced image. baseDigest is
// the resolved immutable digest of Spec.BaseImage; passing it in
// instead of Spec.BaseImage lets the caller turn mutable tags into
// stable content references before hashing. If baseDigest is empty
// the hash falls back to Spec.BaseImage verbatim (caller accepts the
// risk of false hits when the upstream tag changes content).
func ComputeSpecHash(spec *omsv1alpha1.ImagePatchSpec, baseDigest string) string {
	if spec == nil {
		return ""
	}
	base := baseDigest
	if base == "" {
		base = spec.BaseImage
	}

	// Sort ENV by key so map iteration order doesn't affect the hash.
	envKeys := make([]string, 0, len(spec.ENV))
	for k := range spec.ENV {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	envPairs := make([][2]string, 0, len(envKeys))
	for _, k := range envKeys {
		envPairs = append(envPairs, [2]string{k, spec.ENV[k]})
	}

	// The struct's field order doubles as the canonical hash order;
	// json tags use lowercase keys so encoder output is stable.
	input := struct {
		Base       string                `json:"base"`
		APT        *omsv1alpha1.AptConfig  `json:"apt,omitempty"`
		PIP        *omsv1alpha1.PipConfig  `json:"pip,omitempty"`
		Shell      []omsv1alpha1.ShellStep `json:"shell,omitempty"`
		FromImages []omsv1alpha1.FromImage `json:"fromImages,omitempty"`
		Env        [][2]string             `json:"env,omitempty"`
		User       *omsv1alpha1.UserConfig `json:"user,omitempty"`
		Entrypoint []string                `json:"entrypoint,omitempty"`
		CMD        []string                `json:"cmd,omitempty"`
	}{
		Base:       base,
		APT:        spec.APT,
		PIP:        spec.PIP,
		Shell:      spec.Shell,
		FromImages: spec.FromImages,
		Env:        envPairs,
		User:       spec.User,
		Entrypoint: spec.Entrypoint,
		CMD:        spec.CMD,
	}

	raw, err := json.Marshal(input)
	if err != nil {
		// json.Marshal only fails on cycles/unsupported types -- none of
		// the spec fields qualify, so an error here is a programming bug,
		// not a runtime condition. Fall back to a sentinel so we don't
		// silently dedup-collide unrelated specs.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:specHashLen]
}
