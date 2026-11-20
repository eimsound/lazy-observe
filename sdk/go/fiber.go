package lazyobserve

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const unmatchedHTTPRoute = "_unmatched"

var httpServerRequestDurationBucketsSeconds = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

// Fiber returns explicit Fiber v3 HTTP server instrumentation. Fiber v2 is not
// supported: the handler takes the v3 fiber.Ctx interface.
//
// Without fiber.Config.Immutable, Fiber's request accessors (Method, Path,
// Hostname, Get, Route fallbacks, ...) return strings that borrow pooled request
// buffers which the next request overwrites. Every such value that outlives the
// handler (span attributes exported in batches, metric attributes used as
// aggregation keys, error-event tags, log records, propagated trace state) is
// copied with strings.Clone at the point it is read, so the middleware is
// correct regardless of the application's Immutable setting.
func (c *Client) Fiber() fiber.Handler {
	meter := c.meter.Meter(instrumentationName + "/fiber")
	duration, _ := meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."),
		metric.WithExplicitBucketBoundaries(httpServerRequestDurationBucketsSeconds...),
	)
	active, _ := meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Number of active HTTP server requests."),
	)
	tracer := c.tracer.Tracer(instrumentationName + "/fiber")

	return func(fc fiber.Ctx) (returnErr error) {
		start := time.Now()
		method := strings.Clone(fc.Method())
		path := strings.Clone(fc.Path())
		parent := c.propagator.Extract(fc.Context(), fiberHeaderCarrier{ctx: fc})
		spanAttrs := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("url.scheme", requestScheme(fc)),
			attribute.String("url.path", path),
			attribute.String("server.address", strings.Clone(fc.Hostname())),
			attribute.String(
				"network.protocol.version",
				strings.TrimPrefix(string(fc.Request().Header.Protocol()), "HTTP/"),
			),
		}
		ctx, span := tracer.Start(parent, method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(spanAttrs...),
		)
		fc.SetContext(ctx)
		activeAttrs := metric.WithAttributes(attribute.String("http.request.method", method))
		active.Add(ctx, 1, activeAttrs)

		defer func() {
			route := unmatchedHTTPRoute
			if fc.Matched() && fc.Route() != nil && fc.Route().Path != "" {
				// Registered route paths are app-owned, but clone defensively.
				route = strings.Clone(fc.Route().Path)
			}
			panicked := false
			var spanErrDesc string
			if recovered := recover(); recovered != nil {
				panicked = true
				panicErr := fmt.Errorf("panic: %v", recovered)
				spanErrDesc = panicErr.Error()
				span.RecordError(panicErr, trace.WithStackTrace(true))
				span.SetStatus(codes.Error, spanErrDesc)
				c.logger.ErrorContext(ctx, "panic recovered",
					"error", panicErr,
					"stack", string(debug.Stack()),
					"http.request.method", method,
					"url.path", path,
				)
				if handlerErr := fc.App().ErrorHandler(fc, fiber.ErrInternalServerError); handlerErr != nil {
					_ = fc.SendStatus(fiber.StatusInternalServerError)
				}
				status := fc.Response().StatusCode()
				if status < http.StatusInternalServerError {
					status = fiber.StatusInternalServerError
				}
				c.captureException(ctx, fmt.Errorf("%v", recovered), errorCaptureOptions{
					exceptionType: "panic",
					frames:        callerFrames(4),
					tags:          fiberErrorTags(method, route, status),
					url:           fiberRequestURL(fc),
				})
				returnErr = nil
			}
			handledErr := returnErr
			if handledErr != nil {
				if handlerErr := fc.App().ErrorHandler(fc, handledErr); handlerErr != nil {
					_ = fc.SendStatus(fiber.StatusInternalServerError)
					handledErr = errors.Join(handledErr, handlerErr)
				}
				returnErr = nil
			}
			status := fc.Response().StatusCode()
			if panicked && status < http.StatusInternalServerError {
				status = fiber.StatusInternalServerError
			}
			if handledErr != nil && status < http.StatusBadRequest {
				status = fiber.StatusInternalServerError
				var fiberErr *fiber.Error
				if errors.As(handledErr, &fiberErr) && fiberErr.Code >= http.StatusBadRequest {
					status = fiberErr.Code
				}
			}
			if handledErr != nil && status >= http.StatusInternalServerError && !panicked {
				spanErrDesc = handledErr.Error()
				span.RecordError(handledErr)
				span.SetStatus(codes.Error, spanErrDesc)
				c.captureException(ctx, handledErr, errorCaptureOptions{
					omitStack: true,
					tags:      fiberErrorTags(method, route, status),
					url:       fiberRequestURL(fc),
				})
			}
			span.SetName(method + " " + route)
			span.SetAttributes(
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", status),
			)
			if status >= http.StatusInternalServerError && spanErrDesc == "" {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
				attribute.String("http.request.method", method),
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", status),
			))
			active.Add(ctx, -1, activeAttrs)
			span.End()
		}()

		returnErr = fc.Next()
		return returnErr
	}
}

func fiberErrorTags(method, route string, status int) map[string]string {
	return map[string]string{
		"http.request.method":       method,
		"http.route":                route,
		"http.response.status_code": fmt.Sprint(status),
	}
}

// fiberRequestURL builds a fresh string by concatenation, so it never aliases the
// request buffers it reads from.
func fiberRequestURL(c fiber.Ctx) string {
	return requestScheme(c) + "://" + c.Hostname() + c.Path()
}

func requestScheme(c fiber.Ctx) string {
	// Fiber v3 Protocol() is the HTTP version ("HTTP/1.1"); Scheme() is http or https.
	if c.Scheme() == "https" {
		return "https"
	}
	if forwarded := c.Get(fiber.HeaderXForwardedProto); forwarded != "" {
		return strings.Clone(forwarded)
	}
	return "http"
}

type fiberHeaderCarrier struct{ ctx fiber.Ctx }

// Get copies the header: propagators keep parsed values (for example
// tracestate members) in the span context, which is exported after the request.
func (c fiberHeaderCarrier) Get(key string) string { return strings.Clone(c.ctx.Get(key)) }

func (c fiberHeaderCarrier) Set(key, value string) { c.ctx.Set(key, value) }

func (c fiberHeaderCarrier) Keys() []string {
	keys := make([]string, 0, c.ctx.Request().Header.Len())
	for key := range c.ctx.Request().Header.All() {
		keys = append(keys, string(key))
	}
	return keys
}

var _ propagation.TextMapCarrier = fiberHeaderCarrier{}
