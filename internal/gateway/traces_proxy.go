package gateway

import (
	"net/url"

	"github.com/gofiber/fiber/v3"
)

var traceSearchAllowedParams = map[string]bool{
	"service":     true,
	"operation":   true,
	"start":       true,
	"end":         true,
	"limit":       true,
	"minDuration": true,
	"maxDuration": true,
	"lookback":    true,
	"tags":        true,
}

func (g *Gateway) traceOperations(c fiber.Ctx) error {
	svc, err := unescapedPathParam(c, "svc", validateTraceServiceName)
	if err != nil {
		return err
	}
	return g.tenantProxy(
		g.storage.TracesQueryBase(),
		"/select/jaeger/api/services/"+url.PathEscape(svc)+"/operations",
	)(c)
}

func (g *Gateway) traceByID(c fiber.Ctx) error {
	traceID, err := unescapedPathParam(c, "traceID", validateTraceID)
	if err != nil {
		return err
	}
	return g.tenantProxy(
		g.storage.TracesQueryBase(),
		"/select/jaeger/api/traces/"+url.PathEscape(traceID),
	)(c)
}

func (g *Gateway) traceSearch(c fiber.Ctx) error {
	p := currentProject(c)
	return g.executeProxy(c, proxySpec{
		kind:          proxyKindUnary,
		base:          g.storage.TracesQueryBase(),
		upstreamPath:  "/select/jaeger/api/traces",
		projectID:     p.ID,
		requireTenant: true,
		allowedParams: traceSearchAllowedParams,
	})
}
