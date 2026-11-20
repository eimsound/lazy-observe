package gateway

import (
	"strconv"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

func (g *Gateway) listNotifications(c fiber.Ctx) error {
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
	items, err := store.ListNotificationOutbox(c.Context(), options)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(items)
}

func (g *Gateway) requeueNotification(c fiber.Ctx) error {
	store := g.notifications
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "notification history unavailable"})
	}
	proj := currentProject(c)
	audit := auditEvent{
		Action:      "notification.requeue",
		TargetType:  "notification_outbox",
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "requeue_failed"
		}
		g.auditManagement(c, audit)
	}()
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	audit.TargetID = int(id)
	updated, err := store.RequeueNotification(c.Context(), id, proj.ID)
	if err != nil {
		audit.FailureReason = alertFailureReason(err, "requeue_failed")
		return apiError(c, err)
	}
	if !updated {
		audit.FailureReason = "not_found"
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	audit.Success = true
	return c.SendStatus(fiber.StatusNoContent)
}
