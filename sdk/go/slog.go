package lazyobserve

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type correlatingHandler struct {
	service string
	otel    slog.Handler
	tee     slog.Handler
}

func (h *correlatingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return (h.otel != nil && h.otel.Enabled(ctx, level)) || (h.tee != nil && h.tee.Enabled(ctx, level))
}

func (h *correlatingHandler) Handle(ctx context.Context, record slog.Record) error {
	record.AddAttrs(slog.String("service.name", h.service))
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	var errs []error
	if h.otel != nil && h.otel.Enabled(ctx, record.Level) {
		errs = append(errs, h.otel.Handle(ctx, record.Clone()))
	}
	if h.tee != nil && h.tee.Enabled(ctx, record.Level) {
		errs = append(errs, h.tee.Handle(ctx, record.Clone()))
	}
	return errors.Join(errs...)
}

func (h *correlatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	if next.otel != nil {
		next.otel = next.otel.WithAttrs(attrs)
	}
	if next.tee != nil {
		next.tee = next.tee.WithAttrs(attrs)
	}
	return &next
}

func (h *correlatingHandler) WithGroup(name string) slog.Handler {
	next := *h
	if next.otel != nil {
		next.otel = next.otel.WithGroup(name)
	}
	if next.tee != nil {
		next.tee = next.tee.WithGroup(name)
	}
	return &next
}
