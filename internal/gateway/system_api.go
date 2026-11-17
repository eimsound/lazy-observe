package gateway

import (
	"net/http"
	"runtime"

	"github.com/gofiber/fiber/v2"
)

// systemInfo exposes only a process-scoped opaque identity and the effective
// Go CPU budget. Performance and HA gates use it to reject URL aliases that
// resolve to the same gateway and to verify equal per-replica resources; it
// deliberately excludes hostnames, storage endpoints, build paths, and keys.
func (g *Gateway) systemInfo(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"instance_id": g.instanceID, "gomaxprocs": runtime.GOMAXPROCS(0)})
}

func (g *Gateway) errorInboxDiagnostics(c *fiber.Ctx) error {
	if g.errorTracker == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "error Inbox unavailable"})
	}
	diagnostics, err := g.errorTracker.Diagnostics(c.UserContext())
	if err != nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "error Inbox diagnostics unavailable"})
	}
	return c.JSON(diagnostics)
}
