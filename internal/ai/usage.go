package ai

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// TotalTokens returns the combined input and output token count.
func (u Usage) TotalTokens() int {
	return u.InputTokens + u.OutputTokens
}

// Add combines two token usage snapshots.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + other.InputTokens,
		OutputTokens: u.OutputTokens + other.OutputTokens,
	}
}

type operationContextKey struct{}

// WithOperation tags a context with a low-cardinality AI operation label
// (for example "assistant_query", "assistant_explain", "issue_ai", or "connectivity_test").
func WithOperation(ctx context.Context, operation string) context.Context {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return ctx
	}
	return context.WithValue(ctx, operationContextKey{}, operation)
}

// OperationFromContext extracts the operation label set by WithOperation,
// falling back to "unknown".
func OperationFromContext(ctx context.Context) string {
	if ctx != nil {
		if op, ok := ctx.Value(operationContextKey{}).(string); ok && op != "" {
			return op
		}
	}
	return "unknown"
}

// UsageEvent describes one completed provider invocation for telemetry or
// domain-level accounting callbacks.
type UsageEvent struct {
	Provider  string
	Model     string
	Operation string
	Stream    bool
	Usage     Usage
	Duration  time.Duration
	ErrKind   ErrorKind
}

// UsageMetricsOptions configures the OTel + callback instrumentation decorator.
type UsageMetricsOptions struct {
	Provider      string
	Model         string
	MeterProvider metric.MeterProvider
	OnUsage       func(context.Context, UsageEvent)
}

// WithUsageMetrics wraps client so every Chat and ChatStream call records
// low-cardinality OTel token, request, and latency metrics and invokes the
// optional OnUsage callback.
func WithUsageMetrics(client Client, opts UsageMetricsOptions) Client {
	if client == nil {
		return nil
	}
	if opts.MeterProvider == nil && opts.OnUsage == nil {
		return client
	}
	mp := opts.MeterProvider
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	meter := mp.Meter("github.com/eimsound/lazy-observe/internal/ai")
	tokens, err := meter.Int64Counter(
		"lazy_observe.ai.tokens",
		metric.WithDescription("AI provider tokens consumed, split by direction (input, output, total)."),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		tokens, _ = noop.NewMeterProvider().Meter("").Int64Counter("lazy_observe.ai.tokens")
	}
	requests, err := meter.Int64Counter(
		"lazy_observe.ai.requests",
		metric.WithDescription("AI provider requests by provider, model, operation, and status."),
	)
	if err != nil {
		requests, _ = noop.NewMeterProvider().Meter("").Int64Counter("lazy_observe.ai.requests")
	}
	durationMs, err := meter.Float64Histogram(
		"lazy_observe.ai.request_duration_ms",
		metric.WithDescription("AI provider request duration in milliseconds."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		durationMs, _ = noop.NewMeterProvider().Meter("").Float64Histogram("lazy_observe.ai.request_duration_ms")
	}
	return &instrumentedClient{
		next:       client,
		provider:   normalizeMetricLabel(opts.Provider, "unknown"),
		model:      normalizeMetricLabel(opts.Model, "unknown"),
		tokens:     tokens,
		requests:   requests,
		durationMs: durationMs,
		onUsage:    opts.OnUsage,
	}
}

type instrumentedClient struct {
	next       Client
	provider   string
	model      string
	tokens     metric.Int64Counter
	requests   metric.Int64Counter
	durationMs metric.Float64Histogram
	onUsage    func(context.Context, UsageEvent)
}

func (c *instrumentedClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	started := time.Now()
	resp, err := c.next.Chat(ctx, req)
	elapsed := time.Since(started)
	model := normalizeMetricLabel(resp.Model, c.model)
	c.record(ctx, UsageEvent{
		Provider:  c.provider,
		Model:     model,
		Operation: OperationFromContext(ctx),
		Stream:    false,
		Usage:     resp.Usage,
		Duration:  elapsed,
		ErrKind:   errorKindOf(err),
	})
	return resp, err
}

func (c *instrumentedClient) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	started := time.Now()
	op := OperationFromContext(ctx)
	stream, err := c.next.ChatStream(ctx, req)
	if err != nil {
		c.record(ctx, UsageEvent{
			Provider:  c.provider,
			Model:     c.model,
			Operation: op,
			Stream:    true,
			Duration:  time.Since(started),
			ErrKind:   errorKindOf(err),
		})
		return nil, err
	}
	return &instrumentedStream{
		next:      stream,
		ctx:       ctx,
		client:    c,
		operation: op,
		started:   started,
		model:     c.model,
	}, nil
}

func (c *instrumentedClient) record(ctx context.Context, event UsageEvent) {
	status := "ok"
	if event.ErrKind != "" {
		status = "error"
	}
	baseAttrs := []attribute.KeyValue{
		attribute.String("provider", event.Provider),
		attribute.String("model", event.Model),
		attribute.String("operation", event.Operation),
	}
	reqAttrs := append(append([]attribute.KeyValue(nil), baseAttrs...), attribute.String("status", status))
	if event.ErrKind != "" {
		reqAttrs = append(reqAttrs, attribute.String("error_kind", string(event.ErrKind)))
	}
	c.requests.Add(ctx, 1, metric.WithAttributes(reqAttrs...))
	c.durationMs.Record(
		ctx,
		float64(event.Duration.Microseconds())/1000.0,
		metric.WithAttributes(
			attribute.String("provider", event.Provider),
			attribute.String("model", event.Model),
			attribute.String("operation", event.Operation),
			attribute.String("status", status),
		),
	)
	if event.Usage.InputTokens > 0 {
		c.tokens.Add(
			ctx,
			int64(event.Usage.InputTokens),
			metric.WithAttributes(
				append(append([]attribute.KeyValue(nil), baseAttrs...), attribute.String("direction", "input"))...),
		)
	}
	if event.Usage.OutputTokens > 0 {
		c.tokens.Add(
			ctx,
			int64(event.Usage.OutputTokens),
			metric.WithAttributes(
				append(append([]attribute.KeyValue(nil), baseAttrs...), attribute.String("direction", "output"))...),
		)
	}
	if total := event.Usage.TotalTokens(); total > 0 {
		c.tokens.Add(
			ctx,
			int64(total),
			metric.WithAttributes(
				append(append([]attribute.KeyValue(nil), baseAttrs...), attribute.String("direction", "total"))...),
		)
	}
	if c.onUsage != nil {
		c.onUsage(ctx, event)
	}
}

type instrumentedStream struct {
	next      Stream
	ctx       context.Context
	client    *instrumentedClient
	operation string
	started   time.Time
	model     string
	once      sync.Once
}

func (s *instrumentedStream) Next() bool {
	if s.next.Next() {
		if m := strings.TrimSpace(s.next.Chunk().Model); m != "" {
			s.model = m
		}
		return true
	}
	s.finish(s.next.Err())
	return false
}

func (s *instrumentedStream) Chunk() ChatChunk { return s.next.Chunk() }
func (s *instrumentedStream) Err() error       { return s.next.Err() }

func (s *instrumentedStream) Close() error {
	err := s.next.Close()
	streamErr := s.next.Err()
	if streamErr == nil {
		streamErr = err
	}
	s.finish(streamErr)
	return err
}

func (s *instrumentedStream) finish(err error) {
	s.once.Do(func() {
		s.client.record(s.ctx, UsageEvent{
			Provider:  s.client.provider,
			Model:     normalizeMetricLabel(s.model, s.client.model),
			Operation: s.operation,
			Stream:    true,
			Duration:  time.Since(s.started),
			ErrKind:   errorKindOf(err),
		})
	})
}

func errorKindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var typed *Error
	if errors.As(err, &typed) && typed.Kind != "" {
		return typed.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTimeout
	}
	return ErrorProvider
}

func normalizeMetricLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
