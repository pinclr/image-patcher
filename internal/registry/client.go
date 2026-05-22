/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package registry is the controller-side OCI registry client used for
// content-addressed build dedup. It only does what the dedup short-circuit
// needs: HEAD a manifest to test for existence, and copy a manifest from
// one tag to another within the same repo. Nothing here is a general-
// purpose registry library -- if a third operation lands, reconsider
// pulling in a higher-level helper instead of growing this file.
package registry

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Client wraps the go-containerregistry remote calls with a single
// keychain loaded from a docker config.json. The keychain is captured
// at construction so each call site doesn't redo auth resolution.
type Client struct {
	keychain authn.Keychain
}

// NewFromDockerConfig builds a Client whose auth comes from the
// dockerconfigjson file at configPath (the same shape Kaniko mounts).
// The file must exist and parse; missing/broken files are caller
// errors -- the caller decides whether to treat that as fatal or just
// disable dedup.
//
// Auth resolution uses DefaultKeychain pointed at configPath via the
// DOCKER_CONFIG env contract that go-containerregistry's authn
// package honours. We point it at the *directory* containing the
// file and rely on the standard "config.json" filename, so the
// passed-in path MUST end in /config.json.
func NewFromDockerConfig(configPath string) (*Client, error) {
	if configPath == "" {
		return nil, fmt.Errorf("registry: configPath is empty")
	}
	if _, err := os.Stat(configPath); err != nil {
		return nil, fmt.Errorf("registry: stat %s: %w", configPath, err)
	}
	dir := dockerConfigDir(configPath)
	// authn.DefaultKeychain reads DOCKER_CONFIG at *call time*, not at
	// import time, so setting it once on the process is enough -- any
	// goroutine resolving auth later sees it. We deliberately set the
	// global env rather than carry a keychain around configured per
	// instance, because the upstream Keychain interface has no hook
	// for "use this directory" without going through DOCKER_CONFIG.
	if err := os.Setenv("DOCKER_CONFIG", dir); err != nil {
		return nil, fmt.Errorf("registry: set DOCKER_CONFIG: %w", err)
	}
	return &Client{keychain: authn.DefaultKeychain}, nil
}

func dockerConfigDir(configPath string) string {
	// Strip trailing "/config.json" if present; otherwise treat the
	// path as already being a directory. We do not import path/filepath
	// here to keep the dependency footprint trivial.
	const suffix = "/config.json"
	if len(configPath) > len(suffix) && configPath[len(configPath)-len(suffix):] == suffix {
		return configPath[:len(configPath)-len(suffix)]
	}
	return configPath
}

// Exists reports whether the manifest at ref is present in the
// registry. A clean 404 returns (false, nil) so callers can branch on
// hit/miss without parsing error messages. Any other error -- network,
// auth, unexpected status -- bubbles up as a non-nil err with exists
// = false, and callers in the dedup short-circuit are expected to
// fail-open (skip dedup and run the build).
func (c *Client) Exists(ctx context.Context, ref string) (bool, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return false, fmt.Errorf("registry: parse %q: %w", ref, err)
	}
	if _, err := remote.Head(parsed,
		remote.WithAuthFromKeychain(c.keychain),
		remote.WithContext(ctx),
	); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("registry: HEAD %s: %w", ref, err)
	}
	return true, nil
}

// Retag copies the manifest at src to dst. Both refs must point at the
// same repository -- the registry can only resolve the manifest's blob
// references within a single repo's blob store, so cross-repo retag
// requires explicit blob mount which this client does not do (and which
// dedup does not need by construction; see same-repo rationale in the
// controller's computeDedupRef comment).
//
// Implementation: GET the descriptor at src (one round trip), then
// PUT it at dst (a second round trip). No blobs move; the registry
// only writes a new tag→digest binding pointing at the existing
// manifest. This is exactly what the docker CLI calls "tag and push"
// when source and target share a repo, except without local pull.
func (c *Client) Retag(ctx context.Context, src, dst string) error {
	srcRef, err := name.ParseReference(src)
	if err != nil {
		return fmt.Errorf("registry: parse src %q: %w", src, err)
	}
	dstRef, err := name.ParseReference(dst)
	if err != nil {
		return fmt.Errorf("registry: parse dst %q: %w", dst, err)
	}
	if srcRef.Context().String() != dstRef.Context().String() {
		// Defensive: caller should already enforce same repo. Surface
		// the mismatch loudly rather than silently doing the wrong
		// thing (cross-repo PUT would 400 or worse with
		// MANIFEST_BLOB_UNKNOWN at retrieval time).
		return fmt.Errorf("registry: retag refuses cross-repo: src=%s dst=%s",
			srcRef.Context(), dstRef.Context())
	}
	desc, err := remote.Get(srcRef,
		remote.WithAuthFromKeychain(c.keychain),
		remote.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("registry: GET src %s: %w", src, err)
	}
	// remote.Put accepts a Taggable -- a Manifest descriptor satisfies
	// that interface (it carries the raw manifest bytes and media type),
	// so we don't need to materialize an Image or ImageIndex. Works
	// uniformly for OCI / Docker schema and for image vs. index.
	return retagPut(ctx, dstRef, desc, c.keychain)
}

func retagPut(ctx context.Context, dstRef name.Reference, desc *remote.Descriptor, keychain authn.Keychain) error {
	if err := remote.Put(dstRef, desc,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
	); err != nil {
		return fmt.Errorf("registry: PUT dst %s: %w", dstRef, err)
	}
	return nil
}

// isNotFound matches the registry "manifest unknown" response. The
// transport package returns *transport.Error with Errors[].Code set
// to "MANIFEST_UNKNOWN" for clean 404s; some registries also return
// "NAME_UNKNOWN" when the *repo* doesn't exist yet, which for our
// purposes is also a miss (first time we're touching this dedup tag).
func isNotFound(err error) bool {
	var te *transport.Error
	if !errors.As(err, &te) {
		return false
	}
	if te.StatusCode == 404 {
		return true
	}
	for _, e := range te.Errors {
		switch e.Code {
		case "MANIFEST_UNKNOWN", "NAME_UNKNOWN":
			return true
		}
	}
	return false
}
