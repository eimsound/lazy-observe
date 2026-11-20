package lazyobserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

func TestFiberUnmatchedRouteUsesLowCardinalityAndDoesNotPolluteRoot(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = traceProvider.Shutdown(context.Background()) })
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	collector := &errorEventCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{
		cfg:            Config{ServiceName: "fiber-unmatched"},
		meter:          meterProvider,
		tracer:         traceProvider,
		logger:         logger,
		propagator:     propagation.TraceContext{},
		errorTransport: newErrorTransport(server.URL, "secret", logger, defaultDeliveryConfig(t), nil),
	}
	app := fiber.New()
	app.Use(client.Fiber())
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	for _, target := range []string{"/", "/missing/111", "/missing/222", "/another/333"} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, target, nil), fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		wantStatus := http.StatusNotFound
		if target == "/" {
			wantStatus = http.StatusOK
		}
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s status=%d want=%d", target, resp.StatusCode, wantStatus)
		}
	}
	shutdownClient(t, client)

	spans := exporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("spans=%d want=4", len(spans))
	}
	if spans[0].Name != "GET /" || spanStringAttrs(spans[0].Attributes)["http.route"] != "/" {
		t.Fatalf("root span=%q attrs=%v", spans[0].Name, spanStringAttrs(spans[0].Attributes))
	}
	for i := 1; i < len(spans); i++ {
		attrs := spanStringAttrs(spans[i].Attributes)
		if spans[i].Name != "GET "+unmatchedHTTPRoute || attrs["http.route"] != unmatchedHTTPRoute {
			t.Fatalf("unmatched span %d name=%q route=%q", i, spans[i].Name, attrs["http.route"])
		}
		if spans[i].Status.Code == codes.Error {
			t.Fatalf("unmatched 404 span %d unexpectedly marked Error: %+v", i, spans[i].Status)
		}
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	routeCounts := map[string]uint64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if hist, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == "http.server.request.duration" {
				for _, pt := range hist.DataPoints {
					route, _ := pt.Attributes.Value("http.route")
					routeCounts[route.AsString()] += pt.Count
				}
			}
		}
	}
	if routeCounts["/"] != 1 || routeCounts[unmatchedHTTPRoute] != 3 || len(routeCounts) != 2 {
		t.Fatalf("routeCounts=%v want {/ : 1, %s : 3}", routeCounts, unmatchedHTTPRoute)
	}
	_, _, events := collector.snapshot()
	if len(events) != 0 {
		t.Fatalf("404 requests unexpectedly generated error events: %#v", events)
	}
}

func TestFiber4xxCustomErrorHandlerAndPanicFormatting(t *testing.T) {
	t.Parallel()
	errDomainNotFound := errors.New("order missing")
	errUpstreamDown := errors.New("upstream database offline")

	exporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = traceProvider.Shutdown(context.Background()) })
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	collector := &errorEventCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{
		cfg:            Config{ServiceName: "fiber-errors-custom"},
		meter:          meterProvider,
		tracer:         traceProvider,
		logger:         logger,
		propagator:     propagation.TraceContext{},
		errorTransport: newErrorTransport(server.URL, "secret", logger, defaultDeliveryConfig(t), nil),
	}

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			status := fiber.StatusInternalServerError
			var fe *fiber.Error
			switch {
			case errors.As(err, &fe):
				status = fe.Code
			case errors.Is(err, errDomainNotFound):
				status = fiber.StatusNotFound
			case errors.Is(err, errUpstreamDown):
				status = fiber.StatusBadGateway
			}
			return c.Status(status).JSON(fiber.Map{"handled_by": "custom", "status": status})
		},
	})
	app.Use(client.Fiber())
	app.Get("/bad-request", func(fiber.Ctx) error { return fiber.NewError(fiber.StatusBadRequest, "bad input") })
	app.Get("/domain-404", func(fiber.Ctx) error { return errDomainNotFound })
	app.Get("/upstream-502", func(fiber.Ctx) error { return errUpstreamDown })
	app.Get("/custom-panic", func(fiber.Ctx) error { panic("custom boom") })

	requests := []struct {
		path   string
		status int
	}{
		{"/bad-request", fiber.StatusBadRequest},
		{"/domain-404", fiber.StatusNotFound},
		{"/upstream-502", fiber.StatusBadGateway},
		{"/custom-panic", fiber.StatusInternalServerError},
	}
	for _, tc := range requests {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil), fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s status=%d want=%d", tc.path, resp.StatusCode, tc.status)
		}
		if decodeErr != nil || body["handled_by"] != "custom" {
			t.Fatalf("%s did not use custom ErrorHandler: err=%v body=%v", tc.path, decodeErr, body)
		}
	}
	shutdownClient(t, client)

	spans := exporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("spans=%d want=4", len(spans))
	}
	// 400 *fiber.Error and 404 custom ErrorHandler must NOT mark spans as Error.
	for i, name := range []string{"/bad-request", "/domain-404"} {
		if spans[i].Status.Code == codes.Error || len(spans[i].Events) != 0 {
			t.Fatalf("%s 4xx span marked Error: status=%+v events=%+v", name, spans[i].Status, spans[i].Events)
		}
	}
	// 502 custom ErrorHandler keeps concrete error description (not "Bad Gateway").
	if spans[2].Status.Code != codes.Error || spans[2].Status.Description != errUpstreamDown.Error() {
		t.Fatalf("502 span status=%+v want Error/%q", spans[2].Status, errUpstreamDown.Error())
	}
	// Panic keeps concrete panic description (not overwritten by "Internal Server Error").
	if spans[3].Status.Code != codes.Error || spans[3].Status.Description != "panic: custom boom" {
		t.Fatalf("panic span status=%+v want Error/%q", spans[3].Status, "panic: custom boom")
	}

	// Only 502 and panic should generate Issues (400 and domain 404 must not).
	_, _, events := collector.snapshot()
	if len(events) != 2 {
		t.Fatalf("error events=%d want=2: %#v", len(events), events)
	}
	for _, ev := range events {
		if ev.Tags["http.route"] == "/bad-request" || ev.Tags["http.route"] == "/domain-404" {
			t.Fatalf("4xx unexpectedly captured as Issue: %#v", ev)
		}
	}
}
