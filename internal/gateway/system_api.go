package gateway

import (
	"net/http"
	"runtime"

	"github.com/gofiber/fiber/v3"
)

type webSelfMonitoringPayload struct {
	Enabled       bool   `json:"enabled"`
	ProjectSlug   string `json:"project_slug,omitempty"`
	Token         string `json:"token,omitempty"`
	ServiceName   string `json:"service_name,omitempty"`
	Release       string `json:"release,omitempty"`
	ReplayEnabled bool   `json:"replay_enabled,omitempty"`
}

func (g *Gateway) webSelfMonitoringPayload() webSelfMonitoringPayload {
	if !g.webSelfMonitoring.active() {
		return webSelfMonitoringPayload{Enabled: false}
	}
	cfg := g.webSelfMonitoring.normalized()
	return webSelfMonitoringPayload{
		Enabled:       true,
		ProjectSlug:   cfg.ProjectSlug,
		Token:         cfg.Token,
		ServiceName:   cfg.ServiceName,
		Release:       cfg.Release,
		ReplayEnabled: cfg.ReplayEnabled,
	}
}

func (g *Gateway) webSelfMonitoringConfig(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(g.webSelfMonitoringPayload())
}

// systemInfo exposes only a process-scoped opaque identity and the effective
// Go CPU budget. Performance and HA gates use it to reject URL aliases that
// resolve to the same gateway and to verify equal per-replica resources; it
// deliberately excludes hostnames, storage endpoints, build paths, and keys.
func (g *Gateway) systemInfo(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"instance_id": g.instanceID, "gomaxprocs": runtime.GOMAXPROCS(0)})
}

func (g *Gateway) errorInboxDiagnostics(c fiber.Ctx) error {
	if g.errorTracker == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "error Inbox unavailable"})
	}
	diagnostics, err := g.errorTracker.Diagnostics(c.Context())
	if err != nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "error Inbox diagnostics unavailable"})
	}
	return c.JSON(diagnostics)
}
