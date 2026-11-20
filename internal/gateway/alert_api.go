package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/gofiber/fiber/v3"
)

type alertRuleResponse struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Signal           string            `json:"signal"`
	EvaluationEngine string            `json:"evaluation_engine"`
	Expr             string            `json:"expr"`
	For              string            `json:"for"`
	Severity         string            `json:"severity"`
	Labels           map[string]string `json:"labels"`
	Annotations      map[string]string `json:"annotations"`
	Enabled          bool              `json:"enabled"`
	ChannelIDs       []string          `json:"channel_ids"`
	LastError        string            `json:"last_error,omitempty"`
	CreatedAt        string            `json:"created_at"`
	UpdatedAt        string            `json:"updated_at"`
}

type apiID int

func (id *apiID) UnmarshalJSON(data []byte) error {
	var value int
	if len(data) > 0 && data[0] == '"' {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("invalid id %q", raw)
		}
		value = parsed
	} else if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("invalid id: %w", err)
	}
	if value <= 0 {
		return fmt.Errorf("invalid id %d", value)
	}
	*id = apiID(value)
	return nil
}

type alertRuleRequest struct {
	Name             string            `json:"name"`
	Signal           string            `json:"signal"`
	EvaluationEngine string            `json:"evaluation_engine"`
	Expr             string            `json:"expr"`
	For              string            `json:"for"`
	Severity         string            `json:"severity"`
	Labels           map[string]string `json:"labels"`
	Annotations      map[string]string `json:"annotations"`
	Enabled          *bool             `json:"enabled"`
	ChannelIDs       []apiID           `json:"channel_ids"`
}

func (request alertRuleRequest) input(project string) alert.RuleInput {
	var channelIDs []int
	if request.ChannelIDs != nil {
		channelIDs = make([]int, 0, len(request.ChannelIDs))
		for _, id := range request.ChannelIDs {
			channelIDs = append(channelIDs, int(id))
		}
	}
	return alert.RuleInput{
		Project:          project,
		Name:             request.Name,
		Signal:           request.Signal,
		EvaluationEngine: request.EvaluationEngine,
		Expr:             request.Expr,
		For:              request.For,
		Severity:         request.Severity,
		Labels:           request.Labels,
		Annotations:      request.Annotations,
		Enabled:          request.Enabled,
		ChannelIDs:       channelIDs,
	}
}

type notificationChannelResponse struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	Config    map[string]any `json:"config"`
	Enabled   bool           `json:"enabled"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// notificationChannelSummaryResponse is the minimal read-only projection exposed
// to non-admin members who need to bind alert rules or project alert configs to
// existing channels without seeing any channel config, URLs, hosts, or secrets.
type notificationChannelSummaryResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type silenceResponse struct {
	ID        string          `json:"id"`
	Matchers  []alert.Matcher `json:"matchers"`
	StartsAt  string          `json:"starts_at"`
	EndsAt    string          `json:"ends_at"`
	Comment   string          `json:"comment"`
	Creator   string          `json:"creator"`
	CreatedAt string          `json:"created_at"`
}

type alertEventResponse struct {
	ID          string            `json:"id"`
	Fingerprint string            `json:"fingerprint"`
	Rule        string            `json:"rule"`
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"starts_at"`
	EndsAt      *string           `json:"ends_at,omitempty"`
	Inhibited   bool              `json:"inhibited"`
	UpdatedAt   string            `json:"updated_at"`
}

type channelTestResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func apiTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func stringMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}

func anyMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func toAlertRuleResponse(value meta.AlertRuleView, lastError string) alertRuleResponse {
	numericChannelIDs := append([]int(nil), value.ChannelIDs...)
	sort.Ints(numericChannelIDs)
	channelIDs := make([]string, 0, len(numericChannelIDs))
	for _, channelID := range numericChannelIDs {
		channelIDs = append(channelIDs, strconv.Itoa(channelID))
	}
	return alertRuleResponse{
		ID: strconv.Itoa(
			value.ID,
		),
		Name:             value.Name,
		Signal:           value.Signal,
		EvaluationEngine: value.EvaluationEngine,
		Expr:             value.Expression,
		For:              value.For,
		Severity:         value.Severity,
		Labels:           stringMap(value.Labels),
		Annotations:      stringMap(value.Annotations),
		Enabled:          value.Enabled,
		ChannelIDs:       channelIDs,
		LastError:        lastError,
		CreatedAt:        apiTime(value.CreatedAt),
		UpdatedAt:        apiTime(value.UpdatedAt),
	}
}

func (g *Gateway) toAlertRuleResponses(values []meta.AlertRuleView) []alertRuleResponse {
	out := make([]alertRuleResponse, 0, len(values))
	for _, value := range values {
		var lastErr string
		if g.alerts != nil {
			lastErr = g.alerts.RuleLastError(value.ID)
		}
		out = append(out, toAlertRuleResponse(value, lastErr))
	}
	return out
}

func toNotificationChannelResponse(value meta.NotificationChannelView) notificationChannelResponse {
	return notificationChannelResponse{
		ID: strconv.Itoa(
			value.ID,
		),
		Type: value.Type,
		Name: value.Name,
		Config: anyMap(
			alert.RedactChannelConfig(value.Type, alert.NormalizeChannelConfig(value.Type, value.Config)),
		),
		Enabled:   value.Enabled,
		CreatedAt: apiTime(value.CreatedAt),
		UpdatedAt: apiTime(value.UpdatedAt),
	}
}

func toNotificationChannelResponses(values []meta.NotificationChannelView) []notificationChannelResponse {
	out := make([]notificationChannelResponse, 0, len(values))
	for _, value := range values {
		out = append(out, toNotificationChannelResponse(value))
	}
	return out
}

func toNotificationChannelSummaryResponses(
	values []meta.NotificationChannelView,
) []notificationChannelSummaryResponse {
	out := make([]notificationChannelSummaryResponse, 0, len(values))
	for _, value := range values {
		out = append(out, notificationChannelSummaryResponse{
			ID:      strconv.Itoa(value.ID),
			Type:    value.Type,
			Name:    value.Name,
			Enabled: value.Enabled,
		})
	}
	return out
}

func toSilenceResponse(value meta.SilenceView) silenceResponse {
	matchers := make([]alert.Matcher, 0, len(value.Matchers))
	for _, matcher := range value.Matchers {
		matchers = append(matchers, alert.Matcher{Name: matcher["name"], Op: matcher["op"], Value: matcher["value"]})
	}
	return silenceResponse{
		ID: strconv.Itoa(
			value.ID,
		),
		Matchers:  matchers,
		StartsAt:  apiTime(value.StartsAt),
		EndsAt:    apiTime(value.EndsAt),
		Comment:   value.Comment,
		Creator:   value.Creator,
		CreatedAt: apiTime(value.CreatedAt),
	}
}

func toSilenceResponses(values []meta.SilenceView) []silenceResponse {
	out := make([]silenceResponse, 0, len(values))
	for _, value := range values {
		out = append(out, toSilenceResponse(value))
	}
	return out
}

func toAlertEventResponse(value meta.AlertEventView) alertEventResponse {
	rule := value.RuleName
	if rule == "" && value.Labels != nil {
		rule = value.Labels["alertname"]
	}
	var endsAt *string
	if value.EndsAt != nil {
		formatted := apiTime(*value.EndsAt)
		endsAt = &formatted
	}
	return alertEventResponse{
		ID:          strconv.Itoa(value.ID),
		Fingerprint: value.Fingerprint,
		Rule:        rule,
		Status:      value.Status,
		Labels: stringMap(
			value.Labels,
		),
		Annotations: stringMap(value.Annotations),
		StartsAt:    apiTime(value.StartsAt),
		EndsAt:      endsAt,
		Inhibited:   value.Inhibited,
		UpdatedAt:   apiTime(value.UpdatedAt),
	}
}

func toAlertEventResponses(values []meta.AlertEventView) []alertEventResponse {
	out := make([]alertEventResponse, 0, len(values))
	for _, value := range values {
		out = append(out, toAlertEventResponse(value))
	}
	return out
}

func (g *Gateway) alertingUnavailable(c fiber.Ctx) bool {
	if g.alerts == nil {
		_ = c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "alerting unavailable"})
		return true
	}
	return false
}

func alertAPIError(c fiber.Ctx, err error) error {
	if errors.Is(err, alert.ErrInvalidInput) || errors.Is(err, alert.ErrInvalidChannelConfig) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": outbound.SanitizeError(err)})
	}
	return apiError(c, err)
}

func alertFailureReason(err error, fallback string) string {
	if errors.Is(err, alert.ErrInvalidInput) || errors.Is(err, alert.ErrInvalidChannelConfig) {
		return "invalid_input"
	}
	if meta.IsNotFound(err) {
		return "not_found"
	}
	return fallback
}

func (g *Gateway) listAlertRules(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	v, e := g.alerts.ListRules(c.Context(), proj.Slug)
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(g.toAlertRuleResponses(v))
}

func (g *Gateway) createAlertRule(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "alert_rule.create",
		TargetType:  "alert_rule",
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "create_failed"
		}
		g.auditManagement(c, audit)
	}()
	var request alertRuleRequest
	if c.Bind().Body(&request) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	audit.TargetName = strings.TrimSpace(request.Name)
	in := request.input(proj.Slug)
	v, e := g.alerts.CreateRule(c.Context(), in)
	if e != nil {
		audit.FailureReason = alertFailureReason(e, "create_failed")
		return alertAPIError(c, e)
	}
	audit.TargetID = v.ID
	audit.TargetName = v.Name
	audit.Success = true
	return c.Status(fiber.StatusCreated).JSON(toAlertRuleResponse(v, g.alerts.RuleLastError(v.ID)))
}

func (g *Gateway) updateAlertRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "alert_rule.update",
		TargetType:  "alert_rule",
		TargetID:    id,
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "update_failed"
		}
		g.auditManagement(c, audit)
	}()
	var request alertRuleRequest
	if c.Bind().Body(&request) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	audit.TargetName = strings.TrimSpace(request.Name)
	in := request.input(proj.Slug)
	v, e := g.alerts.UpdateRule(c.Context(), id, in)
	if e != nil {
		audit.FailureReason = alertFailureReason(e, "update_failed")
		return alertAPIError(c, e)
	}
	audit.TargetID = v.ID
	audit.TargetName = v.Name
	audit.Success = true
	return c.JSON(toAlertRuleResponse(v, g.alerts.RuleLastError(v.ID)))
}

func (g *Gateway) deleteAlertRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "alert_rule.delete",
		TargetType:  "alert_rule",
		TargetID:    id,
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "delete_failed"
		}
		g.auditManagement(c, audit)
	}()
	if e := g.alerts.DeleteRule(c.Context(), proj.Slug, id); e != nil {
		audit.FailureReason = alertFailureReason(e, "delete_failed")
		return apiError(c, e)
	}
	audit.Success = true
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listChannels(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	v, e := g.alerts.ListChannels(c.Context())
	if e != nil {
		return apiError(c, e)
	}
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	if u.Role != "admin" {
		return c.JSON(toNotificationChannelSummaryResponses(v))
	}
	return c.JSON(toNotificationChannelResponses(v))
}

func (g *Gateway) createChannel(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	audit := auditEvent{Action: "notification_channel.create", TargetType: "notification_channel"}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "create_failed"
		}
		g.auditManagement(c, audit)
	}()
	var in alert.ChannelInput
	if c.Bind().Body(&in) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	audit.TargetName = strings.TrimSpace(in.Name)
	v, e := g.alerts.CreateChannel(c.Context(), in)
	if e != nil {
		audit.FailureReason = alertFailureReason(e, "create_failed")
		return alertAPIError(c, e)
	}
	audit.TargetID = v.ID
	audit.TargetName = v.Name
	audit.Success = true
	return c.Status(fiber.StatusCreated).JSON(toNotificationChannelResponse(v))
}

func (g *Gateway) updateChannel(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	audit := auditEvent{Action: "notification_channel.update", TargetType: "notification_channel", TargetID: id}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "update_failed"
		}
		g.auditManagement(c, audit)
	}()
	var in alert.ChannelInput
	if c.Bind().Body(&in) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	audit.TargetName = strings.TrimSpace(in.Name)
	v, e := g.alerts.UpdateChannel(c.Context(), id, in)
	if e != nil {
		audit.FailureReason = alertFailureReason(e, "update_failed")
		return alertAPIError(c, e)
	}
	audit.TargetID = v.ID
	audit.TargetName = v.Name
	audit.Success = true
	return c.JSON(toNotificationChannelResponse(v))
}

func (g *Gateway) deleteChannel(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	audit := auditEvent{Action: "notification_channel.delete", TargetType: "notification_channel", TargetID: id}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "delete_failed"
		}
		g.auditManagement(c, audit)
	}()
	if e := g.alerts.DeleteChannel(c.Context(), id); e != nil {
		audit.FailureReason = alertFailureReason(e, "delete_failed")
		return apiError(c, e)
	}
	audit.Success = true
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) testChannel(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	audit := auditEvent{Action: "notification_channel.test", TargetType: "notification_channel", TargetID: id}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "test_failed"
		}
		g.auditManagement(c, audit)
	}()
	if e := g.alerts.TestChannel(c.Context(), id); e != nil {
		if meta.IsNotFound(e) {
			audit.FailureReason = "not_found"
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		}
		audit.FailureReason = "delivery_failed"
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": outbound.SanitizeError(e)})
	}
	audit.Success = true
	return c.JSON(channelTestResponse{OK: true, Message: "test notification sent"})
}

func (g *Gateway) listSilences(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	v, e := g.alerts.ListSilences(c.Context(), proj.Slug)
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(toSilenceResponses(v))
}

func (g *Gateway) createSilence(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "silence.create",
		TargetType:  "silence",
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "create_failed"
		}
		g.auditManagement(c, audit)
	}()
	var in alert.SilenceInput
	if c.Bind().Body(&in) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	in.Creator = u.Username
	in.Project = proj.Slug
	v, e := g.alerts.CreateSilence(c.Context(), in)
	if e != nil {
		audit.FailureReason = alertFailureReason(e, "create_failed")
		return alertAPIError(c, e)
	}
	audit.TargetID = v.ID
	audit.Success = true
	return c.Status(fiber.StatusCreated).JSON(toSilenceResponse(v))
}

func (g *Gateway) updateSilence(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "silence.update",
		TargetType:  "silence",
		TargetID:    id,
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "update_failed"
		}
		g.auditManagement(c, audit)
	}()
	var in alert.SilenceInput
	if c.Bind().Body(&in) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	in.Project = proj.Slug
	v, e := g.alerts.UpdateSilence(c.Context(), id, in)
	if e != nil {
		audit.FailureReason = alertFailureReason(e, "update_failed")
		return alertAPIError(c, e)
	}
	audit.TargetID = v.ID
	audit.Success = true
	return c.JSON(toSilenceResponse(v))
}

func (g *Gateway) deleteSilence(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "silence.delete",
		TargetType:  "silence",
		TargetID:    id,
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "delete_failed"
		}
		g.auditManagement(c, audit)
	}()
	if e := g.alerts.DeleteSilence(c.Context(), proj.Slug, id); e != nil {
		audit.FailureReason = alertFailureReason(e, "delete_failed")
		return apiError(c, e)
	}
	audit.Success = true
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listAlerts(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	status := c.Query("status")
	if status != "" && status != "firing" && status != "resolved" {
		return badRequest(c)
	}
	var inhibited *bool
	if raw := c.Query("inhibited"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return badRequest(c)
		}
		inhibited = &value
	}
	v, e := g.alerts.ListEvents(c.Context(), proj.Slug, status, inhibited)
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(toAlertEventResponses(v))
}

func (g *Gateway) listInhibitionRules(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	v, err := g.alerts.ListInhibitionRules(c.Context(), proj.Slug)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(v)
}

func (g *Gateway) createInhibitionRule(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "inhibition_rule.create",
		TargetType:  "inhibition_rule",
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "create_failed"
		}
		g.auditManagement(c, audit)
	}()
	var in alert.InhibitionRuleInput
	if c.Bind().Body(&in) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	in.Project = proj.Slug
	v, err := g.alerts.CreateInhibitionRule(c.Context(), in)
	if err != nil {
		audit.FailureReason = alertFailureReason(err, "create_failed")
		return alertAPIError(c, err)
	}
	audit.TargetID = v.ID
	audit.Success = true
	return c.Status(fiber.StatusCreated).JSON(v)
}

func (g *Gateway) updateInhibitionRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "inhibition_rule.update",
		TargetType:  "inhibition_rule",
		TargetID:    id,
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "update_failed"
		}
		g.auditManagement(c, audit)
	}()
	var in alert.InhibitionRuleInput
	if c.Bind().Body(&in) != nil {
		audit.FailureReason = "invalid_request"
		return badRequest(c)
	}
	in.Project = proj.Slug
	v, err := g.alerts.UpdateInhibitionRule(c.Context(), id, in)
	if err != nil {
		audit.FailureReason = alertFailureReason(err, "update_failed")
		return alertAPIError(c, err)
	}
	audit.TargetID = v.ID
	audit.Success = true
	return c.JSON(v)
}

func (g *Gateway) deleteInhibitionRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	proj, ok := sessionProject(c)
	if !ok {
		return nil
	}
	audit := auditEvent{
		Action:      "inhibition_rule.delete",
		TargetType:  "inhibition_rule",
		TargetID:    id,
		ProjectID:   proj.ID,
		ProjectSlug: proj.Slug,
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "delete_failed"
		}
		g.auditManagement(c, audit)
	}()
	if err := g.alerts.DeleteInhibitionRule(c.Context(), proj.Slug, id); err != nil {
		audit.FailureReason = alertFailureReason(err, "delete_failed")
		return apiError(c, err)
	}
	audit.Success = true
	return c.SendStatus(fiber.StatusNoContent)
}
