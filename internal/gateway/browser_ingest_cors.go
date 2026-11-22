package gateway

import (
	"github.com/gofiber/fiber/v3"
)

const browserIngestAllowedHeaders = "Authorization, Content-Type, Content-Encoding, " +
	"X-LO-Replay-Session, X-LO-Replay-Seq, X-LO-Replay-Compression, X-LO-Replay-Error"

// browserIngestCORS permits token-authenticated browser SDK writes without
// exposing the platform's management and query APIs cross-origin.
func browserIngestCORS(c fiber.Ctx) error {
	if c.Get(fiber.HeaderOrigin) != "" {
		c.Set(fiber.HeaderAccessControlAllowOrigin, "*")
		c.Set(fiber.HeaderAccessControlAllowMethods, fiber.MethodPost)
		c.Set(fiber.HeaderAccessControlAllowHeaders, browserIngestAllowedHeaders)
		c.Set(fiber.HeaderAccessControlExposeHeaders, "Retry-After")
		c.Set(fiber.HeaderAccessControlMaxAge, "600")
	}
	if c.Method() == fiber.MethodOptions {
		return c.SendStatus(fiber.StatusNoContent)
	}
	return c.Next()
}
