package gateway

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const managementAuditEvent = "management_audit"

type auditEvent struct {
	Action        string
	Actor         meta.UserInfo
	ActorUsername string
	TargetType    string
	TargetID      int
	TargetName    string
	TargetRole    string
	ProjectID     int
	ProjectSlug   string
	ChangedFields []string
	Success       bool
	FailureReason string
}

// owned copies the strings callers may take from the request. Actor comes from
// the authenticated user loaded from the metadata store and already owns its
// strings.
func (event auditEvent) owned() auditEvent {
	event.Action = strings.Clone(event.Action)
	event.ActorUsername = strings.Clone(event.ActorUsername)
	event.TargetType = strings.Clone(event.TargetType)
	event.TargetName = strings.Clone(event.TargetName)
	event.TargetRole = strings.Clone(event.TargetRole)
	event.ProjectSlug = strings.Clone(event.ProjectSlug)
	event.FailureReason = strings.Clone(event.FailureReason)
	if event.ChangedFields != nil {
		fields := make([]string, len(event.ChangedFields))
		for i, field := range event.ChangedFields {
			fields[i] = strings.Clone(field)
		}
		event.ChangedFields = fields
	}
	return event
}

func (g *Gateway) auditManagementFacts(c fiber.Ctx, facts management.AuditFacts) {
	g.auditManagement(c, auditEvent{
		Action:        facts.Action,
		TargetType:    facts.TargetType,
		TargetID:      facts.TargetID,
		TargetName:    facts.TargetName,
		TargetRole:    facts.TargetRole,
		ProjectID:     facts.ProjectID,
		ProjectSlug:   facts.ProjectSlug,
		ChangedFields: facts.ChangedFields,
		Success:       facts.Success,
		FailureReason: facts.FailureReason,
	})
}

func (g *Gateway) auditAccountFacts(c fiber.Ctx, facts account.AuditFacts) {
	g.auditManagement(c, auditEvent{
		Action:        facts.Action,
		ActorUsername: facts.ActorUsername,
		TargetType:    facts.TargetType,
		TargetID:      facts.TargetID,
		TargetName:    facts.TargetName,
		TargetRole:    facts.TargetRole,
		ChangedFields: facts.ChangedFields,
		Success:       facts.Success,
		FailureReason: facts.FailureReason,
	})
}

// auditManagement emits a deliberately allow-listed event. Callers cannot pass
// arbitrary request bodies, credentials, token values, error messages, or
// provider configuration through this API.
func (g *Gateway) auditManagement(c fiber.Ctx, event auditEvent) {
	// g.logger can be the self-monitoring OpenTelemetry handler, which batches
	// records and exports them after this request returns. Request-derived
	// strings (form-encoded BodyParser fields, Method, Path) may borrow Fiber's
	// reusable request buffer, so the record must only hold owned copies.
	event = event.owned()
	actor := event.Actor
	if actor.ID == 0 && actor.Username == "" {
		actor, _ = c.Locals(userLocal).(meta.UserInfo)
	}
	if actor.Username == "" {
		actor.Username = event.ActorUsername
	}
	if actor.Username == "" {
		actor.Username = "anonymous"
	}

	attrs := []any{
		"event", managementAuditEvent,
		"action", event.Action,
		"actor_username", actor.Username,
		"target_type", event.TargetType,
		"success", event.Success,
		"http_method", strings.Clone(c.Method()),
		"http_path", strings.Clone(c.Path()),
	}
	if actor.ID > 0 {
		attrs = append(attrs, "actor_id", strconv.Itoa(actor.ID))
	}
	if actor.Role != "" {
		attrs = append(attrs, "actor_role", actor.Role)
	}
	if event.TargetID > 0 {
		attrs = append(attrs, "target_id", strconv.Itoa(event.TargetID))
	}
	if event.TargetName != "" {
		attrs = append(attrs, "target_name", event.TargetName)
	}
	if event.TargetRole != "" {
		attrs = append(attrs, "target_role", event.TargetRole)
	}
	if event.ProjectID > 0 {
		attrs = append(attrs, "project_id", strconv.Itoa(event.ProjectID))
	}
	if event.ProjectSlug != "" {
		attrs = append(attrs, "project_slug", event.ProjectSlug)
	}
	if len(event.ChangedFields) > 0 {
		attrs = append(attrs, "changed_fields", event.ChangedFields)
	}
	if event.FailureReason != "" {
		attrs = append(attrs, "failure_reason", event.FailureReason)
	}
	g.logger.Log(c.Context(), slog.LevelInfo, "management audit", attrs...)
	g.persistAudit(c, event, actor)
}

// persistAudit best-effort writes the audit event to the metadata store.
// Persistence failures are logged but never fail the surrounding request.
func (g *Gateway) persistAudit(c fiber.Ctx, event auditEvent, actor meta.UserInfo) {
	if g.store == nil {
		return
	}
	record := meta.AuditRecord{
		Action:        event.Action,
		ActorID:       actor.ID,
		ActorUsername: actor.Username,
		ActorRole:     actor.Role,
		TargetType:    event.TargetType,
		TargetID:      event.TargetID,
		TargetName:    event.TargetName,
		TargetRole:    event.TargetRole,
		ProjectID:     event.ProjectID,
		ProjectSlug:   event.ProjectSlug,
		ChangedFields: strings.Join(event.ChangedFields, ","),
		Success:       event.Success,
		FailureReason: event.FailureReason,
		HTTPMethod:    c.Method(),
		HTTPPath:      c.Path(),
	}
	if err := g.store.RecordAudit(c.Context(), record); err != nil {
		g.logger.Warn("audit persistence failed", "action", event.Action, "error", err)
	}
}
