package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert/matcher"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/state"
	prommodel "github.com/prometheus/common/model"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// ErrInvalidChannelConfig reports a rejected notification channel configuration.
var ErrInvalidChannelConfig = errors.New("invalid notification channel config")

// ErrInvalidInput reports a rejected alert rule, silence, inhibition, or config input.
var ErrInvalidInput = errors.New("invalid alert input")

const (
	platformConfigCacheTTL        = 5 * time.Second
	platformRecoveryCheckInterval = 5 * time.Second
	maxRuleExprLength             = 8192

	MinMetricsEvaluationInterval = 5 * time.Second
	MinLogsEvaluationInterval    = 30 * time.Second
	MaxEvaluationInterval        = time.Hour
	EvaluationIntervalStep       = 5 * time.Second

	// DefaultRepeatInterval is the global alert.repeat_interval fallback used
	// when the server config leaves it unset. Native rules may override it per
	// rule within [MinRepeatInterval, MaxRepeatInterval].
	DefaultRepeatInterval = time.Hour
	MinRepeatInterval     = time.Minute
	MaxRepeatInterval     = 7 * 24 * time.Hour
)

type cachedPlatformConfig struct {
	cfg       meta.ProjectAlertConfigView
	project   meta.ProjectInfo
	expiresAt time.Time
}

// AlertDiagnostics aggregates runtime observability across the native rule
// evaluator, durable notification outbox, and compiled matcher regex cache.
type AlertDiagnostics struct {
	Evaluator    EvaluatorSnapshot  `json:"evaluator"`
	Outbox       OutboxSnapshot     `json:"outbox"`
	MatcherCache matcher.CacheStats `json:"matcher_cache"`
}

type Config struct {
	Store           *meta.Store
	Sender          *Sender
	OutboundPolicy  outbound.Policy
	ExternalURL     string
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
	now            func() time.Time
	repeatInterval time.Duration
	stormThreshold int
	cfgCacheMu     sync.RWMutex
	cfgCache       map[int]cachedPlatformConfig
	outbox         *OutboxWorker
	evaluator      *RuleEvaluator
	monitorCancel  context.CancelFunc
	monitorWG      sync.WaitGroup
}

func NewService(cfg Config) *Service {
	if cfg.Sender == nil {
		cfg.Sender = &Sender{Outbound: cfg.OutboundPolicy, ExternalURL: cfg.ExternalURL}
	} else {
		if !cfg.Sender.Outbound.AllowPrivate && len(cfg.Sender.Outbound.AllowedCIDRs) == 0 &&
			len(cfg.Sender.Outbound.AllowedHosts) == 0 && len(cfg.Sender.Outbound.BlockedPorts) == 0 {
			cfg.Sender.Outbound = cfg.OutboundPolicy
		}
		if cfg.Sender.ExternalURL == "" {
			cfg.Sender.ExternalURL = cfg.ExternalURL
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RepeatInterval <= 0 {
		cfg.RepeatInterval = DefaultRepeatInterval
	}
	stormThreshold := cfg.StormThreshold
	if stormThreshold <= 0 {
		stormThreshold = defaultEvaluatorStormThreshold
	}
	s := &Service{
		store:          cfg.Store,
		sender:         cfg.Sender,
		now:            cfg.Now,
		repeatInterval: cfg.RepeatInterval,
		stormThreshold: stormThreshold,
		cfgCache:       make(map[int]cachedPlatformConfig),
	}
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
	s.evaluator.Close()
	s.outbox.Close()
}

// WakeOutbox nudges the outbox worker to claim newly pending notifications.
func (s *Service) WakeOutbox() {
	if s != nil && s.outbox != nil {
		s.outbox.Wake()
	}
}

// RuleLastError returns the sanitized latest evaluation error for ruleID, if any.
func (s *Service) RuleLastError(ruleID int) string {
	if s == nil || s.evaluator == nil {
		return ""
	}
	return s.evaluator.RuleLastError(ruleID)
}

// RuleStatus returns the latest runtime evaluation status for ruleID.
func (s *Service) RuleStatus(ruleID int) (RuleEvaluationStatus, bool) {
	if s == nil || s.evaluator == nil {
		return RuleEvaluationStatus{}, false
	}
	return s.evaluator.RuleStatus(ruleID)
}

// Diagnostics returns live runtime observability across evaluator, outbox, and
// compiled matcher regex cache.
func (s *Service) Diagnostics(ctx context.Context) (AlertDiagnostics, error) {
	diag := AlertDiagnostics{
		MatcherCache: matcher.DefaultCacheStats(),
	}
	if s == nil {
		return diag, nil
	}
	if s.evaluator != nil {
		diag.Evaluator = s.evaluator.Snapshot()
	}
	if s.outbox != nil {
		outboxSnap, err := s.outbox.Snapshot(ctx)
		if err != nil {
			return diag, err
		}
		diag.Outbox = outboxSnap
	} else if s.store != nil {
		depth, err := s.store.NotificationOutboxDepth(ctx)
		if err != nil {
			return diag, err
		}
		diag.Outbox.Depth = depth
	}
	return diag, nil
}

type RuleInput struct {
	Project            string            `json:"project"`
	Name               string            `json:"name"`
	Signal             string            `json:"signal"`
	EvaluationEngine   string            `json:"evaluation_engine"`
	Expr               string            `json:"expr"`
	For                string            `json:"for"`
	EvaluationInterval *string           `json:"evaluation_interval"`
	RepeatInterval     *string           `json:"repeat_interval"`
	Severity           string            `json:"severity"`
	Labels             map[string]string `json:"labels"`
	Annotations        map[string]string `json:"annotations"`
	Enabled            *bool             `json:"enabled"`
	ChannelIDs         []int             `json:"channel_ids"`
}

// NormalizeRuleEvaluationInterval validates and canonicalizes a per-rule
// evaluation interval for signal. Empty/whitespace input returns "" to indicate
// the signal default.
func NormalizeRuleEvaluationInterval(signal, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	effectiveSignal := strings.TrimSpace(signal)
	if effectiveSignal == "" {
		effectiveSignal = "metrics"
	}
	var minInterval time.Duration
	switch effectiveSignal {
	case "metrics":
		minInterval = MinMetricsEvaluationInterval
	case "logs":
		minInterval = MinLogsEvaluationInterval
	default:
		return "", fmt.Errorf("%w: unsupported alert signal %q", ErrInvalidInput, signal)
	}
	parsed, err := prommodel.ParseDuration(trimmed)
	if err != nil {
		return "", fmt.Errorf(
			"%w: invalid alert evaluation_interval %q: must be a duration such as 30s or 1m",
			ErrInvalidInput,
			trimmed,
		)
	}
	d := time.Duration(parsed)
	if d <= 0 || d < minInterval || d > MaxEvaluationInterval {
		return "", fmt.Errorf(
			"%w: alert evaluation_interval for %s rules must be between %s and %s",
			ErrInvalidInput,
			effectiveSignal,
			prommodel.Duration(minInterval).String(),
			prommodel.Duration(MaxEvaluationInterval).String(),
		)
	}
	if d%EvaluationIntervalStep != 0 {
		return "", fmt.Errorf("%w: alert evaluation_interval %q must be a multiple of 5s", ErrInvalidInput, trimmed)
	}
	return prommodel.Duration(d).String(), nil
}

// NormalizeRuleRepeatInterval validates and canonicalizes a per-rule
// repeat_interval. Empty/whitespace input returns "" so the rule inherits the
// global alert.repeat_interval. Accepted values are whole-second Prometheus
// durations within [MinRepeatInterval, MaxRepeatInterval]; the canonical form
// uses d/h/m/s units only (for example "60m" -> "1h", "168h" -> "7d").
func NormalizeRuleRepeatInterval(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := prommodel.ParseDuration(trimmed)
	if err != nil {
		return "", fmt.Errorf(
			"%w: invalid alert repeat_interval %q: must be a duration such as 30m, 1h or 1d",
			ErrInvalidInput,
			trimmed,
		)
	}
	d := time.Duration(parsed)
	if d < MinRepeatInterval || d > MaxRepeatInterval {
		return "", fmt.Errorf(
			"%w: alert repeat_interval must be between %s and %s",
			ErrInvalidInput,
			FormatRepeatInterval(MinRepeatInterval),
			FormatRepeatInterval(MaxRepeatInterval),
		)
	}
	if d%time.Second != 0 {
		return "", fmt.Errorf(
			"%w: alert repeat_interval %q must be a whole number of seconds",
			ErrInvalidInput,
			trimmed,
		)
	}
	return FormatRepeatInterval(d), nil
}

// FormatRepeatInterval renders d as a Prometheus-compatible duration using
// only d/h/m/s units so repeat intervals never surface as weeks (e.g. "1w").
func FormatRepeatInterval(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, unit := range []struct {
		suffix string
		size   time.Duration
	}{
		{"d", 24 * time.Hour},
		{"h", time.Hour},
		{"m", time.Minute},
		{"s", time.Second},
	} {
		if n := d / unit.size; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10))
			b.WriteString(unit.suffix)
			d %= unit.size
		}
	}
	if d >= time.Millisecond {
		b.WriteString(strconv.FormatInt(d.Milliseconds(), 10))
		b.WriteString("ms")
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}

// effectiveRuleRepeatInterval returns the per-rule repeat override when raw is
// a valid positive duration, or fallback otherwise. invalid reports whether a
// non-empty raw value failed to parse.
func effectiveRuleRepeatInterval(raw string, fallback time.Duration) (d time.Duration, invalid bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback, false
	}
	parsed, err := prommodel.ParseDuration(trimmed)
	if err != nil || time.Duration(parsed) <= 0 {
		return fallback, true
	}
	return time.Duration(parsed), false
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

// ValidateRuleExpr validates a rule expression according to its signal type.
// Metrics rules are parsed with the PromQL parser; logs rules verify non-empty
// bounded LogsQL input without control characters.
func ValidateRuleExpr(signal, expr string) error {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return fmt.Errorf("%w: alert expression is required", ErrInvalidInput)
	}
	if len(trimmed) > maxRuleExprLength {
		return fmt.Errorf("%w: alert expression exceeds maximum length of %d bytes", ErrInvalidInput, maxRuleExprLength)
	}
	for _, r := range trimmed {
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			return fmt.Errorf("%w: alert expression contains invalid control characters", ErrInvalidInput)
		}
	}
	if signal == "" || signal == "metrics" {
		if _, err := promparser.NewParser(promparser.Options{}).ParseExpr(trimmed); err != nil {
			return fmt.Errorf("%w: invalid PromQL expression: %s", ErrInvalidInput, err.Error())
		}
		return nil
	}
	if signal == "logs" {
		return nil
	}
	return fmt.Errorf("%w: unsupported alert signal %q", ErrInvalidInput, signal)
}

// ValidateRuleLabels ensures custom rule label keys are non-empty.
func ValidateRuleLabels(labels map[string]string) error {
	for k := range labels {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: alert label name must not be empty", ErrInvalidInput)
		}
	}
	return nil
}

// NormalizeRuleSeverity validates a native rule severity against the
// critical/warning/info enum shared with the Web console. Empty or
// whitespace-only input defaults to warning.
func NormalizeRuleSeverity(raw string) (string, error) {
	switch severity := strings.TrimSpace(raw); severity {
	case "":
		return "warning", nil
	case "critical", "warning", "info":
		return severity, nil
	default:
		return "", fmt.Errorf(
			"%w: unsupported alert severity %q: must be critical, warning or info",
			ErrInvalidInput,
			raw,
		)
	}
}

// ValidateChannelType verifies that kind (or its canonical alias) is a
// supported notification channel type.
func ValidateChannelType(kind string) error {
	switch NormalizeChannelType(strings.TrimSpace(kind)) {
	case "webhook", "feishu", "dingtalk", "wecom", "smtp", "slack":
		return nil
	default:
		return fmt.Errorf("%w: unsupported channel type %q", ErrInvalidInput, kind)
	}
}

// NormalizeChannelType maps legacy aliases (such as "email") to canonical
// channel types.
func NormalizeChannelType(v string) string {
	v = strings.TrimSpace(v)
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

// EffectiveEvaluationInterval returns the Prometheus duration string used by
// the native rule evaluator for rule, falling back to the signal default when
// no per-rule override is configured.
func (s *Service) EffectiveEvaluationInterval(rule meta.AlertRuleView) string {
	var eval *RuleEvaluator
	if s != nil {
		eval = s.evaluator
	}
	return prommodel.Duration(eval.ruleInterval(&rule)).String()
}

// EffectiveRepeatInterval returns the repeat_interval the native rule evaluator
// applies to rule: its per-rule override, or the global alert.repeat_interval
// when the override is empty.
func (s *Service) EffectiveRepeatInterval(rule meta.AlertRuleView) string {
	fallback := DefaultRepeatInterval
	if s != nil && s.repeatInterval > 0 {
		fallback = s.repeatInterval
	}
	d, _ := effectiveRuleRepeatInterval(rule.RepeatInterval, fallback)
	return FormatRepeatInterval(d)
}

func (s *Service) ListRules(ctx context.Context, slug string) ([]meta.AlertRuleView, error) {
	return s.store.ListAlertRules(ctx, slug)
}

func (s *Service) CreateRule(ctx context.Context, in RuleInput) (meta.AlertRuleView, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return meta.AlertRuleView{}, fmt.Errorf("%w: alert rule name is required", ErrInvalidInput)
	}
	if in.EvaluationEngine != "" && in.EvaluationEngine != "native" {
		return meta.AlertRuleView{}, fmt.Errorf(
			"%w: unsupported alert evaluation engine %q",
			ErrInvalidInput,
			in.EvaluationEngine,
		)
	}
	signal := defaultString(strings.TrimSpace(in.Signal), "metrics")
	if signal != "metrics" && signal != "logs" {
		return meta.AlertRuleView{}, fmt.Errorf("%w: unsupported alert signal %q", ErrInvalidInput, in.Signal)
	}
	expr := strings.TrimSpace(in.Expr)
	if err := ValidateRuleExpr(signal, expr); err != nil {
		return meta.AlertRuleView{}, err
	}
	forDuration := defaultString(strings.TrimSpace(in.For), "0s")
	if err := ValidateRuleFor(forDuration); err != nil {
		return meta.AlertRuleView{}, err
	}
	evaluationInterval := ""
	if in.EvaluationInterval != nil {
		var normErr error
		evaluationInterval, normErr = NormalizeRuleEvaluationInterval(signal, *in.EvaluationInterval)
		if normErr != nil {
			return meta.AlertRuleView{}, normErr
		}
	}
	repeatInterval := ""
	if in.RepeatInterval != nil {
		var normErr error
		repeatInterval, normErr = NormalizeRuleRepeatInterval(*in.RepeatInterval)
		if normErr != nil {
			return meta.AlertRuleView{}, normErr
		}
	}
	if err := ValidateRuleLabels(in.Labels); err != nil {
		return meta.AlertRuleView{}, err
	}
	severity, err := NormalizeRuleSeverity(in.Severity)
	if err != nil {
		return meta.AlertRuleView{}, err
	}
	if err := ValidateRuleAnnotations(in.Annotations); err != nil {
		return meta.AlertRuleView{}, err
	}
	return s.store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug:        in.Project,
		Name:               name,
		Signal:             signal,
		Expression:         expr,
		For:                forDuration,
		EvaluationInterval: evaluationInterval,
		RepeatInterval:     repeatInterval,
		Severity:           severity,
		Labels:             nonNilMap(in.Labels),
		Annotations:        nonNilMap(in.Annotations),
		Enabled:            in.Enabled,
		ChannelIDs:         in.ChannelIDs,
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
	trimmedSignal := strings.TrimSpace(in.Signal)
	if trimmedSignal != "" && trimmedSignal != "metrics" && trimmedSignal != "logs" {
		return meta.AlertRuleView{}, fmt.Errorf("%w: unsupported alert signal %q", ErrInvalidInput, in.Signal)
	}
	if in.For != "" {
		if err := ValidateRuleFor(strings.TrimSpace(in.For)); err != nil {
			return meta.AlertRuleView{}, err
		}
	}
	if in.Labels != nil {
		if err := ValidateRuleLabels(in.Labels); err != nil {
			return meta.AlertRuleView{}, err
		}
	}
	if err := ValidateRuleAnnotations(in.Annotations); err != nil {
		return meta.AlertRuleView{}, err
	}
	// Severity is a plain string, so "" (or whitespace) means "keep the current
	// value" on update; only Create applies the warning default.
	var updatedSeverity *string
	if strings.TrimSpace(in.Severity) != "" {
		severity, err := NormalizeRuleSeverity(in.Severity)
		if err != nil {
			return meta.AlertRuleView{}, err
		}
		updatedSeverity = &severity
	}
	// repeat_interval only changes notification cadence: like evaluation_interval
	// it never resets firing state, so the next evaluation applies it at once.
	var normalizedRepeatInterval *string
	if in.RepeatInterval != nil {
		norm, normErr := NormalizeRuleRepeatInterval(*in.RepeatInterval)
		if normErr != nil {
			return meta.AlertRuleView{}, normErr
		}
		normalizedRepeatInterval = &norm
	}
	if in.Name != "" && strings.TrimSpace(in.Name) == "" {
		return meta.AlertRuleView{}, fmt.Errorf("%w: alert rule name must not be blank", ErrInvalidInput)
	}
	old, err := s.store.AlertRule(ctx, in.Project, id)
	if err != nil {
		return meta.AlertRuleView{}, err
	}
	effectiveSignal := old.Signal
	if trimmedSignal != "" {
		effectiveSignal = trimmedSignal
	}
	effectiveExpr := old.Expression
	if in.Expr != "" {
		effectiveExpr = strings.TrimSpace(in.Expr)
	}
	if in.Expr != "" || trimmedSignal != "" {
		if err := ValidateRuleExpr(effectiveSignal, effectiveExpr); err != nil {
			return meta.AlertRuleView{}, err
		}
	}
	var normalizedEvalInterval *string
	if in.EvaluationInterval != nil {
		norm, normErr := NormalizeRuleEvaluationInterval(effectiveSignal, *in.EvaluationInterval)
		if normErr != nil {
			return meta.AlertRuleView{}, normErr
		}
		normalizedEvalInterval = &norm
	} else if trimmedSignal != "" && trimmedSignal != old.Signal && old.EvaluationInterval != "" {
		if _, normErr := NormalizeRuleEvaluationInterval(effectiveSignal, old.EvaluationInterval); normErr != nil {
			return meta.AlertRuleView{}, normErr
		}
	}
	resolveFiring := (in.Expr != "" && effectiveExpr != old.Expression) ||
		(trimmedSignal != "" && trimmedSignal != old.Signal) ||
		(in.Enabled != nil && *in.Enabled != old.Enabled)
	var activeSilences []matcher.Set
	if resolveFiring {
		policy, policyErr := s.store.AlertPolicySnapshot(ctx, old.Project.ID, s.now().UTC())
		if policyErr != nil {
			return meta.AlertRuleView{}, policyErr
		}
		activeSilences = matcher.CompileSetsFromMapsLenient(policy.Silences)
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
			candidates := make([]AlertCandidate, 0, len(resolvedStates))
			for _, st := range resolvedStates {
				candidates = append(candidates, AlertCandidate{
					Alert: alertFromRuleState(st, "resolved", resolvedAt),
				})
			}
			ruleID := old.ID
			plan := PlanNotifications(PlanConfig{
				ProjectSlug:    old.Project.Slug,
				ProjectID:      old.Project.ID,
				RuleName:       old.Name,
				RuleID:         &ruleID,
				GroupKey:       nativeGroupKey(old.ID),
				RepeatInterval: s.repeatInterval,
				Now:            resolvedAt,
			}, CompiledPolicy{Silences: activeSilences}, candidates)
			if len(plan.Notifications) == 0 {
				return nil, nil
			}
			entries := BuildOutboxEntries(
				old.Project.ID,
				&ruleID,
				enabledChannelIDs,
				plan.Notifications,
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
		trimmedName := strings.TrimSpace(in.Name)
		update.Name = &trimmedName
	}
	if trimmedSignal != "" {
		update.Signal = &trimmedSignal
	}
	if in.Expr != "" {
		update.Expression = &effectiveExpr
	}
	if in.For != "" {
		trimmedFor := strings.TrimSpace(in.For)
		update.For = &trimmedFor
	}
	if normalizedEvalInterval != nil {
		update.EvaluationInterval = normalizedEvalInterval
	}
	update.RepeatInterval = normalizedRepeatInterval
	update.Severity = updatedSeverity
	if in.ChannelIDs != nil {
		update.ChannelIDs = &in.ChannelIDs
	}
	updated, err := s.store.UpdateAlertRule(ctx, in.Project, id, update)
	if err != nil {
		return meta.AlertRuleView{}, err
	}
	if s.evaluator != nil {
		s.evaluator.ClearRuleStatus(id)
	}
	if enqueuedResolved {
		s.WakeOutbox()
	}
	return updated, nil
}

func (s *Service) DeleteRule(ctx context.Context, projectSlug string, id int) error {
	if err := s.store.DeleteAlertRule(ctx, projectSlug, id); err != nil {
		return err
	}
	if s.evaluator != nil {
		s.evaluator.ClearRuleStatus(id)
	}
	return nil
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
	if err := ValidateChannelType(in.Type); err != nil {
		return meta.NotificationChannelView{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return meta.NotificationChannelView{}, fmt.Errorf("%w: notification channel name is required", ErrInvalidInput)
	}
	in.Type = NormalizeChannelType(in.Type)
	in.Name = name
	in.Config = NormalizeChannelConfig(in.Type, in.Config)
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	if err := validateChannelConfig(in.Type, in.Config, s.sender.Outbound); err != nil {
		return meta.NotificationChannelView{}, err
	}
	return s.store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: in.Type, Name: in.Name, Config: in.Config, Enabled: in.Enabled,
	})
}

func (s *Service) UpdateChannel(ctx context.Context, id int, in ChannelInput) (meta.NotificationChannelView, error) {
	if in.Type != "" {
		if err := ValidateChannelType(in.Type); err != nil {
			return meta.NotificationChannelView{}, err
		}
		in.Type = NormalizeChannelType(in.Type)
	}
	if in.Name != "" && strings.TrimSpace(in.Name) == "" {
		return meta.NotificationChannelView{}, fmt.Errorf(
			"%w: notification channel name must not be blank",
			ErrInvalidInput,
		)
	}
	update := meta.NotificationChannelUpdate{Enabled: in.Enabled}
	if in.Type != "" {
		update.Type = &in.Type
	}
	if in.Name != "" {
		trimmedName := strings.TrimSpace(in.Name)
		update.Name = &trimmedName
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
			Test:    true,
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
	Comment  *string   `json:"comment"`
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
	comment := ""
	if in.Comment != nil {
		comment = *in.Comment
	}
	return s.store.CreateSilence(ctx, meta.SilenceCreate{
		ProjectSlug: in.Project,
		Matchers:    matchersToMaps(in.Matchers),
		StartsAt:    in.StartsAt,
		EndsAt:      in.EndsAt,
		Comment:     comment,
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
	if in.Comment != nil {
		update.Comment = in.Comment
	}
	silence, err := s.store.UpdateSilence(ctx, in.Project, id, update)
	if err == nil {
		s.replayProjectPlatformAlertStates(ctx, silence.ProjectID)
	}
	return silence, err
}

func (s *Service) DeleteSilence(ctx context.Context, projectSlug string, id int) error {
	if err := s.store.DeleteSilence(ctx, projectSlug, id); err != nil {
		return err
	}
	p, err := s.store.ProjectBySlug(ctx, projectSlug)
	if err != nil {
		slog.Warn(
			"lookup project for platform alert replay after silence delete",
			"project", projectSlug,
			"silence_id", id,
			"error", err,
		)
		return nil
	}
	s.replayProjectPlatformAlertStates(ctx, p.ID)
	return nil
}

func (s *Service) ListEvents(
	ctx context.Context,
	slug, status string,
	inhibited *bool,
) ([]meta.AlertEventView, error) {
	return s.store.ListAlertEvents(ctx, slug, status, inhibited)
}

func (s *Service) platformPolicy(
	ctx context.Context,
	slug string,
	now time.Time,
	localSources []Alert,
	localGroups []GroupSummary,
	excludeGroupKey string,
) (CompiledPolicy, error) {
	if s.store == nil {
		return CompilePolicy(nil, nil, localSources, localGroups), nil
	}
	projectInfo, err := s.store.ProjectBySlug(ctx, slug)
	if err != nil {
		return CompiledPolicy{}, err
	}
	policySnap, err := s.store.AlertPolicySnapshot(ctx, projectInfo.ID, now)
	if err != nil {
		return CompiledPolicy{}, err
	}
	firingRules, err := s.store.ProjectFiringAlertRuleStates(ctx, projectInfo.ID)
	if err != nil {
		return CompiledPolicy{}, err
	}
	firingPlatform, err := s.store.ProjectFiringPlatformAlertStates(ctx, projectInfo.ID)
	if err != nil {
		return CompiledPolicy{}, err
	}
	return CompilePolicyFromMeta(
		policySnap,
		firingRules,
		firingPlatform,
		localSources,
		localGroups,
		excludeGroupKey,
	), nil
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
	compiled, err := matcher.CompileInhibitionRule(in.SourceMatchers, in.TargetMatchers, in.EqualLabels)
	if err != nil {
		return meta.InhibitionRuleView{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	rule, err := s.store.CreateInhibitionRule(ctx, meta.InhibitionRuleCreate{
		ProjectSlug:    in.Project,
		SourceMatchers: matchersToMaps(in.SourceMatchers),
		TargetMatchers: matchersToMaps(in.TargetMatchers),
		EqualLabels:    nonNilStrings(compiled.EqualLabels),
		Enabled:        in.Enabled,
		Comment:        in.Comment,
	})
	if err == nil {
		err = s.recomputeInhibited(ctx, in.Project)
		s.replayProjectPlatformAlertStates(ctx, rule.ProjectID)
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
	var cleanedEqualLabels []string
	if in.EqualLabels != nil {
		cleanedEqualLabels = make([]string, 0, len(in.EqualLabels))
		for i, label := range in.EqualLabels {
			trimmed := strings.TrimSpace(label)
			if trimmed == "" {
				return meta.InhibitionRuleView{}, fmt.Errorf(
					"%w: equal_labels[%d] must not be empty",
					ErrInvalidInput,
					i,
				)
			}
			cleanedEqualLabels = append(cleanedEqualLabels, trimmed)
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
		update.EqualLabels = cleanedEqualLabels
	}
	if in.Comment != "" {
		update.Comment = &in.Comment
	}
	rule, err := s.store.UpdateInhibitionRule(ctx, in.Project, id, update)
	if err == nil {
		err = s.recomputeInhibited(ctx, rule.ProjectSlug)
		s.replayProjectPlatformAlertStates(ctx, rule.ProjectID)
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
	s.replayProjectPlatformAlertStates(ctx, projectInfo.ID)
	return nil
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
	firingPlatform, err := s.store.ProjectFiringPlatformAlertStates(ctx, projectInfo.ID)
	if err != nil {
		return err
	}
	compiled := CompilePolicyFromMeta(policy, nil, firingPlatform, nil, nil, "")
	updates := make([]meta.AlertEventInhibition, 0, len(policy.FiringEvents))
	for _, event := range policy.FiringEvents {
		inhibited := matcher.IsInhibited(
			matcher.AlertRef{Fingerprint: event.Fingerprint, Status: "firing", Labels: event.Labels},
			compiled.Sources,
			compiled.Inhibitions,
		)
		if event.Inhibited != inhibited {
			updates = append(updates, meta.AlertEventInhibition{EventID: event.ID, Inhibited: inhibited})
		}
	}
	return s.store.SetAlertEventInhibition(ctx, updates)
}

func (s *Service) enqueueOutboxEntries(ctx context.Context, entries []meta.NotificationOutboxEnqueue) error {
	var errs []error
	wake := false
	for _, entry := range entries {
		_, inserted, enqueueErr := s.store.EnqueueNotification(ctx, entry)
		if enqueueErr != nil {
			errs = append(errs, fmt.Errorf("enqueue notification for channel %d: %w", entry.ChannelID, enqueueErr))
			continue
		}
		if inserted {
			wake = true
		}
	}
	if wake {
		s.WakeOutbox()
	}
	return errors.Join(errs...)
}

func (s *Service) notifyStorm(ctx context.Context, n Notification) error {
	channels, projectInfo, err := s.store.StormNotificationChannels(ctx, n.Project)
	if err != nil {
		return err
	}
	channelIDs := make([]int, 0, len(channels))
	for _, ch := range channels {
		if ch.Enabled {
			channelIDs = append(channelIDs, ch.ID)
		}
	}
	entries := BuildOutboxEntries(
		projectInfo.ID,
		nil,
		channelIDs,
		[]Notification{n},
		s.now().UTC(),
		s.repeatInterval,
	)
	return s.enqueueOutboxEntries(ctx, entries)
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
	transition, clearErr := s.store.ClearPlatformErrorRateState(ctx, projectID, s.now().UTC())
	if clearErr != nil {
		slog.Warn("clear platform error rate state after config update", "project_id", projectID, "error", clearErr)
	} else if transition.Transition == "resolved" {
		// Close the open cycle durably so receivers are not left with a dangling
		// firing incident; a disabled threshold suppresses it in
		// enqueuePlatformNotification. The resolution is logged durably with the
		// state change, so a failed drain is retried by the recovery loop.
		if drainErr := s.drainPlatformAlertTransitions(ctx, projectID); drainErr != nil {
			slog.Warn(
				"drain platform alert transitions after config update",
				"project_id", projectID,
				"error", drainErr,
			)
		}
	}
	return cfg, nil
}

// HandleIssueChange evaluates silence, inhibition, and storm policy through
// the unified compiled planner for a committed issue lifecycle event and
// durably enqueues matching platform notifications into notification_outbox
// before returning nil. Returning only after the outbox write commits
// eliminates the crash window where an in-memory GroupWait timer could be lost
// after issue_lifecycle_events was marked dispatched (ADR 0007).
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
	alertItem := Alert{
		Status: "firing",
		Labels: labels,
		Annotations: map[string]string{
			"summary": change.Title,
			"title":   change.Title,
			"issue":   "/issues/" + strconv.Itoa(change.IssueID),
			"link":    "/issues/" + strconv.Itoa(change.IssueID),
		},
		StartsAt:  change.SeenAt,
		Transient: true,
	}
	alertItem.Fingerprint = Fingerprint(alertItem.Labels)
	now := s.now().UTC()
	groupKey := platformGroupKey(change.Kind)
	compiledPolicy, err := s.platformPolicy(ctx, p.Slug, now, nil, nil, groupKey)
	if err != nil {
		return err
	}
	plan := PlanNotifications(PlanConfig{
		ProjectSlug:    p.Slug,
		ProjectID:      p.ID,
		RuleName:       change.Kind,
		GroupKey:       groupKey,
		RepeatInterval: s.repeatInterval,
		StormThreshold: s.stormThreshold,
		Now:            now,
	}, compiledPolicy, []AlertCandidate{{Alert: alertItem, DueOverride: true}})
	if len(plan.Notifications) == 0 {
		return nil
	}
	var errs []error
	for _, notification := range plan.Notifications {
		if notification.Storm != nil {
			if stormErr := s.notifyStorm(ctx, notification); stormErr != nil {
				errs = append(errs, stormErr)
			}
			continue
		}
		if notifyErr := s.notifyPlatformLifecycle(ctx, notification, change.LifecycleID); notifyErr != nil {
			errs = append(errs, notifyErr)
		}
	}
	return errors.Join(errs...)
}

// RecordErrorEvent uses a durable five-minute sliding window. A spike fires
// when the observed count divided by five minutes is strictly greater than the
// configured absolute per-minute threshold, and resolves when it falls back to
// or below the threshold. Threshold zero disables this event type.
//
// A firing or resolved transition commits to platform_alert_states together
// with a platform_alert_transitions row, and is planned and written to
// notification_outbox before returning, keyed by project_id + error_rate_spike
// + cycle + status (ADR 0001). There is no in-process group_wait timer: the
// five-minute window already debounces the signal, and a transition whose
// enqueue was lost to a crash or a failed write stays logged and is drained by
// CheckErrorRateRecoveries on any replica, even if the state has since moved
// on (issue #386).
func (s *Service) RecordErrorEvent(ctx context.Context, projectID int, occurredAt time.Time) error {
	return s.RecordErrorEvents(ctx, []errortrack.ErrorOccurrence{{ProjectID: projectID, OccurredAt: occurredAt}})
}

// RecordErrorEvents applies the RecordErrorEvent rule to a whole completed
// error-inbox batch with a single metadata write transaction (issue #375).
// Occurrences are grouped by project; projects whose threshold is zero are
// skipped. The store logs every firing/resolved transition in
// platform_alert_transitions inside that transaction, in the order the
// equivalent sequence of single-event records would have produced them, and
// each project that moved then has its transition log drained into
// notification_outbox. A per-project config failure skips only that project;
// all failures are joined into the returned error so the caller can log them.
func (s *Service) RecordErrorEvents(ctx context.Context, occurrences []errortrack.ErrorOccurrence) error {
	if len(occurrences) == 0 {
		return nil
	}
	byProject := make(map[int][]time.Time)
	for _, occurrence := range occurrences {
		byProject[occurrence.ProjectID] = append(byProject[occurrence.ProjectID], occurrence.OccurredAt)
	}
	projectIDs := make([]int, 0, len(byProject))
	for projectID := range byProject {
		projectIDs = append(projectIDs, projectID)
	}
	slices.Sort(projectIDs)

	var errs []error
	batches := make([]meta.PlatformErrorRateBatch, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		cfg, _, err := s.cachedPlatformConfig(ctx, projectID)
		if err != nil {
			errs = append(errs, fmt.Errorf("load error_rate_spike config for project %d: %w", projectID, err))
			continue
		}
		if cfg.ErrorRateThresholdPerMinute == 0 {
			continue
		}
		batches = append(batches, meta.PlatformErrorRateBatch{
			ProjectID:       projectID,
			ThresholdPerMin: cfg.ErrorRateThresholdPerMinute,
			OccurredAt:      byProject[projectID],
		})
	}
	if len(batches) == 0 {
		return errors.Join(errs...)
	}
	// Results returned next to an error are committed (only the failing projects
	// were rolled back), so their transitions are still drained below.
	results, err := s.store.RecordPlatformErrorRates(ctx, s.now().UTC(), errorRateWindow, batches)
	if err != nil {
		errs = append(errs, fmt.Errorf("record error_rate_spike window: %w", err))
	}
	for _, res := range results {
		if len(res.Transitions) == 0 {
			continue
		}
		if drainErr := s.drainPlatformAlertTransitions(ctx, res.ProjectID); drainErr != nil {
			errs = append(errs, drainErr)
		}
	}
	return errors.Join(errs...)
}

// CheckErrorRateRecoveries first drains every unacknowledged
// platform_alert_transitions row and replays every firing (and recently
// resolved) durable error_rate_spike state into notification_outbox, then
// resolves states whose 5-minute window dropped back within threshold and
// drains the resolved transitions that logged. Draining before resolving keeps
// a firing transition committed by a crashed replica ahead of its own
// resolution, and the deterministic idempotency keys make a repeated drain or
// replay a no-op once enqueued.
func (s *Service) CheckErrorRateRecoveries(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	var errs []error
	if err := s.replayPlatformAlertStates(ctx, 0); err != nil {
		errs = append(errs, err)
	}
	recoveries, err := s.store.CheckPlatformErrorRateRecoveries(ctx, s.now().UTC(), errorRateWindow)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, rec := range recoveries {
		if drainErr := s.drainPlatformAlertTransitions(ctx, rec.ProjectID); drainErr != nil {
			errs = append(errs, drainErr)
		}
	}
	return errors.Join(errs...)
}

// platformTransitionDrainLimit bounds one drain pass; the recovery loop picks
// up any remainder on its next tick.
const platformTransitionDrainLimit = 500

// drainPlatformAlertTransitions plans unacknowledged platform_alert_transitions
// rows (onlyProjectID > 0 filters to one project) into notification_outbox and
// acknowledges each one after its outbox rows are written. Each (project, kind)
// stream is processed from its oldest row in commit order and stops at its
// first failure, so a firing(C) is always enqueued before resolved(C) is
// planned (whose prior-firing check then sees it) and resolved(C) before
// firing(C+1); the failed row and its successors stay for the next pass.
//
// Concurrent drains of one stream (the ingest path and any replica's recovery
// loop) are safe without a claim: each re-plans the stream from its oldest
// unacknowledged row, a row is acknowledged only after its outbox rows commit,
// and the platform-state idempotency keys turn duplicate enqueues into no-ops,
// so every drain observes the predecessors of a row already in
// notification_outbox before it enqueues that row.
//
// Rows recorded longer than platformResolvedReplayHorizon ago are dropped with
// a warning instead of delivered, matching the state replay horizon: a long
// outage cannot flood receivers with stale transitions. A firing state that is
// still active is redelivered by the state replay regardless.
func (s *Service) drainPlatformAlertTransitions(ctx context.Context, onlyProjectID int) error {
	pending, err := s.store.PendingPlatformAlertTransitions(ctx, onlyProjectID, platformTransitionDrainLimit)
	if err != nil {
		return fmt.Errorf("list pending platform alert transitions: %w", err)
	}
	type stream struct {
		projectID int
		kind      string
	}
	blocked := make(map[stream]struct{})
	staleBefore := s.now().UTC().Add(-platformResolvedReplayHorizon)
	var errs []error
	for _, tr := range pending {
		key := stream{projectID: tr.ProjectID, kind: tr.Kind}
		if _, stop := blocked[key]; stop {
			continue
		}
		if tr.RecordedAt.Before(staleBefore) {
			slog.Warn(
				"drop stale platform alert transition",
				"project_id", tr.ProjectID,
				"kind", tr.Kind,
				"status", tr.Status,
				"cycle", tr.Cycle,
				"recorded_at", tr.RecordedAt,
			)
		} else if notifyErr := s.notifyPlatformTransition(ctx, tr); notifyErr != nil {
			blocked[key] = struct{}{}
			errs = append(errs, notifyErr)
			continue
		}
		if ackErr := s.store.AckPlatformAlertTransition(ctx, tr.ID); ackErr != nil {
			blocked[key] = struct{}{}
			errs = append(errs, ackErr)
		}
	}
	return errors.Join(errs...)
}

// notifyPlatformTransition renders one logged transition and plans it through
// notifyErrorRateTransition.
func (s *Service) notifyPlatformTransition(ctx context.Context, tr meta.PlatformAlertTransition) error {
	if tr.Kind != platformErrorRateKind {
		return nil
	}
	transition := meta.PlatformErrorSpikeTransition{
		Transition: tr.Status,
		Cycle:      tr.Cycle,
		StartsAt:   tr.StartsAt,
	}
	if tr.EndsAt != nil {
		transition.EndsAt = *tr.EndsAt
	}
	var summary string
	switch {
	case tr.WindowCount == nil:
		summary = "Error rate spike state reset after alert configuration change"
	case tr.Status == "resolved":
		transition.Count = *tr.WindowCount
		summary = fmt.Sprintf(
			"Error rate spike recovered: %.1f events/min",
			float64(*tr.WindowCount)/errorRateWindow.Minutes(),
		)
	default:
		transition.Count = *tr.WindowCount
		summary = fmt.Sprintf("Error rate spike: %.1f events/min", float64(*tr.WindowCount)/errorRateWindow.Minutes())
	}
	return s.notifyErrorRateTransition(
		ctx,
		tr.ProjectID,
		tr.ProjectSlug,
		transition,
		errorRateAnnotations(summary, tr.ThresholdPerMin, tr.Cycle),
	)
}

// platformResolvedReplayHorizon bounds how long after a resolved transition
// the recovery loop keeps replaying it, covering a replica that committed the
// resolution and crashed before its outbox write.
const platformResolvedReplayHorizon = 2 * errorRateWindow

// replayPlatformAlertStates idempotently re-plans durable error_rate_spike
// states into notification_outbox. Firing states are always replayed, which
// also delivers a spike once a silence or inhibition stops suppressing it (the
// role Engine.Reevaluate used to play); each cycle notifies firing at most once,
// matching the removed Engine, which never repeated a still-firing spike.
// Per-cycle firing deduplication relies on the delivered or dead outbox row
// remaining in notification_outbox until janitor pruning (retention.alert_events,
// default 30d); a spike continuously firing past that retention window is
// re-enqueued once after its row is pruned.
//
// If a firing spike is folded into a storm summary (keyed by
// storm\x00<project>\x00<bucket>), its own platform-state firing key is not
// written; replays emit at most one storm summary per repeat_interval bucket
// while the storm persists, and deliver an individual firing notification once
// the project drops below the storm threshold if the spike is still active.
//
// Resolved states are replayed within platformResolvedReplayHorizon only for
// channels whose same-cycle firing key already exists in notification_outbox,
// preventing orphan recovery notifications after a silence or inhibition lifts.
//
// The transition log is drained first, and states whose stream still has
// unacknowledged transitions (a drain that failed part-way) are skipped by
// RecentPlatformAlertStates, so the state replay never enqueues a later
// cycle ahead of an earlier logged transition.
// onlyProjectID > 0 filters the query to one project in SQL.
func (s *Service) replayPlatformAlertStates(ctx context.Context, onlyProjectID int) error {
	var errs []error
	if err := s.drainPlatformAlertTransitions(ctx, onlyProjectID); err != nil {
		errs = append(errs, err)
	}
	now := s.now().UTC()
	states, err := s.store.RecentPlatformAlertStates(
		ctx,
		platformErrorRateKind,
		now.Add(-platformResolvedReplayHorizon),
		onlyProjectID,
	)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, st := range states {
		transition := meta.PlatformErrorSpikeTransition{
			Transition: st.Status,
			Cycle:      st.ActiveCycle,
			StartsAt:   st.StartsAt,
		}
		summary := "Error rate spike above threshold"
		if st.Status == "resolved" {
			summary = "Error rate spike recovered"
			if st.EndsAt != nil {
				transition.EndsAt = *st.EndsAt
			}
		}
		threshold := 0
		if cfg, _, cfgErr := s.cachedPlatformConfig(ctx, st.ProjectID); cfgErr == nil {
			threshold = cfg.ErrorRateThresholdPerMinute
		}
		if notifyErr := s.notifyErrorRateTransition(
			ctx,
			st.ProjectID,
			st.ProjectSlug,
			transition,
			errorRateAnnotations(summary, threshold, st.ActiveCycle),
		); notifyErr != nil {
			errs = append(errs, notifyErr)
		}
	}
	return errors.Join(errs...)
}

// replayProjectPlatformAlertStates re-plans one project's platform alert states
// after its silence or inhibition policy changes, so a spike held back by that
// policy is delivered as soon as it is lifted rather than on the next recovery
// tick.
func (s *Service) replayProjectPlatformAlertStates(ctx context.Context, projectID int) {
	if s.store == nil || projectID <= 0 {
		return
	}
	if err := s.replayPlatformAlertStates(ctx, projectID); err != nil {
		slog.Warn("replay platform alert states after policy change", "project_id", projectID, "error", err)
	}
}

const platformErrorRateKind = "error_rate_spike"

func errorRateAnnotations(summary string, threshold int, cycle int64) map[string]string {
	return map[string]string{
		"summary":           summary,
		"window":            "5m",
		"threshold_per_min": strconv.Itoa(threshold),
		"cycle":             strconv.FormatInt(cycle, 10),
	}
}

// notifyErrorRateTransition plans one committed error_rate_spike transition
// through the shared policy planner (silence, inhibition, storm) and writes the
// result to notification_outbox. Enqueue failures are logged here with the
// project, status and cycle before being returned; the recovery loop replays
// the transition.
func (s *Service) notifyErrorRateTransition(
	ctx context.Context,
	projectID int,
	projectSlug string,
	transition meta.PlatformErrorSpikeTransition,
	annotations map[string]string,
) error {
	if transition.Transition != "firing" && transition.Transition != "resolved" {
		return nil
	}
	err := s.notifyPlatformState(ctx, projectID, projectSlug, platformErrorRateKind, transition, annotations)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn(
			"enqueue error_rate_spike notification",
			"project_id", projectID,
			"status", transition.Transition,
			"cycle", transition.Cycle,
			"error", err,
		)
	}
	return err
}

func (s *Service) notifyPlatformState(
	ctx context.Context,
	projectID int,
	projectSlug string,
	kind string,
	transition meta.PlatformErrorSpikeTransition,
	annotations map[string]string,
) error {
	if projectSlug == "" {
		_, p, err := s.cachedPlatformConfig(ctx, projectID)
		if err != nil {
			return err
		}
		projectSlug = p.Slug
	}
	now := s.now().UTC()
	labels := platformLabels(projectSlug, kind)
	alertItem := Alert{
		Status:      transition.Transition,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    transition.StartsAt,
		Fingerprint: Fingerprint(labels),
	}
	if transition.Transition == "resolved" {
		alertItem.EndsAt = transition.EndsAt
	}
	groupKey := platformGroupKey(kind)
	compiledPolicy, err := s.platformPolicy(ctx, projectSlug, now, nil, nil, groupKey)
	if err != nil {
		return err
	}
	plan := PlanNotifications(PlanConfig{
		ProjectSlug:    projectSlug,
		ProjectID:      projectID,
		RuleName:       kind,
		GroupKey:       groupKey,
		RepeatInterval: s.repeatInterval,
		StormThreshold: s.stormThreshold,
		Now:            now,
	}, compiledPolicy, []AlertCandidate{{Alert: alertItem, DueOverride: true}})
	ref := PlatformStateRef{ProjectID: projectID, Kind: kind, Cycle: transition.Cycle, Status: transition.Transition}
	var errs []error
	for _, notification := range plan.Notifications {
		if notification.Storm != nil {
			if stormErr := s.notifyStorm(ctx, notification); stormErr != nil {
				errs = append(errs, stormErr)
			}
			continue
		}
		if notifyErr := s.enqueuePlatformNotification(
			ctx,
			notification,
			func(_ int, channelIDs []int, n Notification) ([]meta.NotificationOutboxEnqueue, error) {
				targetChannels := channelIDs
				if ref.Status == "resolved" {
					var filterErr error
					targetChannels, filterErr = s.channelsWithPriorFiring(ctx, ref, channelIDs)
					if filterErr != nil {
						return nil, filterErr
					}
				}
				return BuildPlatformStateOutboxEntries(ref, targetChannels, []Notification{n}), nil
			},
		); notifyErr != nil {
			errs = append(errs, notifyErr)
		}
	}
	return errors.Join(errs...)
}

// channelsWithPriorFiring filters channelIDs to channels whose notification_outbox
// already contains the firing entry for ref's cycle, so resolved notifications
// are not emitted for incidents whose firing was suppressed or never enqueued.
func (s *Service) channelsWithPriorFiring(
	ctx context.Context,
	ref PlatformStateRef,
	channelIDs []int,
) ([]int, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	firingRef := PlatformStateRef{
		ProjectID: ref.ProjectID,
		Kind:      ref.Kind,
		Cycle:     ref.Cycle,
		Status:    "firing",
	}
	keys := make([]string, len(channelIDs))
	for i, chID := range channelIDs {
		keys[i] = PlatformStateOutboxKey(firingRef, chID)
	}
	existing, err := s.store.NotificationOutboxExistingKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	matched := make([]int, 0, len(channelIDs))
	for i, chID := range channelIDs {
		if _, ok := existing[keys[i]]; ok {
			matched = append(matched, chID)
		}
	}
	return matched, nil
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
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("panic recovered in platform error rate monitor", "panic", r)
					}
				}()
				if err := s.CheckErrorRateRecoveries(ctx); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("check platform error rate recoveries", "error", err)
				}
			}()
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

func (s *Service) notifyPlatformLifecycle(ctx context.Context, n Notification, lifecycleID int64) error {
	return s.enqueuePlatformNotification(
		ctx,
		n,
		func(projectID int, channelIDs []int, n Notification) ([]meta.NotificationOutboxEnqueue, error) {
			return BuildOutboxEntriesWithLifecycle(
				projectID,
				nil,
				lifecycleID,
				channelIDs,
				[]Notification{n},
				s.now().UTC(),
				s.repeatInterval,
			), nil
		},
	)
}

// enqueuePlatformNotification re-reads the project's platform alert config,
// drops kinds that are disabled, attaches optional Issue AI analysis, and
// writes the outbox rows produced by buildEntries for every enabled channel.
func (s *Service) enqueuePlatformNotification(
	ctx context.Context,
	n Notification,
	buildEntries func(projectID int, channelIDs []int, n Notification) ([]meta.NotificationOutboxEnqueue, error),
) error {
	cfg, _, err := s.store.ProjectAlertPolicyBySlug(ctx, n.Project)
	if err != nil {
		return err
	}
	kind := n.Rule
	if (kind == "new_issue" && !cfg.NewIssue) || (kind == "regression" && !cfg.Regression) ||
		(kind == platformErrorRateKind && cfg.ErrorRateThresholdPerMinute == 0) {
		return nil
	}
	n = s.attachIssueAIAnalysis(ctx, n)
	channelIDs := make([]int, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		if ch.Enabled {
			channelIDs = append(channelIDs, ch.ID)
		}
	}
	entries, err := buildEntries(cfg.ProjectID, channelIDs, n)
	if err != nil {
		return err
	}
	return s.enqueueOutboxEntries(ctx, entries)
}

// attachIssueAIAnalysis is intentionally read-only and best-effort. Automatic
// Issue analysis runs beside the ingest path; when it has already finished for
// a new_issue notification, only its one-line summary is attached as the
// `ai_summary` annotation. Probable causes and suggested actions stay in
// issues.ai_analysis for the Web console and are never copied into
// notification payloads. Regression notifications carry no AI annotation
// because the stored analysis predates the regression and may be stale.
// Missing or failed AI must never delay or suppress the core alert.
func (s *Service) attachIssueAIAnalysis(ctx context.Context, n Notification) Notification {
	if n.Rule != "new_issue" {
		return n
	}
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
		rawSummary, _ := row.AIAnalysis["summary"].(string)
		summary := singleLine(rawSummary, maxNotificationAISummaryRunes)
		if summary == "" {
			continue
		}
		annotations := make(map[string]string, len(n.Alerts[i].Annotations)+1)
		for key, value := range n.Alerts[i].Annotations {
			annotations[key] = value
		}
		annotations["ai_summary"] = summary
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
	return matcher.SpecsToMaps(ms)
}
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
