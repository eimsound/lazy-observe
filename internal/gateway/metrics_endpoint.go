package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strconv"
	"strings"

	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	"github.com/gofiber/fiber/v3"
)

const surfaceLocal = "lazy-observe.http-surface"

// MetricsEndpointConfig controls exposition and authentication of GET /metrics.
type MetricsEndpointConfig struct {
	Enabled              bool
	BearerToken          string
	AllowUnauthenticated bool
}

type metricsEndpointState struct {
	enabled              bool
	requireBearerToken   bool
	hasBearerToken       bool
	bearerTokenHash      [32]byte
	allowUnauthenticated bool
}

func newMetricsEndpointState(cfg MetricsEndpointConfig) metricsEndpointState {
	state := metricsEndpointState{
		enabled:              cfg.Enabled,
		allowUnauthenticated: cfg.AllowUnauthenticated,
	}
	if cfg.BearerToken != "" {
		// Fail-closed: any non-empty bearer token input (including whitespace-only
		// values from callers bypassing Config.Validate) disables unauthenticated
		// access and enforces the bearer-token auth mode.
		state.allowUnauthenticated = false
		state.requireBearerToken = true
		if token := strings.TrimSpace(cfg.BearerToken); token != "" {
			state.hasBearerToken = true
			state.bearerTokenHash = sha256.Sum256([]byte(token))
		}
	}
	return state
}

func (s metricsEndpointState) routePolicy() routePolicy {
	if s.requireBearerToken {
		return routePolicy{auth: authMetricsScrapeToken}
	}
	if s.allowUnauthenticated {
		return routePolicy{auth: authPublic}
	}
	return routePolicy{auth: authUser, role: roleAdmin}
}

func (g *Gateway) metricsScrapeTokenMiddleware(c fiber.Ctx) error {
	if !g.metricsEndpoint.hasBearerToken {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	token := bearerCredential(c)
	if token == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(sum[:], g.metricsEndpoint.bearerTokenHash[:]) != 1 {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return c.Next()
}

func (g *Gateway) prometheusMetrics(c fiber.Ctx) error {
	var buf bytes.Buffer
	if err := g.selfMetrics.WritePrometheus(&buf); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to render metrics")
	}
	c.Set(fiber.HeaderContentType, selfmetrics.ContentType)
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(fiber.StatusOK).Send(buf.Bytes())
}

func (g *Gateway) httpMetricsMiddleware(c fiber.Ctx) error {
	if g.selfMetrics == nil {
		return c.Next()
	}
	start := g.now()
	err := c.Next()
	elapsed := g.now().Sub(start)

	surface, ok := c.Locals(surfaceLocal).(selfmetrics.Surface)
	if !ok {
		surface = fallbackSurfaceForPath(c.Path())
	}
	statusCode := effectiveHTTPStatus(c, err)
	g.selfMetrics.ObserveHTTP(surface, statusCode, elapsed)
	return err
}

func (g *Gateway) authenticateAndLimitIngest(c fiber.Ctx, policy routePolicy) error {
	project, ok, err := g.auth.authenticateIngest(c)
	if !ok {
		if g.selfMetrics != nil {
			g.selfMetrics.ObserveIngest(
				selfmetrics.ProtocolHTTP,
				policy.ingestSignal,
				ingestOutcomeFromHTTPStatus(effectiveHTTPStatus(c, err)),
			)
		}
		return err
	}
	c.Locals(projectLocal, project)

	if g.ingestLimiter != nil && g.ingestLimiter.Enabled() {
		payloadBytes := ingestPayloadBytes(c)
		decision := g.ingestLimiter.Allow(project.ID, payloadBytes)
		if !decision.Allowed {
			retryAfterSecs := ingestlimit.RetryAfterSeconds(decision.RetryAfter)
			c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(retryAfterSecs, 10))
			if g.selfMetrics != nil {
				g.selfMetrics.ObserveIngest(
					selfmetrics.ProtocolHTTP,
					policy.ingestSignal,
					selfmetrics.OutcomeRateLimited,
				)
			}
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"code":  "ingest_rate_limited",
				"error": "project ingest rate limit exceeded",
			})
		}
	}

	completed := false
	defer func() {
		if !completed && g.selfMetrics != nil {
			g.selfMetrics.ObserveIngest(
				selfmetrics.ProtocolHTTP,
				policy.ingestSignal,
				selfmetrics.OutcomeUpstreamError,
			)
		}
	}()

	nextErr := c.Next()
	completed = true
	if g.selfMetrics != nil {
		g.selfMetrics.ObserveIngest(
			selfmetrics.ProtocolHTTP,
			policy.ingestSignal,
			ingestOutcomeFromHTTPStatus(effectiveHTTPStatus(c, nextErr)),
		)
	}
	return nextErr
}

func ingestPayloadBytes(c fiber.Ctx) int64 {
	if decodedSize, ok := c.Locals(decodedBodySizeLocal).(int64); ok && decodedSize >= 0 {
		return decodedSize
	}
	return int64(len(c.Request().Body()))
}

func effectiveHTTPStatus(c fiber.Ctx, err error) int {
	if err != nil && !errors.Is(err, errResponseWritten) {
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) && fiberErr.Code >= 400 && fiberErr.Code <= 599 {
			return fiberErr.Code
		}
		return fiber.StatusInternalServerError
	}
	code := c.Response().StatusCode()
	if code <= 0 {
		return fiber.StatusOK
	}
	return code
}

func ingestOutcomeFromHTTPStatus(statusCode int) selfmetrics.Outcome {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return selfmetrics.OutcomeAccepted
	case statusCode == fiber.StatusTooManyRequests:
		return selfmetrics.OutcomeRateLimited
	case statusCode >= 400 && statusCode < 500:
		return selfmetrics.OutcomeRejected
	default:
		return selfmetrics.OutcomeUpstreamError
	}
}

func isAdminAPIPath(requestPath string) bool {
	return strings.HasPrefix(requestPath, "/api/users") ||
		strings.HasPrefix(requestPath, "/api/projects") ||
		strings.HasPrefix(requestPath, "/api/channels") ||
		strings.HasPrefix(requestPath, "/api/audit") ||
		strings.HasPrefix(requestPath, "/api/ai/settings") ||
		strings.HasPrefix(requestPath, "/api/ai/test") ||
		strings.HasPrefix(requestPath, "/api/system/error-inbox")
}

func surfaceForRoute(routePath string, policy routePolicy) selfmetrics.Surface {
	if routePath == "/metrics" {
		return selfmetrics.SurfaceMetrics
	}
	switch policy.auth {
	case authProjectToken, authUploadToken:
		return selfmetrics.SurfaceIngest
	case authPublic:
		return selfmetrics.SurfacePublic
	case authMetricsScrapeToken:
		return selfmetrics.SurfaceMetrics
	}
	if policy.role == roleAdmin || isAdminAPIPath(routePath) {
		return selfmetrics.SurfaceAdmin
	}
	return selfmetrics.SurfaceQuery
}

func fallbackSurfaceForPath(requestPath string) selfmetrics.Surface {
	switch {
	case requestPath == "/metrics":
		return selfmetrics.SurfaceMetrics
	case requestPath == "/api/auth/login":
		return selfmetrics.SurfacePublic
	case strings.HasPrefix(requestPath, "/v1/"),
		requestPath == "/api/v1/write",
		strings.HasPrefix(requestPath, "/api/events/"),
		requestPath == "/api/replay/segments",
		requestPath == "/api/profiles":
		return selfmetrics.SurfaceIngest
	case isAdminAPIPath(requestPath):
		return selfmetrics.SurfaceAdmin
	case strings.HasPrefix(requestPath, "/api/"),
		requestPath == "/mcp",
		strings.HasPrefix(requestPath, "/mcp/"):
		return selfmetrics.SurfaceQuery
	default:
		return selfmetrics.SurfacePublic
	}
}
