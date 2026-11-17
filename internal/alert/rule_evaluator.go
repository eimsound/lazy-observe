package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert/ownership"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
	prommodel "github.com/prometheus/common/model"
)

const (
	defaultEvaluatorTick              = 5 * time.Second
	defaultEvaluatorMetricsInterval   = 5 * time.Second
	defaultEvaluatorLogsInterval      = time.Minute
	defaultEvaluatorMemberTTL         = 30 * time.Second
	defaultEvaluatorHeartbeatInterval = 10 * time.Second
	defaultEvaluatorLeaseTTL          = 20 * time.Second
	defaultEvaluatorQueryTimeout      = 10 * time.Second
)

type SignalBackend interface {
	MetricsMode() storage.MetricsKind
	MetricsQueryBase() string
	LogsBase() string
	SetTenantHeaders(http.Header, int)
}

type RuleSample struct {
	Fingerprint string
	Labels      map[string]string
	Annotations map[string]string
	Value       float64
}

type RuleQuery interface {
	Evaluate(context.Context, *meta.AlertRuleView, *meta.ProjectInfo, time.Time) ([]RuleSample, error)
}

type RuleEvaluatorConfig struct {
	Store             *meta.Store
	Backend           SignalBackend
	Query             RuleQuery
	Client            *http.Client
	MemberID          string
	TickInterval      time.Duration
	MetricsInterval   time.Duration
	LogsInterval      time.Duration
	MemberTTL         time.Duration
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	QueryTimeout      time.Duration
	GroupWait         time.Duration
	RepeatInterval    time.Duration
	WakeOutbox        func()
	Now               func() time.Time
}

// RuleEvaluator is the native rule evaluator retained under its original
// type name for source compatibility. Every rule has notification authority
// only while this instance owns its database-backed lease and fencing epoch.
type RuleEvaluator struct {
	store             *meta.Store
	query             RuleQuery
	memberID          string
	tickInterval      time.Duration
	metricsInterval   time.Duration
	logsInterval      time.Duration
	memberTTL         time.Duration
	heartbeatInterval time.Duration
	leaseTTL          time.Duration
	queryTimeout      time.Duration
	groupWait         time.Duration
	repeatInterval    time.Duration
	wakeOutbox        func()
	now               func() time.Time

	mu     sync.Mutex
	leases map[int]meta.AlertRuleLease
	next   map[int]time.Time
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewRuleEvaluator(cfg RuleEvaluatorConfig) *RuleEvaluator {
	if cfg.Store == nil || cfg.Query == nil && cfg.Backend == nil {
		return nil
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: defaultEvaluatorQueryTimeout}
	}
	if cfg.Query == nil {
		cfg.Query = &HTTPRuleQuery{Backend: cfg.Backend, Client: cfg.Client}
	}
	if cfg.MemberID == "" {
		cfg.MemberID = "evaluator-" + strings.TrimPrefix(randomOutboxWorkerID(), "outbox-")
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultEvaluatorTick
	}
	if cfg.MetricsInterval <= 0 {
		cfg.MetricsInterval = defaultEvaluatorMetricsInterval
	}
	if cfg.LogsInterval <= 0 {
		cfg.LogsInterval = defaultEvaluatorLogsInterval
	}
	if cfg.MemberTTL <= 0 {
		cfg.MemberTTL = defaultEvaluatorMemberTTL
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultEvaluatorHeartbeatInterval
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultEvaluatorLeaseTTL
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = defaultEvaluatorQueryTimeout
	}
	if cfg.GroupWait <= 0 {
		cfg.GroupWait = 30 * time.Second
	}
	if cfg.RepeatInterval <= 0 {
		cfg.RepeatInterval = 4 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RuleEvaluator{
		store: cfg.Store, query: cfg.Query, memberID: cfg.MemberID,
		tickInterval: cfg.TickInterval, metricsInterval: cfg.MetricsInterval,
		logsInterval: cfg.LogsInterval, memberTTL: cfg.MemberTTL,
		heartbeatInterval: cfg.HeartbeatInterval, leaseTTL: cfg.LeaseTTL,
		queryTimeout: cfg.QueryTimeout, now: cfg.Now,
		groupWait: cfg.GroupWait, repeatInterval: cfg.RepeatInterval, wakeOutbox: cfg.WakeOutbox,
		leases: make(map[int]meta.AlertRuleLease), next: make(map[int]time.Time),
	}
}

func (e *RuleEvaluator) Start() {
	if e == nil || e.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.wg.Add(2)
	go e.runHeartbeat(ctx)
	go e.runEvaluations(ctx)
}

func (e *RuleEvaluator) Close() {
	if e == nil || e.cancel == nil {
		return
	}
	e.cancel()
	e.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.releaseAll(ctx)
	if _, err := e.store.DeleteAlertEngineMember(ctx, e.memberID); err != nil {
		slog.Warn("delete alert evaluator member", "member_id", e.memberID, "error", err)
	}
}

func (e *RuleEvaluator) runHeartbeat(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.heartbeatInterval)
	defer ticker.Stop()
	for {
		if _, err := e.store.HeartbeatAlertEngine(
			ctx,
			e.memberID,
			e.memberTTL,
		); err != nil &&
			!errors.Is(err, context.Canceled) {
			slog.Error("heartbeat alert evaluator", "member_id", e.memberID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *RuleEvaluator) runEvaluations(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.tickInterval)
	defer ticker.Stop()
	for {
		e.evaluateCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *RuleEvaluator) evaluateCycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if _, err := e.store.HeartbeatAlertEngine(ctx, e.memberID, e.memberTTL); err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("prepare alert evaluation", "member_id", e.memberID, "error", err)
		}
		return
	}
	members, err := e.store.ActiveAlertEngineMembers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("list alert evaluator members", "member_id", e.memberID, "error", err)
		}
		return
	}
	memberIDs := make([]ownership.MemberID, 0, len(members))
	for _, member := range members {
		memberIDs = append(memberIDs, ownership.MemberID(member.MemberID))
	}
	rules, err := e.store.AlertEvaluationRules(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("list alert rules", "member_id", e.memberID, "error", err)
		}
		return
	}
	owned := make(map[int]struct{})
	for i := range rules {
		rule := &rules[i]
		if rule.EvaluationEngine == "paused" {
			continue
		}
		project := &rule.Project
		owner, ownerErr := ownership.Owner(
			ownership.RuleKey{ProjectID: project.ID, Signal: rule.Signal, RuleID: rule.ID},
			memberIDs,
		)
		if ownerErr != nil || string(owner) != e.memberID {
			continue
		}
		owned[rule.ID] = struct{}{}
		lease, leaseErr := e.store.AcquireAlertRuleLease(ctx, rule.ID, e.memberID, e.leaseTTL)
		if errors.Is(leaseErr, meta.ErrRuleLeaseHeld) || errors.Is(leaseErr, meta.ErrInactiveAlertEngineMember) {
			continue
		}
		if leaseErr != nil {
			slog.Error("acquire alert rule lease", "rule_id", rule.ID, "member_id", e.memberID, "error", leaseErr)
			continue
		}
		e.mu.Lock()
		e.leases[rule.ID] = lease
		due := e.next[rule.ID]
		now := e.now().UTC()
		if due.IsZero() || !now.Before(due) {
			e.next[rule.ID] = now.Add(e.ruleInterval(rule))
		} else {
			now = time.Time{}
		}
		e.mu.Unlock()
		if !now.IsZero() {
			e.evaluateRule(ctx, rule, project, lease, now)
		}
	}
	e.releaseUnowned(ctx, owned)
}

func (e *RuleEvaluator) evaluateRule(
	ctx context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	lease meta.AlertRuleLease,
	now time.Time,
) {
	queryCtx, cancel := context.WithTimeout(ctx, e.queryTimeout)
	samples, err := e.query.Evaluate(queryCtx, rule, project, now)
	cancel()
	if err != nil {
		slog.Error("query native alert rule", "rule_id", rule.ID, "error", err)
		return
	}
	previous, err := e.store.AlertRuleStates(ctx, rule.ID)
	if err != nil {
		slog.Error("read alert rule state", "rule_id", rule.ID, "error", err)
		return
	}
	states, err := advanceRuleStates(rule, previous, samples, now)
	if err != nil {
		slog.Error("advance native alert state", "rule_id", rule.ID, "error", err)
		return
	}
	states, notifications, inhibited, planErr := e.planNativeNotifications(ctx, rule, project, previous, states, now)
	if planErr != nil {
		slog.Error("plan native alert notifications", "rule_id", rule.ID, "error", planErr)
		return
	}
	if err := e.store.CommitNativeAlertEvaluation(ctx, meta.NativeAlertEvaluation{
		RuleID: rule.ID, MemberID: e.memberID, LeaseEpoch: lease.Epoch,
		States: states, InhibitedFingerprints: inhibited, Notifications: notifications,
	}); err != nil {
		if errors.Is(err, meta.ErrStaleRuleFence) {
			slog.Warn("discard native alert evaluation after losing rule lease", "rule_id", rule.ID,
				"member_id", e.memberID, "lease_epoch", lease.Epoch)
		} else {
			slog.Error("commit native alert evaluation", "rule_id", rule.ID, "error", err)
		}
		return
	}
	if len(notifications) != 0 && e.wakeOutbox != nil {
		e.wakeOutbox()
	}
}

func (e *RuleEvaluator) ruleInterval(rule *meta.AlertRuleView) time.Duration {
	if rule.Signal == "logs" {
		return e.logsInterval
	}
	return e.metricsInterval
}

func (e *RuleEvaluator) releaseUnowned(ctx context.Context, owned map[int]struct{}) {
	e.mu.Lock()
	var release []meta.AlertRuleLease
	for ruleID, lease := range e.leases {
		if _, ok := owned[ruleID]; !ok {
			release = append(release, lease)
			delete(e.leases, ruleID)
			delete(e.next, ruleID)
		}
	}
	e.mu.Unlock()
	for _, lease := range release {
		_, _ = e.store.ReleaseAlertRuleLease(ctx, lease.RuleID, e.memberID, lease.Epoch)
	}
}

func (e *RuleEvaluator) releaseAll(ctx context.Context) {
	e.mu.Lock()
	leases := make([]meta.AlertRuleLease, 0, len(e.leases))
	for _, lease := range e.leases {
		leases = append(leases, lease)
	}
	e.leases = make(map[int]meta.AlertRuleLease)
	e.mu.Unlock()
	for _, lease := range leases {
		_, _ = e.store.ReleaseAlertRuleLease(ctx, lease.RuleID, e.memberID, lease.Epoch)
	}
}

func advanceRuleStates(
	rule *meta.AlertRuleView,
	previous []meta.AlertRuleState,
	samples []RuleSample,
	now time.Time,
) ([]meta.AlertRuleState, error) {
	forDuration, err := prommodel.ParseDuration(rule.For)
	if err != nil {
		return nil, fmt.Errorf("parse alert for duration %q: %w", rule.For, err)
	}
	wait := time.Duration(forDuration)
	prior := make(map[string]meta.AlertRuleState, len(previous))
	for _, state := range previous {
		prior[state.Fingerprint] = state
	}
	states := make([]meta.AlertRuleState, 0, len(previous)+len(samples))
	seen := make(map[string]struct{}, len(samples))
	for _, sample := range samples {
		if sample.Fingerprint == "" {
			return nil, errors.New("alert sample fingerprint is empty")
		}
		if _, duplicate := seen[sample.Fingerprint]; duplicate {
			return nil, fmt.Errorf("duplicate alert sample fingerprint %q", sample.Fingerprint)
		}
		seen[sample.Fingerprint] = struct{}{}
		state := meta.AlertRuleState{
			RuleID: rule.ID, Fingerprint: sample.Fingerprint, Status: "pending",
			LastEvaluatedAt: now, Labels: cloneStrings(sample.Labels), Annotations: cloneStrings(sample.Annotations),
		}
		value := sample.Value
		state.Value = &value
		old, exists := prior[sample.Fingerprint]
		if exists && old.Status != "recovered" {
			state.Status, state.PendingSince, state.FiringSince, state.LastNotifiedAt = old.Status, old.PendingSince, old.FiringSince, old.LastNotifiedAt
		} else {
			pending := now
			state.PendingSince = &pending
		}
		if wait <= 0 ||
			state.Status == "pending" && state.PendingSince != nil && !now.Before(state.PendingSince.Add(wait)) {
			state.Status = "firing"
			if state.FiringSince == nil {
				firing := now
				state.FiringSince = &firing
			}
		}
		states = append(states, state)
	}
	for _, old := range previous {
		if _, active := seen[old.Fingerprint]; active {
			continue
		}
		old.Status = "recovered"
		old.LastEvaluatedAt = now
		old.Value = nil
		states = append(states, old)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Fingerprint < states[j].Fingerprint })
	return states, nil
}

func (e *RuleEvaluator) planNativeNotifications(
	ctx context.Context,
	rule *meta.AlertRuleView,
	p *meta.ProjectInfo,
	previous, states []meta.AlertRuleState,
	now time.Time,
) ([]meta.AlertRuleState, []meta.NotificationOutboxEnqueue, map[string]bool, error) {
	prior := make(map[string]meta.AlertRuleState, len(previous))
	for _, state := range previous {
		prior[state.Fingerprint] = state
	}
	firingDue := false
	var resolvedIndexes []int
	for i := range states {
		state := &states[i]
		old, existed := prior[state.Fingerprint]
		switch state.Status {
		case "firing":
			if state.FiringSince == nil || now.Before(state.FiringSince.Add(e.groupWait)) {
				continue
			}
			if !existed || old.Status != "firing" || state.LastNotifiedAt == nil ||
				!now.Before(state.LastNotifiedAt.Add(e.repeatInterval)) {
				firingDue = true
			}
		case "recovered":
			if existed && old.Status == "firing" {
				resolvedIndexes = append(resolvedIndexes, i)
			}
		}
	}
	if !firingDue && len(resolvedIndexes) == 0 {
		return states, nil, nil, nil
	}
	policy, err := e.store.AlertPolicySnapshot(ctx, p.ID, now)
	if err != nil {
		return nil, nil, nil, err
	}
	silences := make([][]Matcher, 0, len(policy.Silences))
	for _, matchers := range policy.Silences {
		silences = append(silences, mapsToMatchers(matchers))
	}
	inhibitions := make([]InhibitionRule, 0, len(policy.InhibitionRules))
	for _, rule := range policy.InhibitionRules {
		inhibitions = append(inhibitions, InhibitionRule{
			SourceMatchers: mapsToMatchers(rule.SourceMatchers),
			TargetMatchers: mapsToMatchers(rule.TargetMatchers),
			EqualLabels:    rule.EqualLabels,
		})
	}
	stateSources, err := e.store.ProjectFiringAlertRuleStates(ctx, p.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	sources := make([]Alert, 0, len(stateSources)+len(policy.FiringEvents))
	for _, state := range stateSources {
		sources = append(sources, alertFromRuleState(state, "firing", time.Time{}))
	}
	for _, event := range policy.FiringEvents {
		sources = append(sources, Alert{
			Status:      "firing",
			Labels:      event.Labels,
			Annotations: event.Annotations,
			StartsAt:    event.StartsAt,
			Fingerprint: event.Fingerprint,
		})
	}
	inhibitedFingerprints := make(map[string]bool)
	var notifications []Notification
	if firingDue {
		allFiring := make([]Alert, 0, len(states))
		kept := make([]Alert, 0, len(states))
		for _, state := range states {
			if state.Status != "firing" {
				continue
			}
			alert := alertFromRuleState(state, "firing", time.Time{})
			allFiring = append(allFiring, alert)
			inhibited := IsInhibited(alert, sources, inhibitions)
			inhibitedFingerprints[alert.Fingerprint] = inhibited
			if !matchesAny(alert.Labels, silences) && !inhibited {
				kept = append(kept, alert)
			}
		}
		if len(kept) != 0 {
			notifications = append(
				notifications,
				Notification{Project: p.Slug, Rule: rule.Name, Status: "firing", Alerts: kept},
			)
			for i := range states {
				if states[i].Status == "firing" {
					marked := now
					states[i].LastNotifiedAt = &marked
				}
			}
		} else if len(allFiring) != 0 {
			allSilenced := true
			for _, alert := range allFiring {
				if !matchesAny(alert.Labels, silences) {
					allSilenced = false
					break
				}
			}
			if allSilenced {
				for i := range states {
					if states[i].Status == "firing" {
						marked := now
						states[i].LastNotifiedAt = &marked
					}
				}
			}
		}
	}
	if len(resolvedIndexes) != 0 {
		resolved := make([]Alert, 0, len(resolvedIndexes))
		for _, index := range resolvedIndexes {
			alert := alertFromRuleState(states[index], "resolved", now)
			if !matchesAny(alert.Labels, silences) {
				resolved = append(resolved, alert)
			}
			marked := now
			states[index].LastNotifiedAt = &marked
		}
		if len(resolved) != 0 {
			notifications = append(
				notifications,
				Notification{Project: p.Slug, Rule: rule.Name, Status: "resolved", Alerts: resolved},
			)
		}
	}
	return states, nativeOutboxEntries(rule, p, notifications, now, e.repeatInterval), inhibitedFingerprints, nil
}

func alertFromRuleState(state meta.AlertRuleState, status string, endsAt time.Time) Alert {
	startsAt := state.LastEvaluatedAt
	if state.FiringSince != nil {
		startsAt = *state.FiringSince
	}
	return Alert{
		Status:      status,
		Labels:      cloneStrings(state.Labels),
		Annotations: cloneStrings(state.Annotations),
		StartsAt:    startsAt,
		EndsAt:      endsAt,
		Fingerprint: state.Fingerprint,
	}
}

func nativeOutboxEntries(
	rule *meta.AlertRuleView,
	p *meta.ProjectInfo,
	notifications []Notification,
	now time.Time,
	repeat time.Duration,
) []meta.NotificationOutboxEnqueue {
	var out []meta.NotificationOutboxEnqueue
	for _, notification := range notifications {
		payload, err := json.Marshal(notification)
		if err != nil {
			continue
		}
		generation := now.UnixNano()
		if notification.Status == "firing" && repeat > 0 && len(notification.Alerts) != 0 {
			generation = now.Sub(notification.Alerts[0].StartsAt).Nanoseconds() / repeat.Nanoseconds()
		}
		keyPayload := append(slices.Clone(payload), []byte(fmt.Sprintf("\x00%d\x00%d", rule.ID, generation))...)
		for _, channelID := range rule.EnabledChannelIDs {
			ruleID := rule.ID
			priority := 0
			if notification.Status == "resolved" {
				priority = 100
			}
			out = append(out, meta.NotificationOutboxEnqueue{
				IdempotencyKey: notificationOutboxKey(channelID, keyPayload), ProjectID: p.ID,
				RuleID: &ruleID, ChannelID: channelID, Payload: payload, Priority: priority,
			})
		}
	}
	return out
}

type HTTPRuleQuery struct {
	Backend SignalBackend
	Client  *http.Client
}

func (q *HTTPRuleQuery) Evaluate(
	ctx context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	at time.Time,
) ([]RuleSample, error) {
	if q.Backend == nil {
		return nil, errors.New("alert query backend is required")
	}
	client := q.Client
	if client == nil {
		client = &http.Client{Timeout: defaultEvaluatorQueryTimeout}
	}
	values := url.Values{
		"query": {rule.Expression},
		"time":  {strconv.FormatFloat(float64(at.UnixNano())/float64(time.Second), 'f', 3, 64)},
	}
	endpoint := ""
	if rule.Signal == "logs" {
		endpoint = strings.TrimRight(q.Backend.LogsBase(), "/") + "/select/logsql/stats_query"
	} else {
		endpoint = strings.TrimRight(q.Backend.MetricsQueryBase(), "/") + "/api/v1/query"
		if q.Backend.MetricsMode() == storage.MetricsPrometheus {
			expr, err := storage.InjectPromQL(rule.Expression, project.Slug)
			if err != nil {
				return nil, fmt.Errorf("inject project into PromQL: %w", err)
			}
			values.Set("query", expr)
		} else {
			values.Set("extra_filters[]", fmt.Sprintf(`{project_id=%q}`, project.Slug))
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if rule.Signal == "logs" {
		q.Backend.SetTenantHeaders(req.Header, project.ID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("alert query status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result prometheusInstantResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode alert query response: %w", err)
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("alert query %s: %s", result.ErrorType, result.Error)
	}
	if result.Data.ResultType != "vector" {
		return nil, fmt.Errorf("alert query returned %q instead of vector", result.Data.ResultType)
	}
	samples := make([]RuleSample, 0, len(result.Data.Result))
	seen := make(map[string]struct{}, len(result.Data.Result))
	for _, item := range result.Data.Result {
		if len(item.Value) != 2 {
			return nil, errors.New("alert vector sample must contain timestamp and value")
		}
		var rawValue string
		if err := json.Unmarshal(item.Value[1], &rawValue); err != nil {
			var number json.Number
			if numberErr := json.Unmarshal(item.Value[1], &number); numberErr != nil {
				return nil, fmt.Errorf("decode alert sample value: %w", err)
			}
			rawValue = number.String()
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return nil, fmt.Errorf("parse alert sample value %q: %w", rawValue, err)
		}
		labels := cloneStrings(item.Metric)
		delete(labels, "__name__")
		labels["project_id"] = project.Slug
		labels["alertname"] = rule.Name
		labels["severity"] = rule.Severity
		for key, label := range rule.Labels {
			labels[key] = label
		}
		fingerprint := Fingerprint(labels)
		if _, duplicate := seen[fingerprint]; duplicate {
			return nil, fmt.Errorf("alert query produced duplicate fingerprint %q", fingerprint)
		}
		seen[fingerprint] = struct{}{}
		samples = append(
			samples,
			RuleSample{
				Fingerprint: fingerprint,
				Labels:      labels,
				Annotations: cloneStrings(rule.Annotations),
				Value:       value,
			},
		)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Fingerprint < samples[j].Fingerprint })
	return samples, nil
}

type prometheusInstantResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}
