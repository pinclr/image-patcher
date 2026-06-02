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
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// pushPlatformImage uploads a single-platform random image with the
// given OS/arch baked into its config. We mutate the config explicitly
// because random.Image otherwise inherits the host's, which makes the
// "linux/arm64 rejected" tests pass for the wrong reason when run on
// an arm Mac.
func pushPlatformImage(t *testing.T, ref, osVal, arch string) {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cf.OS = osVal
	cf.Architecture = arch
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatalf("remote.Write %s: %v", ref, err)
	}
}

// pushIndex uploads a multi-arch index whose child manifests are
// real per-platform random images. Each child has its config OS/arch
// set to match the index entry's platform descriptor (otherwise a
// strict registry would reject the index for inconsistency).
func pushIndex(t *testing.T, ref string, platforms [][2]string) {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}

	adds := make([]mutate.IndexAddendum, 0, len(platforms))
	for _, p := range platforms {
		img, err := random.Image(64, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatalf("ConfigFile: %v", err)
		}
		cf.OS, cf.Architecture = p[0], p[1]
		img, err = mutate.ConfigFile(img, cf)
		if err != nil {
			t.Fatalf("mutate.ConfigFile: %v", err)
		}
		adds = append(adds, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: p[0], Architecture: p[1]},
			},
		})
	}

	// random.Index doesn't expose an empty starter, so synthesise
	// one and let AppendManifests extend it.
	base, err := random.Index(0, 0, 0)
	if err != nil {
		t.Fatalf("random.Index: %v", err)
	}
	idx := mutate.AppendManifests(mutate.IndexMediaType(base, types.OCIImageIndex), adds...)
	if err := remote.WriteIndex(r, idx); err != nil {
		t.Fatalf("remote.WriteIndex %s: %v", ref, err)
	}
}

func TestRejectIfPlatformMismatch(t *testing.T) {
	// Reuse the dedup test scaffold purely to spin up an httptest
	// registry on a real port. The Client returned by newTestClient
	// isn't used -- RejectIfPlatformMismatch is a package-level
	// function with its own (anonymous) keychain.
	_, host := newTestClient(t)
	ctx := context.Background()

	t.Run("single-platform linux/amd64 accepted", func(t *testing.T) {
		ref := host + "/repo/match:latest"
		pushPlatformImage(t, ref, "linux", "amd64")
		if err := RejectIfPlatformMismatch(ctx, ref, "linux", "amd64", nil); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("single-platform linux/arm64 rejected (the real-world luna mirror case)", func(t *testing.T) {
		ref := host + "/repo/wrong-arch:latest"
		pushPlatformImage(t, ref, "linux", "arm64")
		err := RejectIfPlatformMismatch(ctx, ref, "linux", "amd64", nil)
		if !errors.Is(err, ErrPlatformNotSupported) {
			t.Fatalf("want ErrPlatformNotSupported, got %v", err)
		}
		// Detail should mention both actual + expected so the rendered
		// status message is self-explanatory.
		if !strings.Contains(err.Error(), "linux/arm64") {
			t.Errorf("error should name actual platform, got: %v", err)
		}
		if !strings.Contains(err.Error(), "linux/amd64") {
			t.Errorf("error should name expected platform, got: %v", err)
		}
	})

	t.Run("single-platform windows/amd64 rejected", func(t *testing.T) {
		ref := host + "/repo/windows:latest"
		pushPlatformImage(t, ref, "windows", "amd64")
		err := RejectIfPlatformMismatch(ctx, ref, "linux", "amd64", nil)
		if !errors.Is(err, ErrPlatformNotSupported) {
			t.Fatalf("want ErrPlatformNotSupported, got %v", err)
		}
		if !strings.Contains(err.Error(), "windows/amd64") {
			t.Errorf("error should name actual platform, got: %v", err)
		}
	})

	t.Run("multi-arch index containing linux/amd64 accepted", func(t *testing.T) {
		ref := host + "/repo/multi-ok:latest"
		pushIndex(t, ref, [][2]string{
			{"linux", "amd64"},
			{"linux", "arm64"},
		})
		if err := RejectIfPlatformMismatch(ctx, ref, "linux", "amd64", nil); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("multi-arch index missing linux/amd64 rejected with available list", func(t *testing.T) {
		ref := host + "/repo/multi-no-amd64:latest"
		pushIndex(t, ref, [][2]string{
			{"linux", "arm64"},
			{"linux", "arm"},
		})
		err := RejectIfPlatformMismatch(ctx, ref, "linux", "amd64", nil)
		if !errors.Is(err, ErrPlatformNotSupported) {
			t.Fatalf("want ErrPlatformNotSupported, got %v", err)
		}
		if !strings.Contains(err.Error(), "linux/arm64") {
			t.Errorf("rejection should enumerate available platforms, got: %v", err)
		}
	})

	t.Run("missing image falls through to nil (fail-open)", func(t *testing.T) {
		// Image never pushed -> registry returns 404. The user might
		// have typoed the tag; image-patcher's downstream classifier
		// will surface InvalidImage on the build path. We must NOT
		// pre-emptively label ImageOSNotSupported here.
		err := RejectIfPlatformMismatch(ctx, host+"/repo/does-not-exist:latest", "linux", "amd64", nil)
		if err != nil {
			t.Errorf("missing image should fail-open with nil, got %v", err)
		}
	})

	t.Run("unreachable registry falls through to nil (fail-open)", func(t *testing.T) {
		// 127.0.0.1:1 is an unprivileged port that nothing listens on;
		// the HTTP GET errors out at the TCP layer. Fail-open keeps a
		// flaky registry from blocking otherwise-valid builds -- the
		// downstream Job will hit the same registry and surface its
		// own error.
		err := RejectIfPlatformMismatch(ctx, "127.0.0.1:1/nonexistent:latest", "linux", "amd64", nil)
		if err != nil {
			t.Errorf("unreachable registry should fail-open with nil, got %v", err)
		}
	})

	t.Run("malformed image reference falls through to nil", func(t *testing.T) {
		// Pre-flight isn't the right layer to reject malformed refs:
		// kaniko's own parser would catch them on the build path and
		// the error message there is more authoritative. Same fail-open.
		err := RejectIfPlatformMismatch(ctx, ":: not a valid ref ::", "linux", "amd64", nil)
		if err != nil {
			t.Errorf("malformed ref should fail-open with nil, got %v", err)
		}
	})
}
