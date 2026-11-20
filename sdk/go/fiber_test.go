package lazyobserve

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fiberAliasingRounds is sized so that reusing Fiber's pooled request context
// and fasthttp's request buffers happens many times over, which makes a borrowed
// (zero-copy) attribute value show up as another request's value.
const fiberAliasingRounds = 300

var fiberAliasingMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
}

type fiberAliasingRequest struct {
	method, path, host, scheme, traceState string
}

func fiberAliasingProbe(round int) fiberAliasingRequest {
	return fiberAliasingRequest{
		method:     fiberAliasingMethods[round%len(fiberAliasingMethods)],
		path:       fmt.Sprintf("/probe/%06d", round),
		host:       fmt.Sprintf("h%06d.example", round),
		scheme:     fmt.Sprintf("s%06d", round),
		traceState: fmt.Sprintf("probe=r%06d", round),
	}
}

// The Fiber middleware must own every value it keeps past the request: span
// attributes are exported in batches, metric attributes become aggregation map
// keys, and the default (non-Immutable) Fiber config lends request strings that
// the next request overwrites in place.
func TestFiberTelemetryDoesNotBorrowRequestBuffers(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = traceProvider.Shutdown(context.Background()) })
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
	client := &Client{
		cfg:        Config{ServiceName: "fiber-aliasing"},
		meter:      meterProvider,
		tracer:     traceProvider,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		propagator: propagation.TraceContext{},
	}
	app := fiber.New()
	if app.Config().Immutable {
		t.Fatal("probe app must keep Fiber's default zero-copy strings")
	}
	app.Use(client.Fiber())
	app.All("/probe/:id", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	wantCounts := map[string]int64{}
	for round := range fiberAliasingRounds {
		probe := fiberAliasingProbe(round)
		req := httptest.NewRequest(probe.method, probe.path, nil)
		req.Host = probe.host
		req.Header.Set(fiber.HeaderXForwardedProto, probe.scheme)
		req.Header.Set("traceparent", fmt.Sprintf("00-%032x-%016x-01", round+1, round+1))
		req.Header.Set("tracestate", probe.traceState)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusNoContent {
			t.Fatalf("round %d: status=%d", round, resp.StatusCode)
		}
		wantCounts[probe.method]++
	}

	spans := exporter.GetSpans()
	if len(spans) != fiberAliasingRounds {
		t.Fatalf("spans=%d want=%d", len(spans), fiberAliasingRounds)
	}
	mismatched := 0
	for _, span := range spans {
		traceID := span.SpanContext.TraceID()
		round := int(traceID[12])<<24 | int(traceID[13])<<16 | int(traceID[14])<<8 | int(traceID[15]) - 1
		probe := fiberAliasingProbe(round)
		attrs := spanStringAttrs(span.Attributes)
		got := fiberAliasingRequest{
			method:     attrs["http.request.method"],
			path:       attrs["url.path"],
			host:       attrs["server.address"],
			scheme:     attrs["url.scheme"],
			traceState: span.SpanContext.TraceState().String(),
		}
		if got != probe || span.Name != probe.method+" /probe/:id" {
			if mismatched < 5 {
				t.Errorf("round %d: span name=%q attrs=%+v want=%+v", round, span.Name, got, probe)
			}
			mismatched++
		}
	}
	if mismatched != 0 {
		t.Errorf("%d/%d spans carry another request's values", mismatched, fiberAliasingRounds)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	gotCounts := map[string]int64{}
	gotActive := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					method, _ := point.Attributes.Value("http.request.method")
					route, _ := point.Attributes.Value("http.route")
					if route.AsString() != "/probe/:id" {
						t.Errorf("duration route=%q", route.AsString())
					}
					gotCounts[method.AsString()] += int64(point.Count)
				}
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					method, _ := point.Attributes.Value("http.request.method")
					gotActive[method.AsString()] += point.Value
				}
			}
		}
	}
	if !maps.Equal(gotCounts, wantCounts) {
		t.Errorf("duration counts by method=%v want=%v", gotCounts, wantCounts)
	}
	wantActive := map[string]int64{}
	for method := range wantCounts {
		wantActive[method] = 0
	}
	if !maps.Equal(gotActive, wantActive) {
		t.Errorf("active requests by method=%v want=%v", gotActive, wantActive)
	}
}

func spanStringAttrs(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// Fiber v3 moved the scheme to Ctx.Scheme; Ctx.Protocol is now the HTTP version
// ("HTTP/1.1"). Comparing Protocol against "https" compiles but is never true,
// so a TLS request would be reported as http.
func TestFiberReportsTLSRequestsAsHTTPS(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = traceProvider.Shutdown(context.Background()) })
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
	client := &Client{
		cfg:        Config{ServiceName: "fiber-scheme"},
		meter:      meterProvider,
		tracer:     traceProvider,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		propagator: propagation.TraceContext{},
	}
	app := fiber.New()
	app.Use(client.Fiber())
	app.Get("/probe", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	for _, tt := range []struct{ target, forwarded, want string }{
		{"https://secure.example/probe", "", "https"},
		{"http://plain.example/probe", "", "http"},
		{"http://plain.example/probe", "https", "https"},
	} {
		req := httptest.NewRequest(http.MethodGet, tt.target, nil)
		if tt.forwarded != "" {
			req.Header.Set(fiber.HeaderXForwardedProto, tt.forwarded)
		}
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("spans=%d want 3", len(spans))
	}
	for i, want := range []string{"https", "http", "https"} {
		if got := spanStringAttrs(spans[i].Attributes)["url.scheme"]; got != want {
			t.Errorf("request %d: url.scheme=%q want %q", i, got, want)
		}
	}
}
