package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	lazyobserve "github.com/eimsound/lazy-observe/sdk/go"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const exampleInstrumentation = "github.com/eimsound/lazy-observe/sdk/go/examples/fiber"

type exampleServer struct {
	db           *sql.DB
	tracer       trace.Tracer
	workDuration metric.Float64Histogram
	workRequests metric.Int64Counter
	llmClient    *http.Client
	llmURL       string
}

func newExampleApp(lo *lazyobserve.Client, db *sql.DB, llmURL string) *fiber.App {
	meter := lo.MeterProvider().Meter(exampleInstrumentation)
	workDuration, _ := meter.Float64Histogram(
		"example.work.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of example business workloads."),
	)
	workRequests, _ := meter.Int64Counter(
		"example.work.requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Number of completed example business workloads."),
	)
	server := &exampleServer{
		db:           db,
		tracer:       lo.TracerProvider().Tracer(exampleInstrumentation),
		workDuration: workDuration,
		workRequests: workRequests,
		llmClient:    &http.Client{Transport: lo.LLMTransport(nil), Timeout: 10 * time.Second},
		llmURL:       strings.TrimRight(llmURL, "/"),
	}

	app := fiber.New()
	app.Use(lo.Fiber())
	app.Get("/", server.index)
	app.Get("/db", server.database)
	app.Get("/trace/:orderID", server.orderTrace)
	app.Get("/performance/fast", server.workload("fast", 5*time.Millisecond, fiber.StatusOK))
	app.Get("/performance/slow", server.workload("slow", 150*time.Millisecond, fiber.StatusOK))
	app.Get("/performance/error", server.workload("error", 30*time.Millisecond, fiber.StatusServiceUnavailable))
	app.Get("/panic", func(fiber.Ctx) error { panic("example panic") })
	app.Get("/cpu", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"result": cpuWork(250 * time.Millisecond)})
	})
	app.Get("/llm", server.llm)
	return app
}

func (s *exampleServer) index(c fiber.Ctx) error {
	slog.InfoContext(c.Context(), "hello from Fiber")
	return c.JSON(fiber.Map{
		"service": "lazy-observe-fiber-example",
		"routes": []string{
			"/trace/:orderID",
			"/performance/fast",
			"/performance/slow",
			"/performance/error",
			"/db",
			"/cpu",
			"/llm",
			"/panic",
		},
	})
}

func (s *exampleServer) database(c fiber.Ctx) error {
	var value int
	if err := s.db.QueryRowContext(c.Context(), "SELECT 42").Scan(&value); err != nil {
		return err
	}
	slog.InfoContext(c.Context(), "database query complete", "value", value)
	return c.JSON(fiber.Map{"value": value, "trace_id": traceID(c.Context())})
}

// orderTrace demonstrates one server span, three application spans, a DB child
// span, and a log correlated to the same trace.
func (s *exampleServer) orderTrace(c fiber.Ctx) error {
	requestCtx := c.Context()
	// Params borrows Fiber's request buffer; the span attribute and log record
	// below outlive the request, so keep an owned copy.
	orderID := strings.Clone(c.Params("orderID"))

	_, validate := s.tracer.Start(requestCtx, "validate order",
		trace.WithAttributes(attribute.String("example.order.id", orderID)),
	)
	time.Sleep(4 * time.Millisecond)
	validate.AddEvent("order accepted")
	validate.End()

	loadCtx, load := s.tracer.Start(requestCtx, "load order")
	var itemCount int
	err := s.db.QueryRowContext(loadCtx, "SELECT 3").Scan(&itemCount)
	if err != nil {
		load.RecordError(err)
		load.SetStatus(codes.Error, err.Error())
		load.End()
		return err
	}
	load.SetAttributes(attribute.Int("example.order.item_count", itemCount))
	load.End()

	calculateCtx, calculate := s.tracer.Start(requestCtx, "calculate order total")
	time.Sleep(6 * time.Millisecond)
	total := itemCount * 42
	calculate.SetAttributes(attribute.Int("example.order.total", total))
	slog.InfoContext(calculateCtx, "order trace complete", "order_id", orderID, "total", total)
	calculate.End()

	return c.JSON(fiber.Map{
		"order_id": orderID,
		"items":    itemCount,
		"total":    total,
		"trace_id": traceID(requestCtx),
	})
}

func (s *exampleServer) workload(name string, delay time.Duration, status int) fiber.Handler {
	return func(c fiber.Ctx) error {
		actualDelay := delay
		if raw := strings.TrimSpace(c.Query("delay_ms")); raw != "" {
			milliseconds, err := strconv.Atoi(raw)
			if err != nil || milliseconds < 1 || milliseconds > 1_000 {
				return fiber.NewError(fiber.StatusBadRequest, "delay_ms must be between 1 and 1000")
			}
			actualDelay = time.Duration(milliseconds) * time.Millisecond
		}
		started := time.Now()
		ctx, span := s.tracer.Start(c.Context(), "run "+name+" workload",
			trace.WithAttributes(attribute.String("example.workload", name)),
		)
		time.Sleep(actualDelay)
		if status >= http.StatusInternalServerError {
			err := errors.New("simulated upstream failure")
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		elapsed := time.Since(started)
		attrs := metric.WithAttributes(
			attribute.String("example.workload", name),
			attribute.Int("http.response.status_code", status),
		)
		s.workDuration.Record(ctx, elapsed.Seconds(), attrs)
		s.workRequests.Add(ctx, 1, attrs)
		span.SetAttributes(attribute.Int64("example.work.duration_ms", elapsed.Milliseconds()))
		span.End()

		slog.InfoContext(ctx, "performance workload complete",
			"workload", name,
			"duration", elapsed,
			"status", status,
		)
		return c.Status(status).JSON(fiber.Map{
			"workload":    name,
			"duration_ms": elapsed.Milliseconds(),
			"trace_id":    traceID(c.Context()),
		})
	}
}

func (s *exampleServer) llm(c fiber.Ctx) error {
	stream := fiber.Query[bool](c, "stream", false)
	payload := `{"model":"e2e-request-model","messages":[{"role":"user","content":"e2e secret prompt"}],"stream":` + strconv.FormatBool(
		stream,
	) + `}`
	req, err := http.NewRequestWithContext(
		c.Context(),
		http.MethodPost,
		s.llmURL+"/v1/chat/completions",
		strings.NewReader(payload),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.llmClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	c.Status(resp.StatusCode)
	return c.Send(body)
}

func traceID(ctx context.Context) string {
	spanContext := trace.SpanFromContext(ctx).SpanContext()
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

//go:noinline
func cpuWork(duration time.Duration) uint64 {
	deadline := time.Now().Add(duration)
	var value uint64 = 1
	for time.Now().Before(deadline) {
		value = value*1664525 + 1013904223
	}
	return value
}
