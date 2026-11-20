package lazyobserve

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"go.opentelemetry.io/otel/trace"
)

type correlatingHandlerOp struct {
	group string
	attrs []slog.Attr
}

type correlatingHandler struct {
	service  string
	otel     slog.Handler
	tee      slog.Handler
	hasGroup bool
	ops      []correlatingHandlerOp
}

func (h *correlatingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return (h.otel != nil && h.otel.Enabled(ctx, level)) || (h.tee != nil && h.tee.Enabled(ctx, level))
}

func (h *correlatingHandler) Handle(ctx context.Context, record slog.Record) error {
	corrAttrs := make([]slog.Attr, 0, 3)
	corrAttrs = append(corrAttrs, slog.String("service.name", h.service))
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		corrAttrs = append(corrAttrs,
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}

	var out slog.Record
	if !h.hasGroup {
		out = record.Clone()
		out.AddAttrs(corrAttrs...)
	} else {
		out = slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
		out.AddAttrs(corrAttrs...)
		grouped := h.wrapGroupedAttrs(record)
		if len(grouped) > 0 {
			out.AddAttrs(grouped...)
		}
	}

	var errs []error
	if h.otel != nil && h.otel.Enabled(ctx, out.Level) {
		errs = append(errs, h.otel.Handle(ctx, out.Clone()))
	}
	if h.tee != nil && h.tee.Enabled(ctx, out.Level) {
		errs = append(errs, h.tee.Handle(ctx, out.Clone()))
	}
	return errors.Join(errs...)
}

func (h *correlatingHandler) wrapGroupedAttrs(record slog.Record) []slog.Attr {
	current := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		if !attr.Equal(slog.Attr{}) {
			current = append(current, attr)
		}
		return true
	})
	for i := len(h.ops) - 1; i >= 0; i-- {
		op := h.ops[i]
		if op.group != "" {
			if len(current) == 0 {
				continue
			}
			args := make([]any, len(current))
			for idx, attr := range current {
				args[idx] = attr
			}
			current = []slog.Attr{slog.Group(op.group, args...)}
		} else if len(op.attrs) > 0 {
			combined := make([]slog.Attr, 0, len(op.attrs)+len(current))
			combined = append(combined, op.attrs...)
			combined = append(combined, current...)
			current = combined
		}
	}
	return current
}

func (h *correlatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := *h
	if h.hasGroup {
		copied := make([]slog.Attr, 0, len(attrs))
		for _, attr := range attrs {
			if !attr.Equal(slog.Attr{}) {
				copied = append(copied, attr)
			}
		}
		if len(copied) == 0 {
			return h
		}
		next.ops = append(slices.Clip(h.ops), correlatingHandlerOp{attrs: copied})
		return &next
	}
	if next.otel != nil {
		next.otel = next.otel.WithAttrs(attrs)
	}
	if next.tee != nil {
		next.tee = next.tee.WithAttrs(attrs)
	}
	return &next
}

func (h *correlatingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.hasGroup = true
	next.ops = append(slices.Clip(h.ops), correlatingHandlerOp{group: name})
	return &next
}
