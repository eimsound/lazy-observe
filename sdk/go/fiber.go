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

// FiberOptions customizes how FiberWithOptions instruments requests, classifies
// returned handler errors, handles panics, and sanitizes request paths.
type FiberOptions struct {
	// StatusFromError maps a returned handler error to its HTTP status code
	// without invoking Fiber's ErrorHandler inside the middleware. When set,
	// returned errors propagate to Fiber's outer error-handling pipeline
	// untouched. Returning <= 0 falls back to Fiber's default resolution
	// (*fiber.Error.Code, an already-set >=400 response status, or 500).
	StatusFromError func(error) int

	// RepanicOnPanic re-panics the recovered value after finishing span,
	// metric, structured log, and error-event capture. Use this when an outer
	// recovery middleware owns the HTTP response for panics.
	RepanicOnPanic bool

	// RedactPath transforms the raw request path before it is stored in
	// url.path span attributes, panic logs, and error-event URLs. When nil,
	// the raw cloned request path is recorded as-is.
	RedactPath func(string) string
}

// Fiber returns explicit Fiber v3 HTTP server instrumentation with default
// options. Fiber v2 is not supported: the handler takes the v3 fiber.Ctx
// interface.
//
// Without fiber.Config.Immutable, Fiber's request accessors (Method, Path,
// Hostname, Get, Route fallbacks, ...) return strings that borrow pooled request
// buffers which the next request overwrites. Every such value that outlives the
// handler (span attributes exported in batches, metric attributes used as
// aggregation keys, error-event tags, log records, propagated trace state) is
// copied with strings.Clone at the point it is read, so the middleware is
// correct regardless of the application's Immutable setting.
func (c *Client) Fiber() fiber.Handler {
	return c.FiberWithOptions(FiberOptions{})
}

// FiberWithOptions returns explicit Fiber v3 HTTP server instrumentation
// configured by opts.
func (c *Client) FiberWithOptions(opts FiberOptions) fiber.Handler {
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
		if opts.RedactPath != nil {
			path = strings.Clone(opts.RedactPath(path))
		}
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
			var repanicValue any
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
				status := fiber.StatusInternalServerError
				if opts.RepanicOnPanic {
					repanicValue = recovered
				} else {
					if handlerErr := fc.App().ErrorHandler(fc, fiber.ErrInternalServerError); handlerErr != nil {
						_ = fc.SendStatus(fiber.StatusInternalServerError)
					}
					if respStatus := fc.Response().StatusCode(); respStatus >= http.StatusInternalServerError {
						status = respStatus
					}
					returnErr = nil
				}
				c.captureException(ctx, fmt.Errorf("%v", recovered), errorCaptureOptions{
					exceptionType: "panic",
					frames:        callerFrames(4),
					tags:          fiberErrorTags(method, route, status),
					url:           fiberRequestURL(fc, path),
				})
			}
			handledErr := returnErr
			var status int
			if handledErr != nil && !panicked {
				if opts.StatusFromError != nil {
					status = opts.StatusFromError(handledErr)
					if status <= 0 {
						status = defaultFiberErrorStatus(handledErr, fc.Response().StatusCode())
					}
				} else {
					if handlerErr := fc.App().ErrorHandler(fc, handledErr); handlerErr != nil {
						_ = fc.SendStatus(fiber.StatusInternalServerError)
						handledErr = errors.Join(handledErr, handlerErr)
					}
					returnErr = nil
					status = fc.Response().StatusCode()
					if status < http.StatusBadRequest {
						status = defaultFiberErrorStatus(handledErr, status)
					}
				}
			} else {
				status = fc.Response().StatusCode()
			}
			if panicked && status < http.StatusInternalServerError {
				status = fiber.StatusInternalServerError
			}
			if handledErr != nil && status >= http.StatusInternalServerError && !panicked {
				spanErrDesc = handledErr.Error()
				span.RecordError(handledErr)
				span.SetStatus(codes.Error, spanErrDesc)
				c.captureException(ctx, handledErr, errorCaptureOptions{
					omitStack: true,
					tags:      fiberErrorTags(method, route, status),
					url:       fiberRequestURL(fc, path),
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
			if panicked && opts.RepanicOnPanic {
				panic(repanicValue)
			}
		}()

		returnErr = fc.Next()
		return returnErr
	}
}

func defaultFiberErrorStatus(err error, responseStatus int) int {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) && fiberErr.Code >= http.StatusBadRequest {
		return fiberErr.Code
	}
	if responseStatus >= http.StatusBadRequest {
		return responseStatus
	}
	return fiber.StatusInternalServerError
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
func fiberRequestURL(c fiber.Ctx, path string) string {
	return requestScheme(c) + "://" + c.Hostname() + path
}

func requestScheme(c fiber.Ctx) string {
	// Fiber v3 Protocol() is the HTTP version ("HTTP/1.1"); Scheme() returns
	// "https" for TLS connections and honors forwarded scheme headers only when
	// the app explicitly trusts the peer via fiber.Config{TrustProxy: true,
	// TrustProxyConfig: ...}.
	if scheme := c.Scheme(); scheme != "" {
		return strings.Clone(scheme)
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
