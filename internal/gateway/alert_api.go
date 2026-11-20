package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/meta"
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

func toAlertRuleResponse(value meta.AlertRuleView) alertRuleResponse {
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
		CreatedAt:        apiTime(value.CreatedAt),
		UpdatedAt:        apiTime(value.UpdatedAt),
	}
}

func toAlertRuleResponses(values []meta.AlertRuleView) []alertRuleResponse {
	out := make([]alertRuleResponse, 0, len(values))
	for _, value := range values {
		out = append(out, toAlertRuleResponse(value))
	}
	return out
}

func toNotificationChannelResponse(value meta.NotificationChannelView) notificationChannelResponse {
	return notificationChannelResponse{
		ID: strconv.Itoa(
			value.ID,
		),
		Type:      value.Type,
		Name:      value.Name,
		Config:    anyMap(normalizeChannelConfig(value.Type, value.Config)),
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

func projectSlug(c fiber.Ctx) string {
	slug := strings.TrimSpace(c.Get("X-LO-Project"))
	if slug == "" {
		slug = "default"
	}
	return slug
}
func (g *Gateway) alertingUnavailable(c fiber.Ctx) bool {
	if g.alerts == nil {
		_ = c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "alerting unavailable"})
		return true
	}
	return false
}

func (g *Gateway) listAlertRules(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	v, e := g.alerts.ListRules(c.Context(), projectSlug(c))
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(toAlertRuleResponses(v))
}
func (g *Gateway) createAlertRule(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	var request alertRuleRequest
	if c.Bind().Body(&request) != nil || strings.TrimSpace(request.Name) == "" ||
		strings.TrimSpace(request.Expr) == "" ||
		!validAlertSignal(request.Signal, true) {
		return badRequest(c)
	}
	in := request.input(projectSlug(c))
	v, e := g.alerts.CreateRule(c.Context(), in)
	if e != nil {
		return apiError(c, e)
	}
	return c.Status(fiber.StatusCreated).JSON(toAlertRuleResponse(v))
}
func (g *Gateway) updateAlertRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	var request alertRuleRequest
	if c.Bind().Body(&request) != nil || !validAlertSignal(request.Signal, true) {
		return badRequest(c)
	}
	in := request.input(projectSlug(c))
	v, e := g.alerts.UpdateRule(c.Context(), id, in)
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(toAlertRuleResponse(v))
}

func validAlertSignal(signal string, allowEmpty bool) bool {
	switch strings.TrimSpace(signal) {
	case "metrics", "logs":
		return true
	case "":
		return allowEmpty
	default:
		return false
	}
}
func (g *Gateway) deleteAlertRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	if e := g.alerts.DeleteRule(c.Context(), projectSlug(c), id); e != nil {
		return apiError(c, e)
	}
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
	return c.JSON(toNotificationChannelResponses(v))
}
func (g *Gateway) createChannel(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	var in alert.ChannelInput
	if c.Bind().Body(&in) != nil || !validChannelType(in.Type) || strings.TrimSpace(in.Name) == "" {
		return badRequest(c)
	}
	in.Type = normalizeChannelType(in.Type)
	in.Config = normalizeChannelConfig(in.Type, in.Config)
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	v, e := g.alerts.CreateChannel(c.Context(), in)
	if e != nil {
		return apiError(c, e)
	}
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
	var in alert.ChannelInput
	if c.Bind().Body(&in) != nil || (in.Type != "" && !validChannelType(in.Type)) {
		return badRequest(c)
	}
	in.Type = normalizeChannelType(in.Type)
	in.Config = normalizeChannelConfig(in.Type, in.Config)
	v, e := g.alerts.UpdateChannel(c.Context(), id, in)
	if e != nil {
		return apiError(c, e)
	}
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
	if e := g.alerts.DeleteChannel(c.Context(), id); e != nil {
		return apiError(c, e)
	}
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
	if e := g.alerts.TestChannel(c.Context(), id); e != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": e.Error()})
	}
	return c.JSON(channelTestResponse{OK: true, Message: "test notification sent"})
}
func validChannelType(v string) bool {
	switch normalizeChannelType(v) {
	case "webhook", "feishu", "dingtalk", "wecom", "smtp", "slack":
		return true
	}
	return false
}

func normalizeChannelType(v string) string {
	if v == "email" {
		return "smtp"
	}
	return v
}

func normalizeChannelConfig(kind string, config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	if kind != "smtp" {
		return config
	}
	out := make(map[string]any, len(config))
	for key, value := range config {
		out[key] = value
	}
	if _, ok := out["host"]; !ok {
		if value, exists := out["smtp_host"]; exists {
			out["host"] = value
		}
	}
	if _, ok := out["port"]; !ok {
		if value, exists := out["smtp_port"]; exists {
			out["port"] = value
		}
	}
	delete(out, "smtp_host")
	delete(out, "smtp_port")
	return out
}

func (g *Gateway) listSilences(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	v, e := g.alerts.ListSilences(c.Context(), projectSlug(c))
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(toSilenceResponses(v))
}
func (g *Gateway) createSilence(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	var in alert.SilenceInput
	if c.Bind().Body(&in) != nil || len(in.Matchers) == 0 || in.StartsAt.IsZero() || !in.EndsAt.After(in.StartsAt) {
		return badRequest(c)
	}
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	in.Creator = u.Username
	in.Project = projectSlug(c)
	v, e := g.alerts.CreateSilence(c.Context(), in)
	if e != nil {
		return apiError(c, e)
	}
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
	var in alert.SilenceInput
	if c.Bind().Body(&in) != nil {
		return badRequest(c)
	}
	in.Project = projectSlug(c)
	v, e := g.alerts.UpdateSilence(c.Context(), id, in)
	if e != nil {
		return apiError(c, e)
	}
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
	if e := g.alerts.DeleteSilence(c.Context(), projectSlug(c), id); e != nil {
		return apiError(c, e)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
func (g *Gateway) listAlerts(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
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
	v, e := g.alerts.ListEvents(c.Context(), projectSlug(c), status, inhibited)
	if e != nil {
		return apiError(c, e)
	}
	return c.JSON(toAlertEventResponses(v))
}

func (g *Gateway) listInhibitionRules(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	v, err := g.alerts.ListInhibitionRules(c.Context(), projectSlug(c))
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(v)
}

func (g *Gateway) createInhibitionRule(c fiber.Ctx) error {
	if g.alertingUnavailable(c) {
		return nil
	}
	var in alert.InhibitionRuleInput
	if c.Bind().Body(&in) != nil || !validMatchers(in.SourceMatchers) || !validMatchers(in.TargetMatchers) {
		return badRequest(c)
	}
	in.Project = projectSlug(c)
	v, err := g.alerts.CreateInhibitionRule(c.Context(), in)
	if err != nil {
		return apiError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(v)
}

func validMatchers(matchers []alert.Matcher) bool {
	if len(matchers) == 0 {
		return false
	}
	for _, matcher := range matchers {
		if strings.TrimSpace(matcher.Name) == "" {
			return false
		}
		switch matcher.Op {
		case "=":
		case "=~":
			if _, err := regexp.Compile(matcher.Value); err != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (g *Gateway) updateInhibitionRule(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.alertingUnavailable(c) {
		return nil
	}
	var in alert.InhibitionRuleInput
	if c.Bind().Body(&in) != nil || (in.SourceMatchers != nil && !validMatchers(in.SourceMatchers)) ||
		(in.TargetMatchers != nil && !validMatchers(in.TargetMatchers)) {
		return badRequest(c)
	}
	in.Project = projectSlug(c)
	v, err := g.alerts.UpdateInhibitionRule(c.Context(), id, in)
	if err != nil {
		return apiError(c, err)
	}
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
	if err := g.alerts.DeleteInhibitionRule(c.Context(), projectSlug(c), id); err != nil {
		return apiError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
