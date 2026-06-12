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

package tracing

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
)

func TestConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("TRACING_ENABLED", "")
		t.Setenv("TRACING_SAMPLER_RATIO", "")
		t.Setenv("OTEL_SERVICE_NAME", "")
		t.Setenv("OTEL_SERVICE_VERSION", "")

		cfg := ConfigFromEnv()
		if cfg.Enabled {
			t.Errorf("Enabled = true, want false by default")
		}
		if cfg.SamplerRatio != 1.0 {
			t.Errorf("SamplerRatio = %v, want 1.0", cfg.SamplerRatio)
		}
		if cfg.ServiceName != defaultServiceName {
			t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, defaultServiceName)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("TRACING_ENABLED", "true")
		t.Setenv("TRACING_SAMPLER_RATIO", "0.25")
		t.Setenv("OTEL_SERVICE_NAME", "custom")
		t.Setenv("OTEL_SERVICE_VERSION", "v9.9.9")

		cfg := ConfigFromEnv()
		if !cfg.Enabled {
			t.Errorf("Enabled = false, want true")
		}
		if cfg.SamplerRatio != 0.25 {
			t.Errorf("SamplerRatio = %v, want 0.25", cfg.SamplerRatio)
		}
		if cfg.ServiceName != "custom" {
			t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, "custom")
		}
		if cfg.ServiceVersion != "v9.9.9" {
			t.Errorf("ServiceVersion = %q, want %q", cfg.ServiceVersion, "v9.9.9")
		}
	})

	t.Run("bad ratio falls back to default", func(t *testing.T) {
		t.Setenv("TRACING_SAMPLER_RATIO", "not-a-number")
		if got := ConfigFromEnv().SamplerRatio; got != 1.0 {
			t.Errorf("SamplerRatio = %v, want 1.0 fallback", got)
		}
	})
}

func TestSetupDisabledIsNoopAndSafe(t *testing.T) {
	shutdown := Setup(context.Background(), Config{Enabled: false}, logr.Discard())
	if shutdown == nil {
		t.Fatal("Setup returned nil shutdown; must be safe to defer")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}

	// With tracing disabled the global tracer is OTel's no-op: spans created
	// from it must not be recording.
	_, span := Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if span.IsRecording() {
		t.Errorf("span IsRecording() = true with tracing disabled, want false (no-op)")
	}
}
