package lazyobserve

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// BenchmarkFiberMiddleware measures a complete in-process Fiber request both
// without instrumentation and with lazy-observe's explicit middleware. The
// performance gate pairs the two measurements from each benchmark invocation
// so Fiber's request-test machinery and route dispatch are not charged to the
// SDK overhead budget.
func BenchmarkFiberMiddleware(b *testing.B) {
	exporter := new(benchmarkSpanExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exporter),
	)
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	b.Cleanup(func() {
		var metrics metricdata.ResourceMetrics
		if err := metricReader.Collect(context.Background(), &metrics); err != nil {
			b.Errorf("collect benchmark metrics: %v", err)
		} else if !hasBenchmarkMetric(metrics, "http.server.request.duration") {
			b.Error("instrumented benchmark did not record request duration metrics")
		}
		if err := meterProvider.Shutdown(context.Background()); err != nil {
			b.Errorf("shut down benchmark meter provider: %v", err)
		}
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			b.Errorf("shut down benchmark tracer provider: %v", err)
		}
		if exporter.exported.Load() == 0 {
			b.Error("instrumented benchmark did not export any spans")
		}
	})
	client := &Client{
		cfg:        Config{ServiceName: "middleware-benchmark"},
		meter:      meterProvider,
		tracer:     tracerProvider,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		propagator: propagation.TraceContext{},
	}
	benchmarkFiberRequest(b, "baseline", nil)
	benchmarkFiberRequest(b, "instrumented", client.Fiber())
}

func hasBenchmarkMetric(resourceMetrics metricdata.ResourceMetrics, name string) bool {
	for _, scope := range resourceMetrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == name {
				return true
			}
		}
	}
	return false
}

// benchmarkSpanExporter keeps the benchmark on the real recording/export path
// without network I/O or an ever-growing in-memory span buffer. WithSyncer
// invokes it inline and therefore does not leave a batch-processor goroutine.
type benchmarkSpanExporter struct {
	exported atomic.Uint64
}

func (e *benchmarkSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.exported.Add(uint64(len(spans)))
	return nil
}

func (*benchmarkSpanExporter) Shutdown(context.Context) error { return nil }

func benchmarkFiberRequest(b *testing.B, name string, middleware fiber.Handler) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		app := fiber.New()
		if middleware != nil {
			app.Use(middleware)
		}
		app.Get("/orders/:id", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			response, err := app.Test(httptest.NewRequest("GET", "/orders/42", nil), fiber.TestConfig{Timeout: 0})
			if err != nil {
				b.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
