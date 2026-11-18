package lazyobserve

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestSignalMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cfg   Config
		wantM bool
		wantL bool
		wantT bool
	}{{"defaults", Config{}, true, true, true}, {"metrics only", Config{Metrics: true}, true, false, false}, {"no metrics", Config{Logs: true, Traces: true}, false, true, true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, l, tr := signalMatrix(tt.cfg)
			if m != tt.wantM || l != tt.wantL || tr != tt.wantT {
				t.Fatalf("signalMatrix() = %v,%v,%v", m, l, tr)
			}
		})
	}
}

func TestDeliveryConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := normalizedConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Delivery.ErrorQueueCapacity != 100 || cfg.Delivery.ProfileQueueCapacity != 6 ||
		cfg.Delivery.MaxAttempts != 3 || cfg.Delivery.RetryInitial != 100*time.Millisecond ||
		cfg.Delivery.RetryMax != time.Second {
		t.Fatalf("delivery defaults=%+v", cfg.Delivery)
	}
	_, err = normalizedConfig(Config{Delivery: DeliveryConfig{ErrorQueueCapacity: -1}})
	if err == nil {
		t.Fatal("negative delivery capacity was accepted")
	}
}

func TestInitFailureIsDegraded(t *testing.T) {
	t.Parallel()
	lo := Init(Config{DSN: "not-a-dsn"})
	if lo == nil || lo.Err() == nil || lo.Slog() == nil || lo.Fiber() == nil {
		t.Fatal("Init must return usable degraded instance and error")
	}
}

func TestSignalSwitchesConstructOnlyRequestedProviders(t *testing.T) {
	t.Parallel()
	lo, err := newClient(t.Context(), Config{DSN: "http://127.0.0.1:3100/p/app@token", Metrics: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Shutdown(context.Background())
	if lo.metricSDK == nil || lo.logSDK != nil || lo.traceSDK != nil {
		t.Fatalf("providers: metric=%v log=%v trace=%v", lo.metricSDK != nil, lo.logSDK != nil, lo.traceSDK != nil)
	}
}

func TestFiberUsesHTTPServerDurationBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	client := &Client{
		cfg: Config{ServiceName: "duration-bucket-test"}, meter: provider, tracer: tracenoop.NewTracerProvider(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), propagator: propagation.TraceContext{},
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(client.Fiber())
	app.Get("/orders/:id", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	resp, err := app.Test(httptest.NewRequest("GET", "/orders/42", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "http.server.request.duration" {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok || len(histogram.DataPoints) != 1 {
				t.Fatalf("duration metric data = %#v", metric.Data)
			}
			if got := histogram.DataPoints[0].Bounds; !slices.Equal(got, httpServerRequestDurationBucketsSeconds) {
				t.Fatalf("duration buckets = %v, want %v", got, httpServerRequestDurationBucketsSeconds)
			}
			return
		}
	}
	t.Fatal("http.server.request.duration metric not collected")
}

func TestSlogTraceCorrelation(t *testing.T) {
	t.Parallel()
	capture := &captureHandler{}
	logger := slog.New(&correlatingHandler{service: "orders", tee: capture})
	ctx := sampledContext()
	logger.InfoContext(ctx, "hello")
	attrs := capture.lastAttrs()
	spanContext := trace.SpanContextFromContext(ctx)
	if attrs["service.name"] != "orders" || attrs["trace_id"] != spanContext.TraceID().String() ||
		attrs["span_id"] != spanContext.SpanID().String() {
		t.Fatalf("correlation attrs = %#v", attrs)
	}
}

func TestFiberPanicCapture(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	capture := &captureHandler{}
	client := &Client{
		cfg:    Config{ServiceName: "panic-test"},
		meter:  metricnoop.NewMeterProvider(),
		tracer: tp,
		logger: slog.New(
			&correlatingHandler{service: "panic-test", tee: capture},
		),
		propagator: propagation.TraceContext{},
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(client.Fiber())
	app.Get("/panic", func(*fiber.Ctx) error { panic("boom") })
	resp, err := app.Test(httptest.NewRequest("GET", "/panic", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if capture.lastMessage() != "panic recovered" || capture.lastAttrs()["trace_id"] == "" ||
		capture.lastAttrs()["stack"] == "" {
		t.Fatalf("panic log missing correlation or stack: %#v", capture.lastAttrs())
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Status.Code != codes.Error ||
		!slices.ContainsFunc(spans[0].Events, func(e sdktrace.Event) bool { return e.Name == "exception" }) {
		t.Fatalf("panic span not marked as error: %#v", spans)
	}
}

func sampledContext() context.Context {
	tid, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
	sid, _ := trace.SpanIDFromHex("2222222222222222")
	return trace.ContextWithSpanContext(
		context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}),
	)
}

type capturedRecord struct {
	message string
	attrs   map[string]string
}

type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]string)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedRecord{message: record.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) lastAttrs() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[len(h.records)-1].attrs
}

func (h *captureHandler) lastMessage() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[len(h.records)-1].message
}
