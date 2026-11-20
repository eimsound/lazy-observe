package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/state"
)

// ErrInvalidChannelConfig reports a rejected notification channel configuration.
var ErrInvalidChannelConfig = errors.New("invalid notification channel config")

type Config struct {
	Store           *meta.Store
	Sender          *Sender
	OutboundPolicy  outbound.Policy
	GroupWait       time.Duration
	RepeatInterval  time.Duration
	StormThreshold  int
	RateLimitWindow time.Duration
	Now             func() time.Time
	SharedLimiter   state.Limiter
	Outbox          OutboxWorkerConfig
	Signals         SignalBackend
	Evaluator       RuleEvaluatorConfig
}

type Service struct {
	store          *meta.Store
	sender         *Sender
	engine         *Engine
	now            func() time.Time
	repeatInterval time.Duration
	rateMu         sync.Mutex
	rate           map[int][]time.Time
	outbox         *OutboxWorker
	evaluator      *RuleEvaluator
}

func NewService(cfg Config) *Service {
	if cfg.Sender == nil {
		cfg.Sender = &Sender{Outbound: cfg.OutboundPolicy}
	} else if !cfg.Sender.Outbound.AllowPrivate && len(cfg.Sender.Outbound.AllowedCIDRs) == 0 &&
		len(cfg.Sender.Outbound.AllowedHosts) == 0 && len(cfg.Sender.Outbound.BlockedPorts) == 0 {
		cfg.Sender.Outbound = cfg.OutboundPolicy
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RepeatInterval <= 0 {
		cfg.RepeatInterval = 4 * time.Hour
	}
	s := &Service{
		store:          cfg.Store,
		sender:         cfg.Sender,
		now:            cfg.Now,
		repeatInterval: cfg.RepeatInterval,
		rate:           make(map[int][]time.Time),
	}
	s.engine = NewEngine(
		EngineConfig{
			GroupWait:       cfg.GroupWait,
			RepeatInterval:  cfg.RepeatInterval,
			StormThreshold:  cfg.StormThreshold,
			Now:             cfg.Now,
			Silences:        s.activeSilences,
			InhibitionRules: s.activeInhibitionRules,
			Notify:          s.notify,
		},
	)
	cfg.Outbox.Store = cfg.Store
	cfg.Outbox.Sender = cfg.Sender
	if cfg.RateLimitWindow > 0 {
		cfg.Outbox.RateLimitWindow = cfg.RateLimitWindow
	}
	if cfg.SharedLimiter != nil {
		cfg.Outbox.Limiter = cfg.SharedLimiter
	}
	if cfg.Now != nil {
		cfg.Outbox.Now = cfg.Now
	}
	s.outbox = NewOutboxWorker(cfg.Outbox)
	s.outbox.Start()
	cfg.Evaluator.Store = cfg.Store
	if cfg.Signals != nil {
		cfg.Evaluator.Backend = cfg.Signals
	}
	if cfg.Now != nil {
		cfg.Evaluator.Now = cfg.Now
	}
	if cfg.GroupWait > 0 {
		cfg.Evaluator.GroupWait = cfg.GroupWait
	}
	if cfg.RepeatInterval > 0 {
		cfg.Evaluator.RepeatInterval = cfg.RepeatInterval
	}
	cfg.Evaluator.WakeOutbox = s.outbox.Wake
	s.evaluator = NewRuleEvaluator(cfg.Evaluator)
	s.evaluator.Start()
	return s
}
func (s *Service) Close() { s.engine.Close(); s.evaluator.Close(); s.outbox.Close() }

type RuleInput struct {
	Project          string            `json:"project"`
	Name             string            `json:"name"`
	Signal           string            `json:"signal"`
	EvaluationEngine string            `json:"evaluation_engine"`
	Expr             string            `json:"expr"`
	For              string            `json:"for"`
	Severity         string            `json:"severity"`
	Labels           map[string]string `json:"labels"`
	Annotations      map[string]string `json:"annotations"`
	Enabled          *bool             `json:"enabled"`
	ChannelIDs       []int             `json:"channel_ids"`
}

func (s *Service) ListRules(ctx context.Context, slug string) ([]meta.AlertRuleView, error) {
	return s.store.ListAlertRules(ctx, slug)
}
func (s *Service) CreateRule(ctx context.Context, in RuleInput) (meta.AlertRuleView, error) {
	if in.EvaluationEngine != "" && in.EvaluationEngine != "native" {
		return meta.AlertRuleView{}, fmt.Errorf("unsupported alert evaluation engine %q", in.EvaluationEngine)
	}
	return s.store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: in.Project,
		Name:        in.Name,
		Signal:      defaultString(in.Signal, "metrics"),
		Expression:  in.Expr,
		For:         defaultString(in.For, "0s"),
		Severity:    defaultString(in.Severity, "warning"),
		Labels:      nonNilMap(in.Labels),
		Annotations: nonNilMap(in.Annotations),
		Enabled:     in.Enabled,
		ChannelIDs:  in.ChannelIDs,
	})
}
func (s *Service) UpdateRule(ctx context.Context, id int, in RuleInput) (meta.AlertRuleView, error) {
	old, err := s.store.AlertRule(ctx, in.Project, id)
	if err != nil {
		return meta.AlertRuleView{}, err
	}
	if in.EvaluationEngine != "" && in.EvaluationEngine != "native" {
		return meta.AlertRuleView{}, fmt.Errorf("unsupported alert evaluation engine %q", in.EvaluationEngine)
	}
	update := meta.AlertRuleUpdate{
		Labels:             in.Labels,
		Annotations:        in.Annotations,
		Enabled:            in.Enabled,
		ActivateEvaluation: in.EvaluationEngine == "native" && old.EvaluationEngine == "paused",
		ResolveFiring: (in.Expr != "" && in.Expr != old.Expression) ||
			(in.Signal != "" && in.Signal != old.Signal) ||
			(in.Enabled != nil && *in.Enabled != old.Enabled),
	}
	if in.Name != "" {
		update.Name = &in.Name
	}
	if in.Signal != "" {
		update.Signal = &in.Signal
	}
	if in.Expr != "" {
		update.Expression = &in.Expr
	}
	if in.For != "" {
		update.For = &in.For
	}
	if in.Severity != "" {
		update.Severity = &in.Severity
	}
	if in.ChannelIDs != nil {
		update.ChannelIDs = &in.ChannelIDs
	}
	return s.store.UpdateAlertRule(ctx, in.Project, id, update)
}
func (s *Service) DeleteRule(ctx context.Context, projectSlug string, id int) error {
	return s.store.DeleteAlertRule(ctx, projectSlug, id)
}

type ChannelInput struct {
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Config  map[string]any `json:"config"`
	Enabled *bool          `json:"enabled"`
}

func (s *Service) ListChannels(ctx context.Context) ([]meta.NotificationChannelView, error) {
	return s.store.ListNotificationChannels(ctx)
}

func (s *Service) CreateChannel(ctx context.Context, in ChannelInput) (meta.NotificationChannelView, error) {
	if err := validateChannelConfig(in.Type, in.Config, s.sender.Outbound); err != nil {
		return meta.NotificationChannelView{}, err
	}
	return s.store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: in.Type, Name: in.Name, Config: in.Config, Enabled: in.Enabled,
	})
}

func (s *Service) UpdateChannel(ctx context.Context, id int, in ChannelInput) (meta.NotificationChannelView, error) {
	update := meta.NotificationChannelUpdate{Enabled: in.Enabled}
	if in.Type != "" {
		update.Type = &in.Type
	}
	if in.Name != "" {
		update.Name = &in.Name
	}
	if in.Config != nil {
		existing, loadErr := s.store.NotificationChannel(ctx, id)
		if loadErr != nil && meta.IsNotFound(loadErr) {
			return meta.NotificationChannelView{}, loadErr
		}
		effectiveType := in.Type
		if effectiveType == "" {
			effectiveType = existing.Type
		}
		merged, err := preserveRedactedChannelSecrets(in.Config, existing.Config, loadErr)
		if err != nil {
			return meta.NotificationChannelView{}, err
		}
		if err := validateChannelConfig(effectiveType, merged, s.sender.Outbound); err != nil {
			return meta.NotificationChannelView{}, err
		}
		update.Config = merged
	}
	return s.store.UpdateNotificationChannel(ctx, id, update)
}

func validateChannelConfig(kind string, cfg map[string]any, policy outbound.Policy) error {
	if cfg == nil {
		return nil
	}
	if kind == "smtp" {
		port, err := parseSMTPPort(cfg["port"], 587)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidChannelConfig, outbound.SanitizeError(err))
		}
		if host, ok := cfg["host"].(string); ok && strings.TrimSpace(host) != "" {
			if err := outbound.ValidateHostPort(host, port, policy); err != nil {
				return fmt.Errorf("%w: %s", ErrInvalidChannelConfig, outbound.SanitizeError(err))
			}
		}
		if from, ok := cfg["from"].(string); ok && hasHeaderInjection(from) {
			return fmt.Errorf("%w: smtp from contains invalid control characters", ErrInvalidChannelConfig)
		}
		for _, recipient := range stringList(cfg["to"]) {
			if hasHeaderInjection(recipient) {
				return fmt.Errorf("%w: smtp recipient contains invalid control characters", ErrInvalidChannelConfig)
			}
		}
		return nil
	}
	if rawURL, ok := cfg["url"].(string); ok && strings.TrimSpace(rawURL) != "" {
		if _, err := outbound.ValidateURL(rawURL, policy); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidChannelConfig, outbound.SanitizeError(err))
		}
	}
	return nil
}

// RedactChannelConfig returns a copy of cfg safe to return from admin APIs:
// webhook URLs have tokens/query values stripped and secret/password/header
// values are replaced with [REDACTED].
func RedactChannelConfig(_ string, cfg map[string]any) map[string]any {
	if cfg == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if k == "headers" {
			if headers, ok := v.(map[string]any); ok {
				redactedHeaders := make(map[string]any, len(headers))
				for hk, hv := range headers {
					if str, isStr := hv.(string); isStr && str != "" {
						redactedHeaders[hk] = outbound.RedactedValue
					} else {
						redactedHeaders[hk] = hv
					}
				}
				out[k] = redactedHeaders
				continue
			}
		}
		if strings.EqualFold(k, "url") {
			if raw, ok := v.(string); ok && raw != "" {
				if raw == outbound.RedactedValue || strings.HasPrefix(raw, "enc:v1:") {
					out[k] = outbound.RedactedValue
				} else {
					out[k] = outbound.SanitizeURL(raw)
				}
				continue
			}
		}
		if isSecretConfigField(k) {
			if raw, ok := v.(string); ok && raw != "" {
				out[k] = outbound.RedactedValue
				continue
			}
		}
		out[k] = v
	}
	return out
}

func preserveRedactedChannelSecrets(
	incoming, existing map[string]any,
	existingErr error,
) (map[string]any, error) {
	out := make(map[string]any, len(incoming))
	for k, v := range incoming {
		if k == "headers" {
			if headers, ok := v.(map[string]any); ok {
				existingHeaders, _ := existing["headers"].(map[string]any)
				mergedHeaders := make(map[string]any, len(headers))
				for hk, hv := range headers {
					if str, isStr := hv.(string); isStr && str == outbound.RedactedValue {
						if existingErr != nil {
							return nil, fmt.Errorf(
								"%w: existing channel secret cannot be preserved; supply a new value",
								ErrInvalidChannelConfig,
							)
						}
						if prev, exists := existingHeaders[hk]; exists {
							mergedHeaders[hk] = prev
							continue
						}
					}
					mergedHeaders[hk] = hv
				}
				out[k] = mergedHeaders
				continue
			}
		}
		if strings.EqualFold(k, "url") {
			if raw, ok := v.(string); ok && raw != "" {
				existingURL, _ := existing["url"].(string)
				if raw == outbound.RedactedValue || (existingURL != "" && raw == outbound.SanitizeURL(existingURL)) {
					if existingErr != nil {
						return nil, fmt.Errorf(
							"%w: existing channel url cannot be preserved; supply a new value",
							ErrInvalidChannelConfig,
						)
					}
					if existingURL != "" {
						out[k] = existingURL
						continue
					}
				}
			}
		}
		if isSecretConfigField(k) {
			if raw, ok := v.(string); ok && raw == outbound.RedactedValue {
				if existingErr != nil {
					return nil, fmt.Errorf(
						"%w: existing channel secret cannot be preserved; supply a new value",
						ErrInvalidChannelConfig,
					)
				}
				if prev, exists := existing[k]; exists {
					out[k] = prev
					continue
				}
			}
		}
		out[k] = v
	}
	return out, nil
}

func isSecretConfigField(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"secret", "password", "passwd", "token", "key", "auth", "credential", "cookie"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
func (s *Service) DeleteChannel(ctx context.Context, id int) error {
	return s.store.DeleteNotificationChannel(ctx, id)
}
func (s *Service) TestChannel(ctx context.Context, id int) error {
	ch, err := s.store.NotificationChannel(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.sender.Send(
		ctx,
		channelFromView(ch),
		Notification{
			Project: "test",
			Rule:    "test",
			Status:  "firing",
			Alerts:  []Alert{{Status: "firing", Labels: map[string]string{"test": "true"}, StartsAt: time.Now()}},
		},
	)
	return err
}

type SilenceInput struct {
	Project  string    `json:"project"`
	Matchers []Matcher `json:"matchers"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Comment  string    `json:"comment"`
	Creator  string    `json:"creator"`
}

func (s *Service) ListSilences(ctx context.Context, slug string) ([]meta.SilenceView, error) {
	return s.store.ListSilences(ctx, slug)
}
func (s *Service) CreateSilence(ctx context.Context, in SilenceInput) (meta.SilenceView, error) {
	return s.store.CreateSilence(ctx, meta.SilenceCreate{
		ProjectSlug: in.Project,
		Matchers:    matchersToMaps(in.Matchers),
		StartsAt:    in.StartsAt,
		EndsAt:      in.EndsAt,
		Comment:     in.Comment,
		Creator:     in.Creator,
	})
}
func (s *Service) UpdateSilence(ctx context.Context, id int, in SilenceInput) (meta.SilenceView, error) {
	update := meta.SilenceUpdate{}
	if in.Matchers != nil {
		update.Matchers = matchersToMaps(in.Matchers)
	}
	if !in.StartsAt.IsZero() {
		update.StartsAt = &in.StartsAt
	}
	if !in.EndsAt.IsZero() {
		update.EndsAt = &in.EndsAt
	}
	if in.Comment != "" {
		update.Comment = &in.Comment
	}
	return s.store.UpdateSilence(ctx, in.Project, id, update)
}
func (s *Service) DeleteSilence(ctx context.Context, projectSlug string, id int) error {
	return s.store.DeleteSilence(ctx, projectSlug, id)
}
func (s *Service) ListEvents(
	ctx context.Context,
	slug, status string,
	inhibited *bool,
) ([]meta.AlertEventView, error) {
	return s.store.ListAlertEvents(ctx, slug, status, inhibited)
}

func (s *Service) activeSilences(ctx context.Context, slug string, now time.Time) ([][]Matcher, error) {
	projectInfo, err := s.store.ProjectBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	policy, err := s.store.AlertPolicySnapshot(ctx, projectInfo.ID, now)
	if err != nil {
		return nil, err
	}
	out := make([][]Matcher, 0, len(policy.Silences))
	for _, matchers := range policy.Silences {
		out = append(out, mapsToMatchers(matchers))
	}
	return out, nil
}

type InhibitionRuleInput struct {
	Project        string    `json:"project"`
	SourceMatchers []Matcher `json:"source_matchers"`
	TargetMatchers []Matcher `json:"target_matchers"`
	EqualLabels    []string  `json:"equal_labels"`
	Enabled        *bool     `json:"enabled"`
	Comment        string    `json:"comment"`
}

func (s *Service) ListInhibitionRules(ctx context.Context, slug string) ([]meta.InhibitionRuleView, error) {
	return s.store.ListInhibitionRules(ctx, slug)
}

func (s *Service) CreateInhibitionRule(ctx context.Context, in InhibitionRuleInput) (meta.InhibitionRuleView, error) {
	rule, err := s.store.CreateInhibitionRule(ctx, meta.InhibitionRuleCreate{
		ProjectSlug:    in.Project,
		SourceMatchers: matchersToMaps(in.SourceMatchers),
		TargetMatchers: matchersToMaps(in.TargetMatchers),
		EqualLabels:    nonNilStrings(in.EqualLabels),
		Enabled:        in.Enabled,
		Comment:        in.Comment,
	})
	if err == nil {
		err = s.recomputeInhibited(ctx, in.Project)
		s.engine.Reevaluate(in.Project)
	}
	return rule, err
}

func (s *Service) UpdateInhibitionRule(
	ctx context.Context,
	id int,
	in InhibitionRuleInput,
) (meta.InhibitionRuleView, error) {
	update := meta.InhibitionRuleUpdate{Enabled: in.Enabled}
	if in.SourceMatchers != nil {
		update.SourceMatchers = matchersToMaps(in.SourceMatchers)
	}
	if in.TargetMatchers != nil {
		update.TargetMatchers = matchersToMaps(in.TargetMatchers)
	}
	if in.EqualLabels != nil {
		update.EqualLabels = in.EqualLabels
	}
	if in.Comment != "" {
		update.Comment = &in.Comment
	}
	rule, err := s.store.UpdateInhibitionRule(ctx, in.Project, id, update)
	if err == nil {
		err = s.recomputeInhibited(ctx, rule.ProjectSlug)
		s.engine.Reevaluate(rule.ProjectSlug)
	}
	return rule, err
}

func (s *Service) DeleteInhibitionRule(ctx context.Context, projectSlug string, id int) error {
	projectInfo, err := s.store.DeleteInhibitionRule(ctx, projectSlug, id)
	if err != nil {
		return err
	}
	if err := s.recomputeInhibited(ctx, projectInfo.Slug); err != nil {
		return err
	}
	s.engine.Reevaluate(projectInfo.Slug)
	return nil
}

func (s *Service) activeInhibitionRules(ctx context.Context, slug string) ([]InhibitionRule, error) {
	projectInfo, err := s.store.ProjectBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	policy, err := s.store.AlertPolicySnapshot(ctx, projectInfo.ID, s.now())
	if err != nil {
		return nil, err
	}
	out := make([]InhibitionRule, 0, len(policy.InhibitionRules))
	for _, item := range policy.InhibitionRules {
		out = append(out, InhibitionRule{
			SourceMatchers: mapsToMatchers(item.SourceMatchers),
			TargetMatchers: mapsToMatchers(item.TargetMatchers),
			EqualLabels:    item.EqualLabels,
		})
	}
	return out, nil
}

func (s *Service) recomputeInhibited(ctx context.Context, slug string) error {
	projectInfo, err := s.store.ProjectBySlug(ctx, slug)
	if err != nil {
		return err
	}
	policy, err := s.store.AlertPolicySnapshot(ctx, projectInfo.ID, s.now())
	if err != nil {
		return err
	}
	rules := make([]InhibitionRule, 0, len(policy.InhibitionRules))
	for _, item := range policy.InhibitionRules {
		rules = append(rules, InhibitionRule{
			SourceMatchers: mapsToMatchers(item.SourceMatchers),
			TargetMatchers: mapsToMatchers(item.TargetMatchers),
			EqualLabels:    item.EqualLabels,
		})
	}
	sources := make([]Alert, 0, len(policy.FiringEvents))
	for _, event := range policy.FiringEvents {
		sources = append(sources, Alert{Status: "firing", Labels: event.Labels, Fingerprint: event.Fingerprint})
	}
	updates := make([]meta.AlertEventInhibition, 0, len(policy.FiringEvents))
	for _, event := range policy.FiringEvents {
		inhibited := IsInhibited(
			Alert{Status: "firing", Labels: event.Labels, Fingerprint: event.Fingerprint},
			sources,
			rules,
		)
		if event.Inhibited != inhibited {
			updates = append(updates, meta.AlertEventInhibition{EventID: event.ID, Inhibited: inhibited})
		}
	}
	return s.store.SetAlertEventInhibition(ctx, updates)
}
func (s *Service) notify(ctx context.Context, n Notification) error {
	if n.Storm != nil {
		return s.notifyStorm(ctx, n)
	}
	if len(n.Alerts) > 0 && n.Alerts[0].Labels["source"] == "platform" {
		return s.notifyPlatform(ctx, n)
	}
	return nil
}

func (s *Service) notifyStorm(ctx context.Context, n Notification) error {
	channels, projectInfo, err := s.store.StormNotificationChannels(ctx, n.Project)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return err
	}
	bucket := s.now().UnixNano() / s.repeatInterval.Nanoseconds()
	keyPayload := append(append([]byte(nil), payload...), []byte(fmt.Sprintf("\x00storm\x00%d", bucket))...)
	var errs []error
	for _, ch := range channels {
		_, inserted, enqueueErr := s.store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
			IdempotencyKey: notificationOutboxKey(ch.ID, keyPayload), ProjectID: projectInfo.ID,
			ChannelID: ch.ID, Payload: payload,
		})
		if enqueueErr != nil {
			errs = append(errs, enqueueErr)
		} else if inserted {
			s.outbox.Wake()
		}
	}
	return errors.Join(errs...)
}

const errorRateWindow = 5 * time.Minute

type ProjectAlertConfigInput struct {
	NewIssue                    bool  `json:"new_issue"`
	Regression                  bool  `json:"regression"`
	ErrorRateThresholdPerMinute int   `json:"error_rate_threshold_per_min"`
	ChannelIDs                  []int `json:"channel_ids"`
}

type ProjectAlertConfigView = meta.ProjectAlertConfigView

func (s *Service) GetProjectAlertConfig(ctx context.Context, projectID int) (ProjectAlertConfigView, error) {
	return s.store.ProjectAlertConfig(ctx, projectID)
}

func (s *Service) PutProjectAlertConfig(
	ctx context.Context,
	projectID int,
	in ProjectAlertConfigInput,
) (ProjectAlertConfigView, error) {
	if in.ErrorRateThresholdPerMinute < 0 {
		return ProjectAlertConfigView{}, fmt.Errorf("error rate threshold must not be negative")
	}
	cfg, err := s.store.PutProjectAlertConfig(ctx, projectID, meta.ProjectAlertConfigUpdate{
		NewIssue:                    in.NewIssue,
		Regression:                  in.Regression,
		ErrorRateThresholdPerMinute: in.ErrorRateThresholdPerMinute,
		ChannelIDs:                  in.ChannelIDs,
	})
	if err != nil {
		return ProjectAlertConfigView{}, err
	}
	s.rateMu.Lock()
	delete(s.rate, projectID)
	s.rateMu.Unlock()
	return cfg, nil
}

// HandleIssueChange injects a committed issue lifecycle event into the same
// grouping, silence and notification engine used by platform-event alerts.
func (s *Service) HandleIssueChange(ctx context.Context, change errortrack.IssueChange) error {
	cfg, p, err := s.platformConfig(ctx, change.ProjectID)
	if err != nil {
		return err
	}
	if (change.Kind == "new_issue" && !cfg.NewIssue) || (change.Kind == "regression" && !cfg.Regression) {
		return nil
	}
	labels := platformLabels(p.Slug, change.Kind)
	labels["level"] = change.Level
	labels["issue_id"] = strconv.Itoa(change.IssueID)
	s.engine.Add(ctx, p.Slug, change.Kind, Alert{Status: "firing", Labels: labels, Annotations: map[string]string{
		"summary": change.Title,
		"title":   change.Title,
		"issue":   "/issues/" + strconv.Itoa(change.IssueID),
		"link":    "/issues/" + strconv.Itoa(change.IssueID),
	}, StartsAt: change.SeenAt, Transient: true})
	return nil
}

// RecordErrorEvent uses a five-minute sliding window. A spike fires when the
// observed count divided by five minutes is strictly greater than the configured
// absolute per-minute threshold. Threshold zero disables this event type.
func (s *Service) RecordErrorEvent(ctx context.Context, projectID int, occurredAt time.Time) error {
	cfg, p, err := s.platformConfig(ctx, projectID)
	if err != nil {
		return err
	}
	if cfg.ErrorRateThresholdPerMinute == 0 {
		return nil
	}
	now := s.now().UTC()
	cutoff := now.Add(-errorRateWindow)
	occurredAt = occurredAt.UTC()
	if occurredAt.IsZero() || occurredAt.Before(cutoff) {
		return nil
	}
	s.rateMu.Lock()
	times := s.rate[projectID]
	retained := times[:0]
	for _, observedAt := range times {
		if !observedAt.Before(cutoff) {
			retained = append(retained, observedAt)
		}
	}
	times = append(retained, occurredAt)
	s.rate[projectID] = times
	spike := len(times) > cfg.ErrorRateThresholdPerMinute*int(errorRateWindow/time.Minute)
	rate := float64(len(times)) / errorRateWindow.Minutes()
	s.rateMu.Unlock()
	if !spike {
		return nil
	}
	s.engine.Add(
		ctx,
		p.Slug,
		"error_rate_spike",
		Alert{Status: "firing", Labels: platformLabels(p.Slug, "error_rate_spike"), Annotations: map[string]string{
			"summary": fmt.Sprintf("Error rate spike: %.1f events/min", rate),
			"window":  "5m", "threshold_per_min": strconv.Itoa(cfg.ErrorRateThresholdPerMinute),
		}, StartsAt: now},
	)
	return nil
}

func (s *Service) platformConfig(
	ctx context.Context,
	projectID int,
) (meta.ProjectAlertConfigView, meta.ProjectInfo, error) {
	return s.store.ProjectAlertPolicy(ctx, projectID)
}

func platformLabels(slug, kind string) map[string]string {
	return map[string]string{"source": "platform", "project": slug, "project_id": slug, "kind": kind, "alertname": kind}
}

func (s *Service) notifyPlatform(ctx context.Context, n Notification) error {
	cfg, _, err := s.store.ProjectAlertPolicyBySlug(ctx, n.Project)
	if err != nil {
		return err
	}
	kind := n.Rule
	if (kind == "new_issue" && !cfg.NewIssue) || (kind == "regression" && !cfg.Regression) ||
		(kind == "error_rate_spike" && cfg.ErrorRateThresholdPerMinute == 0) {
		return nil
	}
	n = s.attachIssueAIAnalysis(ctx, n)
	var errs []error
	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		payload, marshalErr := json.Marshal(n)
		if marshalErr != nil {
			errs = append(errs, fmt.Errorf("encode platform notification for channel %d: %w", ch.ID, marshalErr))
			continue
		}
		priority := 0
		if n.Status == "resolved" {
			priority = 100
		}
		_, inserted, enqueueErr := s.store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
			IdempotencyKey: notificationOutboxKey(ch.ID, payload),
			ProjectID:      cfg.ProjectID,
			ChannelID:      ch.ID,
			Payload:        payload,
			Priority:       priority,
		})
		if enqueueErr != nil {
			errs = append(errs, fmt.Errorf("enqueue platform notification for channel %d: %w", ch.ID, enqueueErr))
			continue
		}
		if inserted {
			s.outbox.Wake()
		}
	}
	return errors.Join(errs...)
}

// attachIssueAIAnalysis is intentionally read-only and best-effort. Automatic
// Issue analysis runs beside the ingest path; when it finishes within the
// alert grouping window, the structured result is added to the notification.
// Missing or failed AI must never delay or suppress the core alert.
func (s *Service) attachIssueAIAnalysis(ctx context.Context, n Notification) Notification {
	p, err := s.store.ProjectBySlug(ctx, n.Project)
	if err != nil {
		return n
	}
	n.Alerts = append([]Alert(nil), n.Alerts...)
	for i := range n.Alerts {
		issueID, parseErr := strconv.Atoi(n.Alerts[i].Labels["issue_id"])
		if parseErr != nil || issueID <= 0 {
			continue
		}
		row, getErr := s.store.GetIssue(ctx, p.ID, issueID)
		if getErr != nil || row.AIAnalysisAt == nil || row.AIAnalysis == nil {
			continue
		}
		summary, _ := row.AIAnalysis["summary"].(string)
		if summary == "" {
			continue
		}
		annotations := make(map[string]string, len(n.Alerts[i].Annotations)+3)
		for key, value := range n.Alerts[i].Annotations {
			annotations[key] = value
		}
		annotations["ai_summary"] = summary
		for source, target := range map[string]string{"probable_causes": "ai_probable_causes", "suggested_actions": "ai_suggested_actions"} {
			if encoded, marshalErr := json.Marshal(
				row.AIAnalysis[source],
			); marshalErr == nil &&
				string(encoded) != "null" {
				annotations[target] = clipNotificationText(string(encoded), 2000)
			}
		}
		n.Alerts[i].Annotations = annotations
	}
	return n
}

func clipNotificationText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func channelFromView(ch meta.NotificationChannelView) Channel {
	return Channel{ID: ch.ID, Type: ch.Type, Name: ch.Name, Config: ch.Config, Enabled: ch.Enabled}
}

func notificationOutboxKey(channelID int, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(strconv.Itoa(channelID)))
	h.Write([]byte{0})
	h.Write(payload)
	return "notification-outbox:" + hex.EncodeToString(h.Sum(nil))
}
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
func defaultString(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func matchersToMaps(ms []Matcher) []map[string]string {
	out := make([]map[string]string, len(ms))
	for i, m := range ms {
		out[i] = map[string]string{"name": m.Name, "value": m.Value, "op": m.Op}
	}
	return out
}
func mapsToMatchers(ms []map[string]string) []Matcher {
	out := make([]Matcher, len(ms))
	for i, m := range ms {
		out[i] = Matcher{Name: m["name"], Value: m["value"], Op: m["op"]}
	}
	return out
}
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
