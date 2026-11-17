package lazyobserve

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var httpServerRequestDurationBucketsSeconds = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

// Fiber returns explicit Fiber v2 HTTP server instrumentation.
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

	return func(fc *fiber.Ctx) (returnErr error) {
		start := time.Now()
		method := fc.Method()
		path := fc.Path()
		parent := c.propagator.Extract(fc.UserContext(), fiberHeaderCarrier{ctx: fc})
		spanAttrs := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("url.scheme", requestScheme(fc)),
			attribute.String("url.path", path),
			attribute.String("server.address", fc.Hostname()),
			attribute.String(
				"network.protocol.version",
				strings.TrimPrefix(string(fc.Request().Header.Protocol()), "HTTP/"),
			),
		}
		ctx, span := tracer.Start(parent, method+" "+path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(spanAttrs...),
		)
		fc.SetUserContext(ctx)
		activeAttrs := metric.WithAttributes(attribute.String("http.request.method", method))
		active.Add(ctx, 1, activeAttrs)

		defer func() {
			status := fc.Response().StatusCode()
			route := path
			if fc.Route() != nil && fc.Route().Path != "" {
				route = fc.Route().Path
			}
			panicked := false
			if recovered := recover(); recovered != nil {
				panicked = true
				status = fiber.StatusInternalServerError
				panicErr := fmt.Errorf("panic: %v", recovered)
				span.RecordError(panicErr, trace.WithStackTrace(true))
				span.SetStatus(codes.Error, panicErr.Error())
				c.logger.ErrorContext(ctx, "panic recovered",
					"error", panicErr,
					"stack", string(debug.Stack()),
					"http.request.method", method,
					"url.path", path,
				)
				c.captureException(ctx, fmt.Errorf("%v", recovered), errorCaptureOptions{
					exceptionType: "panic",
					frames:        callerFrames(4),
					tags:          fiberErrorTags(method, route, status),
					url:           fiberRequestURL(fc),
				})
				returnErr = fc.Status(status).SendString(http.StatusText(status))
			}
			if returnErr != nil {
				span.RecordError(returnErr)
				span.SetStatus(codes.Error, returnErr.Error())
				if status < 400 {
					status = fiber.StatusInternalServerError
					var fiberErr *fiber.Error
					if errors.As(returnErr, &fiberErr) {
						status = fiberErr.Code
					}
				}
			}
			if returnErr != nil && status >= http.StatusInternalServerError && !panicked {
				c.captureException(ctx, returnErr, errorCaptureOptions{
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
			if status >= 500 {
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

func fiberRequestURL(c *fiber.Ctx) string {
	return requestScheme(c) + "://" + c.Hostname() + c.Path()
}

func requestScheme(c *fiber.Ctx) string {
	if c.Protocol() == "https" {
		return "https"
	}
	if forwarded := c.Get(fiber.HeaderXForwardedProto); forwarded != "" {
		return forwarded
	}
	return "http"
}

type fiberHeaderCarrier struct{ ctx *fiber.Ctx }

func (c fiberHeaderCarrier) Get(key string) string { return c.ctx.Get(key) }

func (c fiberHeaderCarrier) Set(key, value string) { c.ctx.Set(key, value) }

func (c fiberHeaderCarrier) Keys() []string {
	keys := make([]string, 0, c.ctx.Request().Header.Len())
	c.ctx.Request().Header.VisitAll(func(key, _ []byte) { keys = append(keys, string(key)) })
	return keys
}

var _ propagation.TextMapCarrier = fiberHeaderCarrier{}
