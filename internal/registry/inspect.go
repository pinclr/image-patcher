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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
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
// dockerConfig optionally carries the bytes of a docker config.json
// (the CR's pullSecret) so a private BaseImage can be inspected with
// real credentials. When nil/empty, or when the config can't be
// parsed, the inspect falls back to the anonymous keychain: this is
// the v1 "assume public registries" pre-flight. Private registries
// that then 401 fall under bucket 3 and let kaniko handle the build
// with its own mounted credentials -- unchanged from the no-pre-flight
// world. Supplying the pullSecret here closes that gap so the platform
// check actually runs for private bases instead of always failing open.
//
// Failure semantics mirror Exists / Retag: the only "loud" return is
// the one the caller would otherwise turn into a user-visible rejection,
// so a transient registry blip never causes a false ImageOSNotSupported
// label. The price is that misconfigured private images surface as
// InvalidImage downstream rather than catching them here, which is
// the correct trade -- the gateway must never assert "your image is
// broken" without proof.
func RejectIfPlatformMismatch(ctx context.Context, ref, targetOS, targetArch string, dockerConfig []byte) error {
	// Default to anonymous; upgrade to the pullSecret keychain when the
	// caller supplied a parseable docker config. A parse failure logs
	// and stays anonymous rather than rejecting -- same fail-open stance
	// as every other bucket-3 path here.
	authOpt := remote.WithAuth(authn.Anonymous)
	if len(dockerConfig) > 0 {
		if kc, kerr := keychainFromDockerConfig(dockerConfig); kerr != nil {
			log.Printf("preflight: ref=%q docker_config_parse_error=%v -> anonymous", ref, kerr)
		} else {
			authOpt = remote.WithAuthFromKeychain(kc)
		}
	}

	// Diagnostic logging is written to stderr via the stdlib logger so
	// every fail-open path is observable in kubectl logs. Without this
	// we'd silently return nil and have no way to tell whether
	// pre-flight ran but couldn't reach the registry, or whether it
	// ran and saw a matching platform. Cheap, single-line per outcome.
	parsed, err := name.ParseReference(ref)
	if err != nil {
		log.Printf("preflight: ref=%q parse_error=%v -> fail-open", ref, err)
		return nil
	}

	desc, err := remote.Get(parsed,
		authOpt,
		remote.WithContext(ctx),
	)
	if err != nil {
		log.Printf("preflight: ref=%q registry_error=%v -> fail-open", ref, err)
		return nil
	}

	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			log.Printf("preflight: ref=%q index_decode_error=%v -> fail-open", ref, err)
			return nil
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			log.Printf("preflight: ref=%q index_manifest_error=%v -> fail-open", ref, err)
			return nil
		}
		for _, m := range manifest.Manifests {
			if m.Platform == nil {
				continue
			}
			if m.Platform.OS == targetOS && m.Platform.Architecture == targetArch {
				log.Printf("preflight: ref=%q index_has_target=%s/%s -> accept", ref, targetOS, targetArch)
				return nil
			}
		}
		available := formatAvailablePlatforms(manifest.Manifests)
		log.Printf("preflight: ref=%q index_missing_target=%s/%s available=%s -> reject", ref, targetOS, targetArch, available)
		return fmt.Errorf("%w: image %s does not include %s/%s; available: %s",
			ErrPlatformNotSupported, ref, targetOS, targetArch, available)
	}

	// Single-platform manifest. The OS/arch live in the config blob,
	// not the manifest, so we fetch the config explicitly.
	img, err := desc.Image()
	if err != nil {
		log.Printf("preflight: ref=%q image_decode_error=%v -> fail-open", ref, err)
		return nil
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		log.Printf("preflight: ref=%q config_fetch_error=%v -> fail-open", ref, err)
		return nil
	}
	if cfg.OS == targetOS && cfg.Architecture == targetArch {
		log.Printf("preflight: ref=%q platform=%s/%s -> accept", ref, cfg.OS, cfg.Architecture)
		return nil
	}
	log.Printf("preflight: ref=%q platform=%s/%s expected=%s/%s -> reject", ref, cfg.OS, cfg.Architecture, targetOS, targetArch)
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

// dockerConfigKeychain resolves registry auth from an in-memory docker
// config.json "auths" map. It exists so the pre-flight inspect can read a
// private base image using the CR's pullSecret without going through the
// DOCKER_CONFIG-env / file dance the dedup Client uses -- the bytes are
// already in hand from the Secret. Unknown registries resolve to Anonymous
// so a config that only carries the base registry doesn't break inspects of
// other refs.
type dockerConfigKeychain struct {
	auths map[string]authn.AuthConfig
}

func (k *dockerConfigKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	// Docker config keys are usually the bare registry host (private
	// registries) but can be a full URL (Docker Hub's
	// "https://index.docker.io/v1/"). Try the most specific form first,
	// then the host, then give up to Anonymous.
	for _, key := range []string{res.String(), res.RegistryStr()} {
		if cfg, ok := k.auths[key]; ok {
			return authn.FromConfig(cfg), nil
		}
	}
	return authn.Anonymous, nil
}

// keychainFromDockerConfig parses docker config.json bytes into a keychain.
// authn.AuthConfig's json tags line up with the docker config auth entry
// shape (auth / username / password / identitytoken / registrytoken), so the
// "auths" map decodes directly.
func keychainFromDockerConfig(config []byte) (authn.Keychain, error) {
	var doc struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("registry: parse docker config: %w", err)
	}
	return &dockerConfigKeychain{auths: doc.Auths}, nil
}
