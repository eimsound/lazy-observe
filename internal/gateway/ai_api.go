package gateway

import (
	"errors"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/gofiber/fiber/v3"
)

func (g *Gateway) aiStatus(c fiber.Ctx) error {
	state := g.aiState()
	return c.JSON(fiber.Map{
		"enabled":  state.enabled,
		"provider": state.provider,
		"model":    state.model,
	})
}

func (g *Gateway) aiTest(c fiber.Ctx) error {
	if g.ai != nil && g.ai.store != nil {
		g.ai.refreshFromStore(c.Context())
	}
	state := g.aiState()
	if !state.enabled || state.client == nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":   "AI integration is disabled",
			"message": "Enable and configure an AI provider in the admin AI settings.",
		})
	}
	started := time.Now()
	response, err := state.client.Chat(c.Context(), ai.ChatRequest{
		System:      "You are a connectivity check for lazy-observe.",
		Messages:    []ai.Message{{Role: "user", Content: "Reply with exactly: pong"}},
		Temperature: 0,
	})
	latency := time.Since(started)
	if err != nil {
		kind := ai.ErrorProvider
		var typed *ai.Error
		if errors.As(err, &typed) {
			kind = typed.Kind
		}
		// Do not reflect provider error text: some compatible endpoints echo credentials.
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "AI connectivity test failed",
			"kind":  kind,
		})
	}
	model := response.Model
	if model == "" {
		model = state.model
	}
	return c.JSON(fiber.Map{
		"latency_ms": latency.Milliseconds(),
		"model":      model,
		"echo":       firstRunes(response.Content, 100),
	})
}

func firstRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
