package lazyobserve

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
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
	app := fiber.New()
	app.Use(client.Fiber())
	app.Get("/orders/:id", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	resp, err := app.Test(httptest.NewRequest("GET", "/orders/42", nil), fiber.TestConfig{Timeout: 0})
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
	app := fiber.New()
	app.Use(client.Fiber())
	app.Get("/panic", func(fiber.Ctx) error { panic("boom") })
	resp, err := app.Test(httptest.NewRequest("GET", "/panic", nil), fiber.TestConfig{Timeout: 0})
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

func TestSlogWithGroupKeepsCorrelationTopLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	handler := &correlatingHandler{
		service: "orders",
		tee:     slog.NewJSONHandler(&buf, nil),
	}
	ctx := sampledContext()
	spanContext := trace.SpanContextFromContext(ctx)
	logger := slog.New(handler).
		With("env", "prod").
		WithGroup("order").
		With("currency", "USD").
		WithGroup("item")
	logger.InfoContext(ctx, "added", "sku", "abc")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log json: %v (%s)", err, buf.String())
	}
	if record["service.name"] != "orders" ||
		record["trace_id"] != spanContext.TraceID().String() ||
		record["span_id"] != spanContext.SpanID().String() ||
		record["env"] != "prod" {
		t.Fatalf("top-level correlation attrs missing: %#v", record)
	}
	orderGroup, ok := record["order"].(map[string]any)
	if !ok || orderGroup["currency"] != "USD" {
		t.Fatalf("order group unexpected: %#v", record)
	}
	if _, leaked := orderGroup["trace_id"]; leaked {
		t.Fatalf("trace_id leaked into order group: %#v", orderGroup)
	}
	itemGroup, ok := orderGroup["item"].(map[string]any)
	if !ok || itemGroup["sku"] != "abc" {
		t.Fatalf("nested item group unexpected: %#v", orderGroup)
	}
}

func TestDegradedRespectsTeeToStderrFalse(t *testing.T) {
	t.Parallel()
	disabled := false
	lo := Init(Config{DSN: "not-a-dsn", TeeToStderr: &disabled})
	if lo.Err() == nil {
		t.Fatal("expected degraded client error")
	}
	if lo.Slog().Enabled(context.Background(), slog.LevelError) {
		t.Fatal("degraded client ignored TeeToStderr: false")
	}
}

func TestRegisterRuntimeMetricsReportsExpectedInstruments(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	client := &Client{meter: provider}
	if err := client.registerRuntimeMetrics(); err != nil {
		t.Fatal(err)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			seen[m.Name] = true
		}
	}
	for _, want := range []string{
		"process.runtime.go.goroutines",
		"process.runtime.go.memory.heap_alloc",
		"process.runtime.go.gc.count",
	} {
		if !seen[want] {
			t.Fatalf("missing runtime metric %q in %v", want, seen)
		}
	}
}

func TestShutdownStopsProfilingBeforeErrorDrainTimeoutAndBoundsNoDeadline(t *testing.T) {
	t.Run("profiling loop stops even when error drain times out", func(t *testing.T) {
		unblock := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(unblock) })

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		cfg, err := normalizedConfig(Config{ServiceName: "shutdown-order"})
		if err != nil {
			t.Fatal(err)
		}
		profileCtx, profileStop := context.WithCancel(context.Background())
		ingest := newIngestClient(server.URL, "secret", &http.Client{Timeout: time.Second})
		client := &Client{
			cfg:                 cfg,
			logger:              logger,
			ingest:              ingest,
			profileStop:         profileStop,
			profileDone:         make(chan struct{}),
			cpuProfiler:         fakeCPUProfiler(time.Second),
			profileInitialDelay: func(time.Duration) time.Duration { return 0 },
			errorTransport:      newErrorTransport(ingest, logger, cfg.Delivery, nil),
		}
		client.profileDelivery = newProfileDelivery(client)
		go client.profilingLoop(profileCtx)

		client.CaptureException(context.Background(), errors.New("hanging delivery"))
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		_ = client.Shutdown(ctx)

		select {
		case <-client.profileDone:
		case <-time.After(time.Second):
			t.Fatal("profilingLoop was left running after error drain timeout")
		}
	})

	t.Run("Shutdown(context.Background()) is bounded by default timeout", func(t *testing.T) {
		prev := defaultDeliveryShutdownTimeout
		defaultDeliveryShutdownTimeout = 40 * time.Millisecond
		t.Cleanup(func() { defaultDeliveryShutdownTimeout = prev })

		unblock := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(unblock) })

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := &Client{
			cfg:    Config{ServiceName: "no-deadline"},
			logger: logger,
			errorTransport: newErrorTransport(
				newIngestClient(server.URL, "secret", nil),
				logger,
				defaultDeliveryConfig(t),
				nil,
			),
		}
		client.CaptureException(context.Background(), errors.New("hang"))
		startedAt := time.Now()
		err := client.Shutdown(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown(Background) err=%v want DeadlineExceeded", err)
		}
		if elapsed := time.Since(startedAt); elapsed > 300*time.Millisecond {
			t.Fatalf("Shutdown(Background) blocked for %s", elapsed)
		}
	})

	t.Run("defer-created shutdown timeout flushes after process uptime exceeds timeout", func(t *testing.T) {
		collector := &errorEventCollector{}
		server := httptest.NewServer(http.HandlerFunc(collector.handler))
		t.Cleanup(server.Close)

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := &Client{
			cfg:    Config{ServiceName: "deferred-shutdown"},
			logger: logger,
			errorTransport: newErrorTransport(
				newIngestClient(server.URL, "secret", nil),
				logger,
				defaultDeliveryConfig(t),
				nil,
			),
		}
		var shutdownErr error
		func() {
			const shutdownBudget = 50 * time.Millisecond
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
				defer cancel()
				shutdownErr = client.Shutdown(ctx)
			}()
			// Simulate process running longer than shutdownBudget before exiting.
			time.Sleep(80 * time.Millisecond)
			client.CaptureException(context.Background(), errors.New("late error before exit"))
		}()
		if shutdownErr != nil {
			t.Fatalf("deferred shutdown failed: %v", shutdownErr)
		}
		_, _, events := collector.snapshot()
		if len(events) != 1 {
			t.Fatalf("accepted events=%d want=1", len(events))
		}
	})
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

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func TestBuildResourceAvoidsSchemaURLConflict(t *testing.T) {
	t.Parallel()
	const foreignSchemaURL = "https://opentelemetry.io/schemas/9.99.0"
	base := resource.NewWithAttributes(
		foreignSchemaURL,
		attribute.String("service.name", "host-default"),
		attribute.String("host.arch", "amd64"),
	)
	merged, err := buildResource(base, "orders-api", "prod")
	if err != nil {
		t.Fatalf("buildResource returned error on distinct SchemaURL: %v", err)
	}
	if merged.SchemaURL() != foreignSchemaURL {
		t.Fatalf("SchemaURL=%q want %q", merged.SchemaURL(), foreignSchemaURL)
	}
	attrs := map[string]string{}
	for _, kv := range merged.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "orders-api" || attrs["lazy_observe.project"] != "prod" ||
		attrs["host.arch"] != "amd64" {
		t.Fatalf("merged resource attributes=%v", attrs)
	}
}

var registerStubSQLDriverOnce sync.Once

type stubSQLDriver struct{}
type stubSQLConn struct{}

func (stubSQLDriver) Open(string) (driver.Conn, error) { return stubSQLConn{}, nil }
func (stubSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}
func (stubSQLConn) Close() error              { return nil }
func (stubSQLConn) Begin() (driver.Tx, error) { return nil, errors.New("not implemented") }

func stubSQLDriverName() string {
	const name = "lazyobserve_test_stub"
	registerStubSQLDriverOnce.Do(func() {
		sql.Register(name, stubSQLDriver{})
	})
	return name
}

func hasDBPoolMetrics(t *testing.T, reader *sdkmetric.ManualReader) bool {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if strings.HasPrefix(m.Name, "db.sql.") || strings.HasPrefix(m.Name, "db.client.connections") {
				return true
			}
		}
	}
	return false
}

func TestWrapDBLifecycleUnregistersOnCancelAndShutdown(t *testing.T) {
	t.Parallel()
	driverName := stubSQLDriverName()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	client := &Client{
		cfg:        Config{ServiceName: "db-lifecycle"},
		meter:      provider,
		tracer:     tracenoop.NewTracerProvider(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		propagator: propagation.TraceContext{},
	}

	// Already-cancelled context must fail fast without leaking registrations.
	cancelledCtx, cancelAlready := context.WithCancel(context.Background())
	cancelAlready()
	if _, err := client.WrapDBContext(cancelledCtx, driverName, "dsn"); !errors.Is(err, context.Canceled) {
		t.Fatalf("WrapDBContext(cancelled) err=%v want context.Canceled", err)
	}
	if got := client.activeDBRegistrations(); got != 0 {
		t.Fatalf("activeDBRegistrations=%d want 0", got)
	}

	// Cancelling dbCtx unregisters pool metrics without waiting for Shutdown.
	dbCtx, cancelDB := context.WithCancel(context.Background())
	db1, err := client.WrapDBContext(dbCtx, driverName, "dsn-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db1.Close() })
	if got := client.activeDBRegistrations(); got != 1 {
		t.Fatalf("activeDBRegistrations=%d want 1", got)
	}
	if !hasDBPoolMetrics(t, reader) {
		t.Fatal("expected DB pool metrics while WrapDBContext is active")
	}

	cancelDB()
	deadline := time.Now().Add(time.Second)
	for client.activeDBRegistrations() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := client.activeDBRegistrations(); got != 0 {
		t.Fatalf("activeDBRegistrations after cancel=%d want 0", got)
	}
	if hasDBPoolMetrics(t, reader) {
		t.Fatal("DB pool metrics still reported after WrapDBContext cancellation")
	}

	// WrapDB (background context) unregisters on Client.Shutdown.
	db2, err := client.WrapDB(driverName, "dsn-2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	if got := client.activeDBRegistrations(); got != 1 || !hasDBPoolMetrics(t, reader) {
		t.Fatalf("WrapDB active=%d hasMetrics=%v want 1/true", got, hasDBPoolMetrics(t, reader))
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.activeDBRegistrations(); got != 0 {
		t.Fatalf("activeDBRegistrations after Shutdown=%d want 0", got)
	}
	if hasDBPoolMetrics(t, reader) {
		t.Fatal("DB pool metrics still reported after Shutdown")
	}

	// WrapDB after Shutdown must not leak a new registration.
	db3, err := client.WrapDB(driverName, "dsn-3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db3.Close() })
	if got := client.activeDBRegistrations(); got != 0 || hasDBPoolMetrics(t, reader) {
		t.Fatalf("post-Shutdown WrapDB leaked registration: active=%d hasMetrics=%v", got, hasDBPoolMetrics(t, reader))
	}
}
