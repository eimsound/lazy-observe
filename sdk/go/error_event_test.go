package lazyobserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type errorEventCollector struct {
	mu     sync.Mutex
	auth   []string
	paths  []string
	events []errorEvent
}

func (c *errorEventCollector) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []errorEvent `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	c.paths = append(c.paths, r.URL.Path)
	c.events = append(c.events, body.Events...)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"accepted":1}`)
}

func (c *errorEventCollector) snapshot() ([]string, []string, []errorEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.auth...), append([]string(nil), c.paths...), append([]errorEvent(nil), c.events...)
}

func TestCaptureExceptionQueuesCorrelatedEvent(t *testing.T) {
	t.Parallel()
	collector := &errorEventCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{
		cfg: Config{
			ServiceName: "checkout",
			Release:     "checkout@1.2.3",
			Environment: "test",
			Tags:        map[string]string{"region": "local"},
		},
		logger:         logger,
		errorTransport: newErrorTransport(server.URL, "secret", logger, defaultDeliveryConfig(t), nil),
	}

	eventID := client.CaptureException(sampledContext(), errors.New("payment failed"))
	if eventID == "" {
		t.Fatal("CaptureException returned an empty event ID")
	}
	shutdownClient(t, client)

	auth, paths, events := collector.snapshot()
	if len(events) != 1 || len(auth) != 1 || len(paths) != 1 {
		t.Fatalf("requests auth=%v paths=%v events=%#v", auth, paths, events)
	}
	event := events[0]
	if auth[0] != "Bearer secret" || paths[0] != "/api/events/errors" {
		t.Fatalf("request auth=%q path=%q", auth[0], paths[0])
	}
	if event.EventID != eventID || event.Platform != "go" || event.Exception.Type != "Error" ||
		event.Exception.Value != "payment failed" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.Release != "checkout@1.2.3" || event.Environment != "test" ||
		event.Tags["service.name"] != "checkout" || event.Tags["region"] != "local" {
		t.Fatalf("event metadata missing: %#v", event)
	}
	if event.Contexts == nil || event.Contexts.Trace == nil ||
		event.Contexts.Trace.TraceID != "11111111111111111111111111111111" ||
		event.Contexts.Trace.SpanID != "2222222222222222" {
		t.Fatalf("trace context missing: %#v", event.Contexts)
	}
	if !containsInAppFunction(event.Exception.Stacktrace.Frames, "TestCaptureExceptionQueuesCorrelatedEvent") {
		t.Fatalf("capture call site missing from stack: %#v", event.Exception.Stacktrace.Frames)
	}
}

func TestFiberReportsPanicAndReturnedServerError(t *testing.T) {
	t.Parallel()
	collector := &errorEventCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	traceProvider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = traceProvider.Shutdown(context.Background()) })
	client := &Client{
		cfg:            Config{ServiceName: "fiber-errors"},
		meter:          metricnoop.NewMeterProvider(),
		tracer:         traceProvider,
		logger:         logger,
		propagator:     propagation.TraceContext{},
		errorTransport: newErrorTransport(server.URL, "secret", logger, defaultDeliveryConfig(t), nil),
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(client.Fiber())
	app.Get("/panic", func(*fiber.Ctx) error { panic("boom") })
	app.Get("/server-error", func(*fiber.Ctx) error {
		return fiber.NewError(http.StatusServiceUnavailable, "upstream unavailable")
	})
	app.Get("/client-error", func(*fiber.Ctx) error {
		return fiber.NewError(http.StatusBadRequest, "bad input")
	})

	for _, test := range []struct {
		path   string
		status int
	}{
		{path: "/panic?secret=must-not-be-stored", status: http.StatusInternalServerError},
		{path: "/server-error", status: http.StatusServiceUnavailable},
		{path: "/client-error", status: http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != test.status {
			t.Fatalf("%s status=%d want=%d", test.path, resp.StatusCode, test.status)
		}
	}
	shutdownClient(t, client)

	_, _, events := collector.snapshot()
	if len(events) != 2 {
		t.Fatalf("events=%d want=2: %#v", len(events), events)
	}
	var panicEvent, returnedEvent *errorEvent
	for index := range events {
		switch events[index].Exception.Type {
		case "panic":
			panicEvent = &events[index]
		case "Error":
			returnedEvent = &events[index]
		}
	}
	if panicEvent == nil || returnedEvent == nil {
		t.Fatalf("panic or returned error event missing: %#v", events)
	}
	if panicEvent.URL != "http://example.com/panic" || strings.Contains(panicEvent.URL, "secret") ||
		panicEvent.Tags["http.route"] != "/panic" ||
		panicEvent.Tags["http.response.status_code"] != "500" {
		t.Fatalf("panic request metadata unexpected: %#v", panicEvent)
	}
	if panicEvent.Contexts == nil || panicEvent.Contexts.Trace == nil ||
		panicEvent.Contexts.Trace.TraceID != "11111111111111111111111111111111" {
		t.Fatalf("panic trace context missing: %#v", panicEvent.Contexts)
	}
	if !containsInAppFunction(panicEvent.Exception.Stacktrace.Frames, "TestFiberReportsPanicAndReturnedServerError") {
		t.Fatalf("panic handler missing from stack: %#v", panicEvent.Exception.Stacktrace.Frames)
	}
	if returnedEvent.URL != "http://example.com/server-error" ||
		returnedEvent.Tags["http.response.status_code"] != "503" ||
		len(returnedEvent.Exception.Stacktrace.Frames) != 0 {
		t.Fatalf("returned error metadata unexpected: %#v", returnedEvent)
	}
}

func TestClientShutdownReturnsTerminalErrorExportFailure(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &Client{cfg: Config{ServiceName: "checkout"}, logger: logger}
	client.errorTransport = newErrorTransport(
		server.URL,
		"secret",
		logger,
		defaultDeliveryConfig(t),
		client.recordTelemetryDrop,
	)
	if id := client.CaptureException(context.Background(), errors.New("payment failed")); id == "" {
		t.Fatal("CaptureException returned an empty event ID")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := client.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("Shutdown error=%v", err)
	}
	if attempts.Load() != deliveryDefaultMaxAttempts {
		t.Fatalf("attempts=%d want=%d", attempts.Load(), deliveryDefaultMaxAttempts)
	}
	if got := client.Diagnostics().DroppedTelemetry; got != 1 {
		t.Fatalf("dropped telemetry=%d want=1", got)
	}
}

func containsInAppFunction(frames []errorFrame, want string) bool {
	for _, frame := range frames {
		if frame.InApp && strings.Contains(frame.Function, want) {
			return true
		}
	}
	return false
}

func shutdownClient(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func defaultDeliveryConfig(t *testing.T) DeliveryConfig {
	t.Helper()
	var config DeliveryConfig
	if err := normalizeDeliveryConfig(&config); err != nil {
		t.Fatal(err)
	}
	return config
}
