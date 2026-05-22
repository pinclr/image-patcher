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
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// newTestClient spins up an in-memory OCI registry on httptest and
// returns (client, registry host). Auth is anonymous; we still drive
// the docker-config path to exercise NewFromDockerConfig end-to-end.
func newTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	dir := t.TempDir()
	cfg := map[string]any{"auths": map[string]any{}}
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := NewFromDockerConfig(cfgPath)
	if err != nil {
		t.Fatalf("NewFromDockerConfig: %v", err)
	}
	return c, u.Host
}

// pushImage uploads a small random image at ref so subsequent Exists /
// Retag have something real to point at. Anonymous because the test
// registry doesn't enforce auth.
func pushImage(t *testing.T, ref string) {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatalf("remote.Write %s: %v", ref, err)
	}
}

func TestExists_HitAndMiss(t *testing.T) {
	c, host := newTestClient(t)
	ctx := context.Background()

	hitRef := host + "/repo/app:dedup-abc"
	pushImage(t, hitRef)

	ok, err := c.Exists(ctx, hitRef)
	if err != nil || !ok {
		t.Fatalf("Exists(hit) = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = c.Exists(ctx, host+"/repo/app:does-not-exist")
	if err != nil {
		t.Fatalf("Exists(miss) returned err: %v (want nil)", err)
	}
	if ok {
		t.Fatalf("Exists(miss) = true, want false")
	}
}

func TestRetag_SameRepo(t *testing.T) {
	c, host := newTestClient(t)
	ctx := context.Background()

	src := host + "/repo/app:dedup-abc"
	dst := host + "/repo/app:latest"
	pushImage(t, src)

	if err := c.Retag(ctx, src, dst); err != nil {
		t.Fatalf("Retag: %v", err)
	}

	ok, err := c.Exists(ctx, dst)
	if err != nil || !ok {
		t.Fatalf("Exists(dst) after Retag = (%v, %v), want (true, nil)", ok, err)
	}

	// Same digest under both tags confirms manifest reuse (no rebuild).
	srcDesc, err := remote.Get(mustRef(t, src))
	if err != nil {
		t.Fatalf("remote.Get src: %v", err)
	}
	dstDesc, err := remote.Get(mustRef(t, dst))
	if err != nil {
		t.Fatalf("remote.Get dst: %v", err)
	}
	if srcDesc.Digest != dstDesc.Digest {
		t.Fatalf("digest mismatch after Retag: src=%s dst=%s", srcDesc.Digest, dstDesc.Digest)
	}
}

func TestRetag_RejectsCrossRepo(t *testing.T) {
	c, host := newTestClient(t)
	ctx := context.Background()

	src := host + "/repo/app:dedup-abc"
	dst := host + "/repo/other:latest"
	pushImage(t, src)

	err := c.Retag(ctx, src, dst)
	if err == nil || !strings.Contains(err.Error(), "cross-repo") {
		t.Fatalf("Retag(cross-repo) = %v, want error containing 'cross-repo'", err)
	}
}

func TestNewFromDockerConfig_MissingFile(t *testing.T) {
	_, err := NewFromDockerConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatalf("NewFromDockerConfig with missing file returned nil err")
	}
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return r
}
