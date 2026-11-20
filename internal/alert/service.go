package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/state"
	prommodel "github.com/prometheus/common/model"
)

// ErrInvalidChannelConfig reports a rejected notification channel configuration.
var ErrInvalidChannelConfig = errors.New("invalid notification channel config")

// ErrInvalidInput reports a rejected alert rule, silence, inhibition, or config input.
var ErrInvalidInput = errors.New("invalid alert input")

const (
	platformConfigCacheTTL        = 5 * time.Second
	platformRecoveryCheckInterval = 5 * time.Second
)

type cachedPlatformConfig struct {
	cfg       meta.ProjectAlertConfigView
	project   meta.ProjectInfo
	expiresAt time.Time
}

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
	cfgCacheMu     sync.RWMutex
	cfgCache       map[int]cachedPlatformConfig
	outbox         *OutboxWorker
	evaluator      *RuleEvaluator
	monitorCancel  context.CancelFunc
	monitorWG      sync.WaitGroup
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
		cfgCache:       make(map[int]cachedPlatformConfig),
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
	if cfg.StormThreshold > 0 && cfg.Evaluator.StormThreshold <= 0 {
		cfg.Evaluator.StormThreshold = cfg.StormThreshold
	}
	cfg.Evaluator.WakeOutbox = s.outbox.Wake
	s.evaluator = NewRuleEvaluator(cfg.Evaluator)
	s.evaluator.Start()
	if s.store != nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.monitorCancel = cancel
		s.monitorWG.Add(1)
		go s.runPlatformErrorRateMonitor(ctx)
	}
	return s
}

func (s *Service) Close() {
	if s.monitorCancel != nil {
		s.monitorCancel()
		s.monitorWG.Wait()
	}
	s.engine.Close()
	s.evaluator.Close()
	s.outbox.Close()
}

// WakeOutbox nudges the outbox worker to claim newly pending notifications.
func (s *Service) WakeOutbox() {
	if s != nil && s.outbox != nil {
		s.outbox.Wake()
	}
}

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

// ValidateRuleFor validates a Prometheus-style duration string for a rule's
// `for` field (e.g. "0s", "1m", "5m30s").
func ValidateRuleFor(raw string) error {
	d, err := prommodel.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%w: invalid alert for duration %q", ErrInvalidInput, raw)
	}
	if time.Duration(d) < 0 {
		return fmt.Errorf("%w: alert for duration must not be negative", ErrInvalidInput)
	}
	return nil
}

// NormalizeChannelType maps legacy aliases (such as "email") to canonical
// channel types.
func NormalizeChannelType(v string) string {
	if v == "email" {
		return "smtp"
	}
	return v
}

// NormalizeChannelConfig maps legacy SMTP keys (`smtp_host`/`smtp_port`) to
// their canonical names (`host`/`port`) when kind is "smtp".
func NormalizeChannelConfig(kind string, config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	if NormalizeChannelType(kind) != "smtp" {
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

func (s *Service) ListRules(ctx context.Context, slug string) ([]meta.AlertRuleView, error) {
	return s.store.ListAlertRules(ctx, slug)
}
func (s *Service) CreateRule(ctx context.Context, in RuleInput) (meta.AlertRuleView, error) {
	if in.EvaluationEngine != "" && in.EvaluationEngine != "native" {
		return meta.AlertRuleView{}, fmt.Errorf(
			"%w: unsupported alert evaluation engine %q",
			ErrInvalidInput,
			in.EvaluationEngine,
		)
	}
	if in.Signal != "" && in.Signal != "metrics" && in.Signal != "logs" {
		return meta.AlertRuleView{}, fmt.Errorf("%w: unsupported alert signal %q", ErrInvalidInput, in.Signal)
	}
	forDuration := defaultString(in.For, "0s")
	if err := ValidateRuleFor(forDuration); err != nil {
		return meta.AlertRuleView{}, err
	}
	return s.store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: in.Project,
		Name:        in.Name,
		Signal:      defaultString(in.Signal, "metrics"),
		Expression:  in.Expr,
		For:         forDuration,
		Severity:    defaultString(in.Severity, "warning"),
		Labels:      nonNilMap(in.Labels),
		Annotations: nonNilMap(in.Annotations),
		Enabled:     in.Enabled,
		ChannelIDs:  in.ChannelIDs,
	})
}
func (s *Service) UpdateRule(ctx context.Context, id int, in RuleInput) (meta.AlertRuleView, error) {
	if in.EvaluationEngine != "" && in.EvaluationEngine != "native" {
		return meta.AlertRuleView{}, fmt.Errorf(
			"%w: unsupported alert evaluation engine %q",
			ErrInvalidInput,
			in.EvaluationEngine,
		)
	}
	if in.Signal != "" && in.Signal != "metrics" && in.Signal != "logs" {
		return meta.AlertRuleView{}, fmt.Errorf("%w: unsupported alert signal %q", ErrInvalidInput, in.Signal)
	}
	if in.For != "" {
		if err := ValidateRuleFor(in.For); err != nil {
			return meta.AlertRuleView{}, err
		}
	}
	old, err := s.store.AlertRule(ctx, in.Project, id)
	if err != nil {
		return meta.AlertRuleView{}, err
	}
	resolveFiring := (in.Expr != "" && in.Expr != old.Expression) ||
		(in.Signal != "" && in.Signal != old.Signal) ||
		(in.Enabled != nil && *in.Enabled != old.Enabled)
	var activeSilences [][]Matcher
	if resolveFiring {
		policy, policyErr := s.store.AlertPolicySnapshot(ctx, old.Project.ID, s.now().UTC())
		if policyErr != nil {
			return meta.AlertRuleView{}, policyErr
		}
		activeSilences = make([][]Matcher, 0, len(policy.Silences))
		for _, matchers := range policy.Silences {
			activeSilences = append(activeSilences, mapsToMatchers(matchers))
		}
	}
	var enqueuedResolved bool
	update := meta.AlertRuleUpdate{
		Labels:             in.Labels,
		Annotations:        in.Annotations,
		Enabled:            in.Enabled,
		ActivateEvaluation: in.EvaluationEngine == "native" && old.EvaluationEngine == "paused",
		ResolveFiring:      resolveFiring,
		BuildResolveNotifications: func(
			resolvedStates []meta.AlertRuleState,
			enabledChannelIDs []int,
			resolvedAt time.Time,
		) ([]meta.NotificationOutboxEnqueue, error) {
			resolvedAlerts := make([]Alert, 0, len(resolvedStates))
			for _, state := range resolvedStates {
				a := alertFromRuleState(state, "resolved", resolvedAt)
				if !matchesAny(a.Labels, activeSilences) {
					resolvedAlerts = append(resolvedAlerts, a)
				}
			}
			if len(resolvedAlerts) == 0 {
				return nil, nil
			}
			ruleView := old
			ruleView.EnabledChannelIDs = enabledChannelIDs
			entries := nativeOutboxEntries(
				&ruleView,
				&old.Project,
				[]Notification{{
					Project: old.Project.Slug,
					Rule:    old.Name,
					Status:  "resolved",
					Alerts:  resolvedAlerts,
				}},
				resolvedAt,
				s.repeatInterval,
			)
			if len(entries) != 0 {
				enqueuedResolved = true
			}
			return entries, nil
		},
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
	updated, err := s.store.UpdateAlertRule(ctx, in.Project, id, update)
	if err != nil {
		return meta.AlertRuleView{}, err
	}
	if enqueuedResolved {
		s.WakeOutbox()
	}
	return updated, nil
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
	in.Type = NormalizeChannelType(in.Type)
	in.Config = NormalizeChannelConfig(in.Type, in.Config)
	if err := validateChannelConfig(in.Type, in.Config, s.sender.Outbound); err != nil {
		return meta.NotificationChannelView{}, err
	}
	return s.store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: in.Type, Name: in.Name, Config: in.Config, Enabled: in.Enabled,
	})
}

func (s *Service) UpdateChannel(ctx context.Context, id int, in ChannelInput) (meta.NotificationChannelView, error) {
	in.Type = NormalizeChannelType(in.Type)
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
		normalizedIncoming := NormalizeChannelConfig(effectiveType, in.Config)
		normalizedExisting := NormalizeChannelConfig(effectiveType, existing.Config)
		merged, err := preserveRedactedChannelSecrets(normalizedIncoming, normalizedExisting, loadErr)
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
	if err := ValidateMatchers(in.Matchers); err != nil {
		return meta.SilenceView{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if in.StartsAt.IsZero() || !in.EndsAt.After(in.StartsAt) {
		return meta.SilenceView{}, fmt.Errorf("%w: silence ends_at must be after starts_at", ErrInvalidInput)
	}
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
	if in.Matchers != nil {
		if err := ValidateMatchers(in.Matchers); err != nil {
			return meta.SilenceView{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
	}
	if !in.StartsAt.IsZero() || !in.EndsAt.IsZero() {
		existing, err := s.store.SilenceByID(ctx, in.Project, id)
		if err != nil {
			return meta.SilenceView{}, err
		}
		effectiveStart := existing.StartsAt
		if !in.StartsAt.IsZero() {
			effectiveStart = in.StartsAt
		}
		effectiveEnd := existing.EndsAt
		if !in.EndsAt.IsZero() {
			effectiveEnd = in.EndsAt
		}
		if !effectiveEnd.After(effectiveStart) {
			return meta.SilenceView{}, fmt.Errorf("%w: silence ends_at must be after starts_at", ErrInvalidInput)
		}
	}
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
	if err := ValidateMatchers(in.SourceMatchers); err != nil {
		return meta.InhibitionRuleView{}, fmt.Errorf("%w: source_matchers: %w", ErrInvalidInput, err)
	}
	if err := ValidateMatchers(in.TargetMatchers); err != nil {
		return meta.InhibitionRuleView{}, fmt.Errorf("%w: target_matchers: %w", ErrInvalidInput, err)
	}
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
	if in.SourceMatchers != nil {
		if err := ValidateMatchers(in.SourceMatchers); err != nil {
			return meta.InhibitionRuleView{}, fmt.Errorf("%w: source_matchers: %w", ErrInvalidInput, err)
		}
	}
	if in.TargetMatchers != nil {
		if err := ValidateMatchers(in.TargetMatchers); err != nil {
			return meta.InhibitionRuleView{}, fmt.Errorf("%w: target_matchers: %w", ErrInvalidInput, err)
		}
	}
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
		return ProjectAlertConfigView{}, fmt.Errorf("%w: error rate threshold must not be negative", ErrInvalidInput)
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
	s.invalidatePlatformConfigCache(projectID)
	if _, clearErr := s.store.ClearPlatformErrorRateState(ctx, projectID, s.now().UTC()); clearErr != nil {
		slog.Warn("clear platform error rate state after config update", "project_id", projectID, "error", clearErr)
	}
	if _, p, pErr := s.platformConfig(ctx, projectID); pErr == nil {
		s.engine.Remove(p.Slug, "error_rate_spike")
	}
	return cfg, nil
}

// HandleIssueChange injects a committed issue lifecycle event into the same
// grouping, silence and notification engine used by platform-event alerts.
func (s *Service) HandleIssueChange(ctx context.Context, change errortrack.IssueChange) error {
	cfg, p, err := s.cachedPlatformConfig(ctx, change.ProjectID)
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

// RecordErrorEvent uses a durable five-minute sliding window. A spike fires
// when the observed count divided by five minutes is strictly greater than the
// configured absolute per-minute threshold, and resolves when it falls back to
// or below the threshold. Threshold zero disables this event type.
func (s *Service) RecordErrorEvent(ctx context.Context, projectID int, occurredAt time.Time) error {
	cfg, p, err := s.cachedPlatformConfig(ctx, projectID)
	if err != nil {
		return err
	}
	if cfg.ErrorRateThresholdPerMinute == 0 {
		return nil
	}
	now := s.now().UTC()
	transition, err := s.store.RecordPlatformErrorRate(
		ctx,
		projectID,
		occurredAt,
		now,
		errorRateWindow,
		cfg.ErrorRateThresholdPerMinute,
	)
	if err != nil {
		return err
	}
	switch transition.Transition {
	case "firing":
		rate := float64(transition.Count) / errorRateWindow.Minutes()
		s.engine.Add(
			ctx,
			p.Slug,
			"error_rate_spike",
			Alert{Status: "firing", Labels: platformLabels(p.Slug, "error_rate_spike"), Annotations: map[string]string{
				"summary":           fmt.Sprintf("Error rate spike: %.1f events/min", rate),
				"window":            "5m",
				"threshold_per_min": strconv.Itoa(cfg.ErrorRateThresholdPerMinute),
				"cycle":             strconv.FormatInt(transition.Cycle, 10),
			}, StartsAt: transition.StartsAt},
		)
	case "resolved":
		rate := float64(transition.Count) / errorRateWindow.Minutes()
		s.engine.Add(
			ctx,
			p.Slug,
			"error_rate_spike",
			Alert{
				Status: "resolved",
				Labels: platformLabels(p.Slug, "error_rate_spike"),
				Annotations: map[string]string{
					"summary":           fmt.Sprintf("Error rate spike recovered: %.1f events/min", rate),
					"window":            "5m",
					"threshold_per_min": strconv.Itoa(cfg.ErrorRateThresholdPerMinute),
					"cycle":             strconv.FormatInt(transition.Cycle, 10),
				},
				StartsAt: transition.StartsAt,
				EndsAt:   transition.EndsAt,
			},
		)
	}
	return nil
}

// CheckErrorRateRecoveries evaluates durable firing error_rate_spike states and
// emits resolved alerts once their 5-minute window drops back within threshold.
func (s *Service) CheckErrorRateRecoveries(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	recoveries, err := s.store.CheckPlatformErrorRateRecoveries(ctx, s.now().UTC(), errorRateWindow)
	if err != nil {
		return err
	}
	for _, rec := range recoveries {
		rate := float64(rec.Count) / errorRateWindow.Minutes()
		s.engine.Add(
			ctx,
			rec.ProjectSlug,
			"error_rate_spike",
			Alert{
				Status: "resolved",
				Labels: platformLabels(rec.ProjectSlug, "error_rate_spike"),
				Annotations: map[string]string{
					"summary":           fmt.Sprintf("Error rate spike recovered: %.1f events/min", rate),
					"window":            "5m",
					"threshold_per_min": strconv.Itoa(rec.Threshold),
					"cycle":             strconv.FormatInt(rec.Cycle, 10),
				},
				StartsAt: rec.StartsAt,
				EndsAt:   rec.EndsAt,
			},
		)
	}
	return nil
}

func (s *Service) runPlatformErrorRateMonitor(ctx context.Context) {
	defer s.monitorWG.Done()
	ticker := time.NewTicker(platformRecoveryCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.CheckErrorRateRecoveries(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("check platform error rate recoveries", "error", err)
			}
		}
	}
}

func (s *Service) cachedPlatformConfig(
	ctx context.Context,
	projectID int,
) (meta.ProjectAlertConfigView, meta.ProjectInfo, error) {
	now := s.now().UTC()
	s.cfgCacheMu.RLock()
	entry, ok := s.cfgCache[projectID]
	s.cfgCacheMu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.cfg, entry.project, nil
	}
	cfg, proj, err := s.platformConfig(ctx, projectID)
	if err != nil {
		return meta.ProjectAlertConfigView{}, meta.ProjectInfo{}, err
	}
	s.cfgCacheMu.Lock()
	s.cfgCache[projectID] = cachedPlatformConfig{
		cfg:       cfg,
		project:   proj,
		expiresAt: now.Add(platformConfigCacheTTL),
	}
	s.cfgCacheMu.Unlock()
	return cfg, proj, nil
}

func (s *Service) invalidatePlatformConfigCache(projectID int) {
	s.cfgCacheMu.Lock()
	delete(s.cfgCache, projectID)
	s.cfgCacheMu.Unlock()
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
	payload, marshalErr := json.Marshal(n)
	if marshalErr != nil {
		return fmt.Errorf("encode platform notification: %w", marshalErr)
	}
	// Include rule, status, and repeat generation in the idempotency key so
	// concurrent replicas deduplicate the same event while repeat deliveries
	// and subsequent spike cycles are never permanently swallowed.
	keyPayload := append(slices.Clone(payload), []byte(fmt.Sprintf("\x00platform\x00%s\x00%s", n.Rule, n.Status))...)
	if n.Status == "firing" && s.repeatInterval > 0 && len(n.Alerts) > 0 && !n.Alerts[0].StartsAt.IsZero() {
		elapsed := s.now().UTC().Sub(n.Alerts[0].StartsAt.UTC())
		var generation int64
		if elapsed > 0 {
			generation = elapsed.Nanoseconds() / s.repeatInterval.Nanoseconds()
		}
		keyPayload = append(keyPayload, []byte(fmt.Sprintf("\x00%d", generation))...)
	}
	var errs []error
	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		priority := 0
		if n.Status == "resolved" {
			priority = 100
		}
		_, inserted, enqueueErr := s.store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
			IdempotencyKey: notificationOutboxKey(ch.ID, keyPayload),
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
