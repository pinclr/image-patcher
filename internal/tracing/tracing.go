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

// Package tracing wires up OpenTelemetry distributed tracing for the
// controller. It is the bootstrap only: it configures (or, when disabled,
// leaves untouched) the global TracerProvider and propagator. Span
// instrumentation of the reconcile path lives with the controller code.
//
// Tracing is opt-in. When TRACING_ENABLED is not "true" the global provider
// is left as OpenTelemetry's built-in no-op, so any future otel.Tracer(...)
// calls are zero-cost and emit nothing. Setup is also fail-open: a
// misconfigured or unreachable exporter is logged and the controller runs
// without traces rather than refusing to start (tracing is a non-critical
// signal, mirroring how the dedup short-circuit degrades).
//
// Exporter wiring (endpoint, TLS/insecure, headers, timeouts) is read by the
// OTLP/gRPC exporter from the standard OTEL_EXPORTER_OTLP_* environment
// variables, so it interoperates with any collector without bespoke flags.
package tracing

import (
	"context"
	"os"
	"strconv"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// defaultServiceName is the resource service.name reported on every span when
// OTEL_SERVICE_NAME is not set.
const defaultServiceName = "image-patch-operator"

// Config controls tracing setup. Exporter connection details (endpoint, TLS,
// headers) are intentionally absent: they come from the standard
// OTEL_EXPORTER_OTLP_* environment variables read by the exporter itself.
type Config struct {
	// Enabled turns tracing on. When false, Setup is a no-op and the global
	// provider stays OpenTelemetry's no-op implementation.
	Enabled bool
	// SamplerRatio is the head-sampling probability for root spans, in [0,1].
	// Wrapped in a ParentBased sampler so a sampled parent is always honored.
	SamplerRatio float64
	// ServiceName / ServiceVersion populate the OTel resource.
	ServiceName    string
	ServiceVersion string
}

// ConfigFromEnv reads tracing configuration from environment variables:
//
//	TRACING_ENABLED        "true" to enable (default off)
//	TRACING_SAMPLER_RATIO  float in [0,1] (default 1.0)
//	OTEL_SERVICE_NAME      resource service.name (default image-patch-operator)
//	OTEL_SERVICE_VERSION   resource service.version (optional)
//
// Exporter endpoint/TLS come from OTEL_EXPORTER_OTLP_* (read by the exporter).
func ConfigFromEnv() Config {
	cfg := Config{
		Enabled:        os.Getenv("TRACING_ENABLED") == "true",
		SamplerRatio:   1.0,
		ServiceName:    getenvDefault("OTEL_SERVICE_NAME", defaultServiceName),
		ServiceVersion: os.Getenv("OTEL_SERVICE_VERSION"),
	}
	if v := os.Getenv("TRACING_SAMPLER_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.SamplerRatio = f
		}
	}
	return cfg
}

// Setup configures the global OpenTelemetry TracerProvider and propagator
// from cfg and returns a shutdown function that flushes pending spans. The
// returned shutdown is always non-nil and safe to call (a no-op when tracing
// is disabled or setup failed), so callers can defer it unconditionally.
func Setup(ctx context.Context, cfg Config, log logr.Logger) func(context.Context) error {
	noop := func(context.Context) error { return nil }

	if !cfg.Enabled {
		log.Info("tracing disabled (set TRACING_ENABLED=true to enable)")
		return noop
	}

	// The OTLP/gRPC exporter reads OTEL_EXPORTER_OTLP_* for endpoint, TLS and
	// headers. Creation is lazy (no connection yet), so failure here is rare
	// and non-fatal: log and run without tracing.
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		log.Error(err, "tracing: OTLP exporter setup failed; continuing without tracing")
		return noop
	}

	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	// Merge our attributes onto the SDK default resource (host, telemetry.sdk,
	// process, OTEL_RESOURCE_ATTRIBUTES). NewSchemaless carries no schema URL,
	// so the merge cannot conflict on schema; fall back to attrs-only if it
	// somehow does.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		res = resource.NewSchemaless(attrs...)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplerRatio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// Route the SDK's internal errors (e.g. export failures) to our logger at
	// a low verbosity instead of the default stderr writer.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(e error) {
		log.V(1).Info("opentelemetry error", "err", e.Error())
	}))

	log.Info("tracing enabled", "serviceName", cfg.ServiceName, "samplerRatio", cfg.SamplerRatio)
	return tp.Shutdown
}

// Tracer returns the named tracer from the global provider. Use it as the
// single entry point for instrumenting code; when tracing is disabled it
// returns a no-op tracer.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
