/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ErrPlatformNotSupported is returned by RejectIfPlatformMismatch when
// we have positive evidence the image's platform set does NOT include
// the build target. Wrapped with detail about what was actually seen
// so callers can put the message straight into a status field.
//
// The sentinel value "ImageOSNotSupported" intentionally matches the
// classify.go FailureLabel constant of the same name -- callers that
// set ImagePatch.Status.Message from this error get a message that
// IsKnownFailureLabel recognises as sticky, so re-reconciles preserve
// the label.
var ErrPlatformNotSupported = errors.New("ImageOSNotSupported")

// RejectIfPlatformMismatch inspects the registry manifest at ref and
// returns ErrPlatformNotSupported (wrapped with detail) when we KNOW
// the image cannot run on (targetOS, targetArch). Otherwise returns
// nil. Three-way semantics:
//
//  1. Confirmed match -- single-platform image whose config OS/arch
//     match the target, or multi-arch index that contains a matching
//     entry. Returns nil.
//  2. Confirmed mismatch -- single-platform image whose config doesn't
//     match, or multi-arch index with no matching entry. Returns
//     ErrPlatformNotSupported wrapping a human-readable detail.
//  3. Cannot determine -- registry HTTP error (network, auth, 404),
//     manifest parse failure, malformed reference. Returns nil
//     (fail-open). The caller proceeds with the build, and the
//     downstream log classifier handles whatever failure shape
//     eventually surfaces.
//
// Failure semantics mirror Exists / Retag: the only "loud" return is
// the one the caller would otherwise turn into a user-visible rejection,
// so a transient registry blip never causes a false ImageOSNotSupported
// label. The price is that misconfigured private images surface as
// InvalidImage downstream rather than catching them here, which is
// the correct trade -- the gateway must never assert "your image is
// broken" without proof.
func (c *Client) RejectIfPlatformMismatch(ctx context.Context, ref, targetOS, targetArch string) error {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		// Malformed refs fall through to the build, where kaniko's
		// own parse will surface the same problem. Pre-flight isn't
		// the right layer to reject this: doing so couples the
		// rejection to whatever parser version we ship vs. the one
		// kaniko ships, which can diverge.
		return nil
	}

	desc, err := remote.Get(parsed,
		remote.WithAuthFromKeychain(c.keychain),
		remote.WithContext(ctx),
	)
	if err != nil {
		return nil // fail-open on every registry error -- see godoc above
	}

	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			return nil
		}
		for _, m := range manifest.Manifests {
			if m.Platform == nil {
				continue
			}
			if m.Platform.OS == targetOS && m.Platform.Architecture == targetArch {
				return nil
			}
		}
		return fmt.Errorf("%w: image %s does not include %s/%s; available: %s",
			ErrPlatformNotSupported, ref, targetOS, targetArch,
			formatAvailablePlatforms(manifest.Manifests))
	}

	// Single-platform manifest. The OS/arch live in the config blob,
	// not the manifest, so we fetch the config explicitly.
	img, err := desc.Image()
	if err != nil {
		return nil
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil
	}
	if cfg.OS == targetOS && cfg.Architecture == targetArch {
		return nil
	}
	return fmt.Errorf("%w: image %s is %s/%s, expected %s/%s",
		ErrPlatformNotSupported, ref, cfg.OS, cfg.Architecture, targetOS, targetArch)
}

// formatAvailablePlatforms renders the platforms an index actually
// carries, so the rejection message tells the user exactly what they
// fed us. Skips nil-Platform entries (those are attestation /
// signature manifests that OCI lets indexes carry alongside platform
// images and shouldn't count toward "this image supports X").
// Falls back to "<none>" rather than printing an empty string -- the
// latter reads like a bug.
func formatAvailablePlatforms(manifests []v1.Descriptor) string {
	seen := make([]string, 0, len(manifests))
	for _, m := range manifests {
		if m.Platform == nil {
			continue
		}
		seen = append(seen, fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture))
	}
	if len(seen) == 0 {
		return "<none>"
	}
	return strings.Join(seen, ", ")
}
