package gateway

import (
	"strconv"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v2"
)

func (g *Gateway) listNotifications(c *fiber.Ctx) error {
	store := g.notifications
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "notification history unavailable"})
	}
	options := meta.NotificationOutboxListOptions{
		ProjectID: currentProject(c).ID,
		Status:    strings.TrimSpace(c.Query("status")),
	}
	if value := strings.TrimSpace(c.Query("channel_id")); value != "" {
		id, err := strconv.Atoi(value)
		if err != nil || id <= 0 {
			return badRequest(c)
		}
		options.ChannelID = id
	}
	if value := strings.TrimSpace(c.Query("before_id")); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return badRequest(c)
		}
		options.BeforeID = id
	}
	if value := strings.TrimSpace(c.Query("limit")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 0 {
			return badRequest(c)
		}
		options.Limit = limit
	}
	items, err := store.ListNotificationOutbox(c.UserContext(), options)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(items)
}

func (g *Gateway) requeueNotification(c *fiber.Ctx) error {
	store := g.notifications
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "notification history unavailable"})
	}
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		return badRequest(c)
	}
	updated, err := store.RequeueNotification(c.UserContext(), id, currentProject(c).ID)
	if err != nil {
		return apiError(c, err)
	}
	if !updated {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}
