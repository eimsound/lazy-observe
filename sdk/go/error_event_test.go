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

	"github.com/gofiber/fiber/v3"
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
	app := fiber.New()
	app.Use(client.Fiber())
	app.Get("/panic", func(fiber.Ctx) error { panic("boom") })
	app.Get("/server-error", func(fiber.Ctx) error {
		return fiber.NewError(http.StatusServiceUnavailable, "upstream unavailable")
	})
	app.Get("/client-error", func(fiber.Ctx) error {
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
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
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

func TestFrameModuleAndInAppFiltering(t *testing.T) {
	t.Parallel()
	moduleCases := []struct {
		fn   string
		want string
	}{
		{"main.handler", "main"},
		{"main.handler.func1", "main"},
		{"main.(*Server).handle.func1.1", "main"},
		{"github.com/acme/orders/internal/pay.(*Client).Charge.func2", "github.com/acme/orders/internal/pay"},
		{"github.com/acme/pkg.Map[github.com/other/pkg.Item]", "github.com/acme/pkg"},
		{"net/http.(*conn).serve", "net/http"},
		{"runtime.gopanic", "runtime"},
		{"", ""},
	}
	for _, tc := range moduleCases {
		if got := frameModule(tc.fn); got != tc.want {
			t.Errorf("frameModule(%q) = %q, want %q", tc.fn, got, tc.want)
		}
	}

	const mainMod = "github.com/acme/checkout"
	deps := []string{
		"github.com/valyala/fasthttp",
		"github.com/gofiber/fiber/v3",
		"github.com/thirdparty/lib",
	}
	inAppCases := []struct {
		name string
		fn   string
		file string
		want bool
	}{
		{"app main", "main.handleOrder.func1", "/app/main.go", true},
		{"app internal pkg", "github.com/acme/checkout/orders.(*Service).Create", "/app/orders/create.go", true},
		{
			"sdk test file",
			"github.com/eimsound/lazy-observe/sdk/go.TestCapture",
			"/repo/sdk/go/error_event_test.go",
			true,
		},
		{
			"sdk prod file",
			"github.com/eimsound/lazy-observe/sdk/go.(*Client).Fiber.func1",
			"/repo/sdk/go/fiber.go",
			false,
		},
		{
			"fasthttp",
			"github.com/valyala/fasthttp.(*Server).serveConn",
			"/go/pkg/mod/github.com/valyala/fasthttp@v1.78.0/server.go",
			false,
		},
		{
			"fiber",
			"github.com/gofiber/fiber/v3.(*App).next",
			"/go/pkg/mod/github.com/gofiber/fiber/v3@v3.7.0/router.go",
			false,
		},
		{"stdlib net/http", "net/http.(*conn).serve", "/usr/local/go/src/net/http/server.go", false},
		{"stdlib runtime", "runtime.gopanic", "/usr/local/go/src/runtime/panic.go", false},
		{"stdlib testing", "testing.tRunner", "/usr/local/go/src/testing/testing.go", false},
		{"third-party dep", "github.com/thirdparty/lib.Call", "/app/trimmed/lib.go", false},
		{
			"mod cache path",
			"github.com/other/mod.Run",
			"/home/user/go/pkg/mod/github.com/other/mod@v1.0.0/mod.go",
			false,
		},
		{"vendor path", "github.com/other/mod.Run", "/app/vendor/github.com/other/mod/mod.go", false},
	}
	for _, tc := range inAppCases {
		if got := isInAppFrameWithBuildInfo(tc.fn, tc.file, mainMod, deps); got != tc.want {
			t.Errorf("%s: isInAppFrame(%q, %q) = %v, want %v", tc.name, tc.fn, tc.file, got, tc.want)
		}
	}
}

func TestErrorTransport4xxDropsAsServerRejectedAndHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	t.Run("422 rejected without retry", func(t *testing.T) {
		t.Parallel()
		var attempts atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			http.Error(w, `{"error":"invalid event"}`, http.StatusUnprocessableEntity)
		}))
		t.Cleanup(server.Close)

		var mu sync.Mutex
		var droppedReasons []string
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := &Client{cfg: Config{ServiceName: "checkout"}, logger: logger}
		client.errorTransport = newErrorTransport(
			server.URL,
			"secret",
			logger,
			defaultDeliveryConfig(t),
			func(signal, reason string, count int) {
				mu.Lock()
				droppedReasons = append(droppedReasons, signal+":"+reason)
				mu.Unlock()
				client.recordTelemetryDrop(signal, reason, count)
			},
		)
		client.CaptureException(context.Background(), errors.New("bad event"))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := client.Shutdown(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 422") {
			t.Fatalf("Shutdown err=%v want HTTP 422", err)
		}
		if attempts.Load() != 1 {
			t.Fatalf("422 attempts=%d want=1", attempts.Load())
		}
		mu.Lock()
		defer mu.Unlock()
		if len(droppedReasons) != 1 || droppedReasons[0] != "error:server_rejected" {
			t.Fatalf("droppedReasons=%v want=[error:server_rejected]", droppedReasons)
		}
	})

	t.Run("503 with Retry-After succeeds after wait", func(t *testing.T) {
		t.Parallel()
		var attempts atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if attempts.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accepted":1}`)
		}))
		t.Cleanup(server.Close)

		var mu sync.Mutex
		var waited []time.Duration
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		transport := newErrorTransport(server.URL, "secret", logger, defaultDeliveryConfig(t), nil)
		transport.delivery.options.Wait = func(_ context.Context, delay time.Duration) error {
			mu.Lock()
			waited = append(waited, delay)
			mu.Unlock()
			return nil
		}
		client := &Client{cfg: Config{ServiceName: "checkout"}, logger: logger, errorTransport: transport}
		client.CaptureException(context.Background(), errors.New("transient"))
		shutdownClient(t, client)
		if attempts.Load() != 2 {
			t.Fatalf("attempts=%d want=2", attempts.Load())
		}
		mu.Lock()
		defer mu.Unlock()
		if len(waited) != 1 || waited[0] < time.Second || waited[0] > 1250*time.Millisecond {
			t.Fatalf("waited=%v want [1s, 1.25s]", waited)
		}
	})
}

func TestErrorEventSizeBudgetAnd413BatchSplit(t *testing.T) {
	t.Parallel()
	t.Run("oversized event truncated to <= 200KB before enqueue", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var accepted []errorEvent
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var envelope struct {
				Events []json.RawMessage `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			for _, raw := range envelope.Events {
				if len(raw) > maxErrorEventBytes {
					http.Error(w, "event exceeds 200KB", http.StatusRequestEntityTooLarge)
					return
				}
				var ev errorEvent
				if err := json.Unmarshal(raw, &ev); err != nil {
					http.Error(w, err.Error(), http.StatusUnprocessableEntity)
					return
				}
				mu.Lock()
				accepted = append(accepted, ev)
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accepted":1}`)
		}))
		t.Cleanup(server.Close)

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := &Client{
			cfg: Config{
				ServiceName: "checkout",
				Tags:        map[string]string{"huge": strings.Repeat("值", 40_000)},
			},
			logger:         logger,
			errorTransport: newErrorTransport(server.URL, "secret", logger, defaultDeliveryConfig(t), nil),
		}
		hugeErr := errors.New(strings.Repeat("错\x00", 120_000))
		client.CaptureException(context.Background(), hugeErr)
		client.CaptureException(context.Background(), errors.New("sibling normal error"))
		shutdownClient(t, client)

		mu.Lock()
		defer mu.Unlock()
		if len(accepted) != 2 {
			t.Fatalf("accepted=%d want=2", len(accepted))
		}
		for _, ev := range accepted {
			raw, err := json.Marshal(ev)
			if err != nil || len(raw) > maxErrorEventBytes {
				t.Fatalf("event size=%d err=%v exceeds %d", len(raw), err, maxErrorEventBytes)
			}
		}
	})

	t.Run("413 on batch splits and delivers healthy siblings", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var acceptedIDs []string
		var dropReasons []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Events []errorEvent `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for _, ev := range body.Events {
				if strings.Contains(ev.Message, "poison-too-large") {
					http.Error(w, "too large", http.StatusRequestEntityTooLarge)
					return
				}
			}
			mu.Lock()
			for _, ev := range body.Events {
				acceptedIDs = append(acceptedIDs, ev.EventID)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accepted":1}`)
		}))
		t.Cleanup(server.Close)

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := &Client{cfg: Config{ServiceName: "checkout"}, logger: logger}
		client.errorTransport = newErrorTransport(
			server.URL,
			"secret",
			logger,
			defaultDeliveryConfig(t),
			func(signal, reason string, count int) {
				mu.Lock()
				dropReasons = append(dropReasons, signal+":"+reason)
				mu.Unlock()
				client.recordTelemetryDrop(signal, reason, count)
			},
		)
		id1 := client.CaptureException(context.Background(), errors.New("ok-1"))
		_ = client.CaptureException(context.Background(), errors.New("poison-too-large"))
		id2 := client.CaptureException(context.Background(), errors.New("ok-2"))
		id3 := client.CaptureException(context.Background(), errors.New("ok-3"))
		shutdownClient(t, client)

		mu.Lock()
		defer mu.Unlock()
		if len(acceptedIDs) != 3 {
			t.Fatalf("acceptedIDs=%v want 3 (%s, %s, %s)", acceptedIDs, id1, id2, id3)
		}
		if client.Diagnostics().DroppedTelemetry != 1 || len(dropReasons) != 1 ||
			dropReasons[0] != "error:server_rejected" {
			t.Fatalf(
				"drops=%d reasons=%v want 1/error:server_rejected",
				client.Diagnostics().DroppedTelemetry,
				dropReasons,
			)
		}
	})
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
