package gateway

import (
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
)

const (
	maxTraceServiceNameLength = 256
	maxTraceIDLength          = 32
)

func (g *Gateway) traceOperations(c fiber.Ctx) error {
	svc, err := url.PathUnescape(c.Params("svc"))
	if err != nil || !validateTraceServiceName(svc) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid service name")
	}
	return g.tenantProxy(
		g.storage.TracesBase(),
		"/select/jaeger/api/services/"+url.PathEscape(svc)+"/operations",
	)(c)
}

func (g *Gateway) traceByID(c fiber.Ctx) error {
	traceID, err := url.PathUnescape(c.Params("traceID"))
	if err != nil || !validateTraceID(traceID) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid trace ID")
	}
	return g.tenantProxy(
		g.storage.TracesBase(),
		"/select/jaeger/api/traces/"+url.PathEscape(traceID),
	)(c)
}

func (g *Gateway) traceSearch(c fiber.Ctx) error {
	p := currentProject(c)
	if p.ID <= 0 {
		return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
	}
	return g.proxy(c, g.storage.TracesBase(), "/select/jaeger/api/traces", proxyOptions{
		projectID:     p.ID,
		requireTenant: true,
		allowedParams: map[string]bool{
			"service":     true,
			"operation":   true,
			"start":       true,
			"end":         true,
			"limit":       true,
			"minDuration": true,
			"maxDuration": true,
			"lookback":    true,
			"tags":        true,
		},
	})
}

func validateTraceServiceName(svc string) bool {
	if svc == "" || len(svc) > maxTraceServiceNameLength || strings.TrimSpace(svc) == "" {
		return false
	}
	for i := 0; i < len(svc); i++ {
		ch := svc[i]
		if ch < 0x20 || ch == 0x7f || ch == '\\' {
			return false
		}
	}
	for _, seg := range strings.Split(svc, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validateTraceID(traceID string) bool {
	if len(traceID) == 0 || len(traceID) > maxTraceIDLength {
		return false
	}
	for i := 0; i < len(traceID); i++ {
		ch := traceID[i]
		isDigit := ch >= '0' && ch <= '9'
		isLowerHex := ch >= 'a' && ch <= 'f'
		isUpperHex := ch >= 'A' && ch <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}
	return true
}
