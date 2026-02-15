// Package otel provides OpenTelemetry setup and tracing utilities for the application.
package otel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// Config holds OpenTelemetry configuration.
type Config struct {
	// Enabled controls whether OTLP push (traces + metrics) is active.
	Enabled bool

	// Endpoint is the OTLP HTTP endpoint (e.g. "localhost:4318").
	// If empty, falls back to OTEL_EXPORTER_OTLP_ENDPOINT env var.
	Endpoint string

	// Insecure enables plain HTTP (no TLS) for OTLP export.
	Insecure bool

	// StdOut also prints traces and metrics to stdout (for debugging).
	StdOut bool

	// PrometheusPort, when > 0, starts an HTTP server on this port
	// serving a /metrics endpoint for Prometheus scraping.
	// This works independently of Enabled -- you can use Prometheus
	// without OTLP push, or both together.
	PrometheusPort int
}

// SetupOTelSDK configures the OpenTelemetry SDK with the given service name
// and returns a shutdown function. Call this once at application startup.
//
// The function sets up providers based on what is enabled:
//   - cfg.Enabled: OTLP push for traces and metrics
//   - cfg.PrometheusPort > 0: Prometheus /metrics scrape endpoint
//   - Both can be active simultaneously
//
// The shutdown function should be called with defer to ensure proper cleanup
// of exporters, providers, and the Prometheus HTTP server.
func SetupOTelSDK(
	ctx context.Context,
	serviceName string,
	cfg Config,
) (shutdown func(context.Context) error, err error) {
	var shutdownFuncs []func(context.Context) error

	// Composite shutdown function that calls all registered cleanup functions.
	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// Helper to handle errors during setup.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	// Create resource with service name and version.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("0.1.0"),
		),
	)
	if err != nil {
		handleErr(err)
		return
	}

	// Setup trace provider (only when OTLP push is enabled).
	if cfg.Enabled {
		tracerProvider, tErr := newTraceProvider(ctx, res, cfg)
		if tErr != nil {
			handleErr(tErr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
		otel.SetTracerProvider(tracerProvider)
	}

	// Setup meter provider (needed for OTLP push, Prometheus, or both).
	needsMetrics := cfg.Enabled || cfg.PrometheusPort > 0
	if needsMetrics {
		meterProvider, mErr := newMeterProvider(ctx, res, cfg)
		if mErr != nil {
			handleErr(mErr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
		otel.SetMeterProvider(meterProvider)
	}

	// Start Prometheus HTTP server if configured.
	if cfg.PrometheusPort > 0 {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		srv := &http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.PrometheusPort),
			Handler: mux,
		}
		go func() {
			if srvErr := srv.ListenAndServe(); srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
				// Log is not available here; the error will surface
				// if something is already listening on the port.
				fmt.Printf("prometheus /metrics server error: %v\n", srvErr)
			}
		}()
		shutdownFuncs = append(shutdownFuncs, srv.Shutdown)
	}

	return
}

// newTraceProvider creates a TracerProvider with OTLP HTTP exporter.
func newTraceProvider(ctx context.Context, res *resource.Resource, cfg Config) (*trace.TracerProvider, error) {
	var exporters []trace.SpanExporter

	// OTLP HTTP trace exporter.
	opts := []otlptracehttp.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	traceExporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	exporters = append(exporters, traceExporter)

	// Optional stdout trace exporter for debugging.
	if cfg.StdOut {
		stdoutExporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, err
		}
		exporters = append(exporters, stdoutExporter)
	}

	// Build tracer provider with all exporters.
	providerOpts := []trace.TracerProviderOption{
		trace.WithResource(res),
	}
	for _, exp := range exporters {
		providerOpts = append(providerOpts, trace.WithBatcher(exp,
			trace.WithBatchTimeout(time.Second)))
	}

	return trace.NewTracerProvider(providerOpts...), nil
}

// newMeterProvider creates a MeterProvider with the configured readers.
//
// Readers are added based on configuration:
//   - OTLP metric reader: when cfg.Enabled is true
//   - Stdout metric reader: when cfg.StdOut is true
//   - Prometheus reader: when cfg.PrometheusPort > 0
func newMeterProvider(ctx context.Context, res *resource.Resource, cfg Config) (*metric.MeterProvider, error) {
	var readers []metric.Reader

	// OTLP HTTP metric reader (only when OTLP push is enabled).
	if cfg.Enabled {
		opts := []otlpmetrichttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}

		metricExporter, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, err
		}
		readers = append(readers, metric.NewPeriodicReader(metricExporter,
			metric.WithInterval(10*time.Second)))
	}

	// Optional stdout metric reader for debugging.
	if cfg.StdOut {
		stdoutExporter, err := stdoutmetric.New()
		if err != nil {
			return nil, err
		}
		readers = append(readers, metric.NewPeriodicReader(stdoutExporter,
			metric.WithInterval(10*time.Second)))
	}

	// Prometheus metric reader (exposes metrics for /metrics scrape endpoint).
	if cfg.PrometheusPort > 0 {
		promExp, err := promexporter.New()
		if err != nil {
			return nil, fmt.Errorf("creating prometheus exporter: %w", err)
		}
		readers = append(readers, promExp)
	}

	// Build meter provider with all readers.
	providerOpts := []metric.Option{
		metric.WithResource(res),
	}
	for _, reader := range readers {
		providerOpts = append(providerOpts, metric.WithReader(reader))
	}

	return metric.NewMeterProvider(providerOpts...), nil
}
