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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert/ownership"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
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
	defaultEvaluatorStormThreshold    = 10
	defaultEvaluatorMaxConcurrency    = 4
	minLeaseRenewalInterval           = 50 * time.Millisecond
	maxEvaluationDueSlack             = 250 * time.Millisecond
	maxWarnedInvalidIntervals         = 1024
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

// RuleEvaluationStatus records the latest runtime evaluation outcome for a
// single native alert rule on this evaluator instance.
type RuleEvaluationStatus struct {
	RuleID          int           `json:"rule_id"`
	LastEvaluatedAt time.Time     `json:"last_evaluated_at,omitempty"`
	LastDuration    time.Duration `json:"last_duration"`
	LastError       string        `json:"last_error,omitempty"`
	LastErrorAt     time.Time     `json:"last_error_at,omitempty"`
	EvalCount       uint64        `json:"eval_count"`
	ErrorCount      uint64        `json:"error_count"`
}

// EvaluatorSnapshot exposes point-in-time observability metrics for a
// RuleEvaluator instance, including evaluation latencies, lease loss counts,
// and per-rule last_error state.
type EvaluatorSnapshot struct {
	MemberID                  string                       `json:"member_id"`
	OwnedLeases               int                          `json:"owned_leases"`
	EvaluationsTotal          uint64                       `json:"evaluations_total"`
	EvaluationErrorsTotal     uint64                       `json:"evaluation_errors_total"`
	LeaseLossesTotal          uint64                       `json:"lease_losses_total"`
	LeaseRenewalsTotal        uint64                       `json:"lease_renewals_total"`
	LeaseRenewalFailuresTotal uint64                       `json:"lease_renewal_failures_total"`
	LastCycleDuration         time.Duration                `json:"last_cycle_duration"`
	LastEvalDuration          time.Duration                `json:"last_eval_duration"`
	MaxEvalDuration           time.Duration                `json:"max_eval_duration"`
	TotalEvalDuration         time.Duration                `json:"total_eval_duration"`
	RuleStatuses              map[int]RuleEvaluationStatus `json:"rule_statuses"`
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
	StormThreshold    int
	MaxConcurrency    int
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
	stormThreshold    int
	maxConcurrency    int
	wakeOutbox        func()
	now               func() time.Time

	mu              sync.Mutex
	leases          map[int]meta.AlertRuleLease
	lastDue         map[int]time.Time
	wakeAt          map[int]time.Time
	warnedIntervals map[int]string
	ruleStatuses    map[int]RuleEvaluationStatus
	cancel          context.CancelFunc
	wg              sync.WaitGroup

	evaluationsTotal          atomic.Uint64
	evaluationErrorsTotal     atomic.Uint64
	leaseLossesTotal          atomic.Uint64
	leaseRenewalsTotal        atomic.Uint64
	leaseRenewalFailuresTotal atomic.Uint64
	lastCycleDuration         atomic.Int64
	lastEvalDuration          atomic.Int64
	maxEvalDuration           atomic.Int64
	totalEvalDuration         atomic.Int64
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
	if cfg.StormThreshold <= 0 {
		cfg.StormThreshold = defaultEvaluatorStormThreshold
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultEvaluatorMaxConcurrency
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
		groupWait: cfg.GroupWait, repeatInterval: cfg.RepeatInterval,
		stormThreshold: cfg.StormThreshold, maxConcurrency: cfg.MaxConcurrency,
		wakeOutbox:      cfg.WakeOutbox,
		leases:          make(map[int]meta.AlertRuleLease),
		lastDue:         make(map[int]time.Time),
		wakeAt:          make(map[int]time.Time),
		warnedIntervals: make(map[int]string),
		ruleStatuses:    make(map[int]RuleEvaluationStatus),
	}
}

// Snapshot returns a thread-safe copy of evaluator observability counters,
// durations, and per-rule runtime status.
func (e *RuleEvaluator) Snapshot() EvaluatorSnapshot {
	if e == nil {
		return EvaluatorSnapshot{}
	}
	e.mu.Lock()
	ownedCount := len(e.leases)
	statuses := make(map[int]RuleEvaluationStatus, len(e.ruleStatuses))
	for id, st := range e.ruleStatuses {
		statuses[id] = st
	}
	e.mu.Unlock()
	return EvaluatorSnapshot{
		MemberID:                  e.memberID,
		OwnedLeases:               ownedCount,
		EvaluationsTotal:          e.evaluationsTotal.Load(),
		EvaluationErrorsTotal:     e.evaluationErrorsTotal.Load(),
		LeaseLossesTotal:          e.leaseLossesTotal.Load(),
		LeaseRenewalsTotal:        e.leaseRenewalsTotal.Load(),
		LeaseRenewalFailuresTotal: e.leaseRenewalFailuresTotal.Load(),
		LastCycleDuration:         time.Duration(e.lastCycleDuration.Load()),
		LastEvalDuration:          time.Duration(e.lastEvalDuration.Load()),
		MaxEvalDuration:           time.Duration(e.maxEvalDuration.Load()),
		TotalEvalDuration:         time.Duration(e.totalEvalDuration.Load()),
		RuleStatuses:              statuses,
	}
}

// RuleStatus returns the latest runtime evaluation status for ruleID.
func (e *RuleEvaluator) RuleStatus(ruleID int) (RuleEvaluationStatus, bool) {
	if e == nil {
		return RuleEvaluationStatus{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.ruleStatuses[ruleID]
	return st, ok
}

// RuleLastError returns the sanitized latest evaluation error for ruleID, or ""
// when the last evaluation succeeded or the rule has not been evaluated here.
func (e *RuleEvaluator) RuleLastError(ruleID int) string {
	st, ok := e.RuleStatus(ruleID)
	if !ok {
		return ""
	}
	return st.LastError
}

// ClearRuleStatus removes cached runtime status for ruleID when a rule is
// updated or deleted.
func (e *RuleEvaluator) ClearRuleStatus(ruleID int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	delete(e.ruleStatuses, ruleID)
	delete(e.warnedIntervals, ruleID)
	e.mu.Unlock()
}

func (e *RuleEvaluator) recordRuleOutcome(ruleID int, evaluatedAt time.Time, elapsed time.Duration, evalErr error) {
	e.evaluationsTotal.Add(1)
	ns := elapsed.Nanoseconds()
	e.lastEvalDuration.Store(ns)
	e.totalEvalDuration.Add(ns)
	for {
		prev := e.maxEvalDuration.Load()
		if ns <= prev || e.maxEvalDuration.CompareAndSwap(prev, ns) {
			break
		}
	}

	e.mu.Lock()
	st := e.ruleStatuses[ruleID]
	st.RuleID = ruleID
	st.LastEvaluatedAt = evaluatedAt
	st.LastDuration = elapsed
	st.EvalCount++
	if evalErr != nil {
		e.evaluationErrorsTotal.Add(1)
		st.ErrorCount++
		st.LastError = outbound.SanitizeError(evalErr)
		st.LastErrorAt = evaluatedAt
	} else {
		st.LastError = ""
		st.LastErrorAt = time.Time{}
	}
	e.ruleStatuses[ruleID] = st
	e.mu.Unlock()
}

func (e *RuleEvaluator) Start() {
	if e == nil || e.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.wg.Add(3)
	go e.runHeartbeat(ctx)
	go e.runLeaseRenewal(ctx)
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
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic recovered in alert evaluator heartbeat", "member_id", e.memberID, "panic", r)
				}
			}()
			if _, err := e.store.HeartbeatAlertEngine(
				ctx,
				e.memberID,
				e.memberTTL,
			); err != nil &&
				!errors.Is(err, context.Canceled) {
				slog.Error("heartbeat alert evaluator", "member_id", e.memberID, "error", err)
			}
			if _, err := e.store.DeleteExpiredAlertEngineMembers(ctx); err != nil &&
				!errors.Is(err, context.Canceled) {
				slog.Warn("delete expired alert engine members", "member_id", e.memberID, "error", err)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *RuleEvaluator) runLeaseRenewal(ctx context.Context) {
	defer e.wg.Done()
	interval := e.leaseTTL / 3
	if interval < minLeaseRenewalInterval {
		interval = minLeaseRenewalInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("panic recovered in alert lease renewal", "member_id", e.memberID, "panic", r)
					}
				}()
				e.renewLeases(ctx)
			}()
		}
	}
}

func (e *RuleEvaluator) renewLeases(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	snapshot := make([]meta.AlertRuleLease, 0, len(e.leases))
	for _, lease := range e.leases {
		snapshot = append(snapshot, lease)
	}
	e.mu.Unlock()
	if len(snapshot) == 0 {
		return
	}
	renewed, stale, err := e.store.RenewAlertRuleLeases(ctx, e.memberID, snapshot, e.leaseTTL)
	if err != nil {
		e.leaseRenewalFailuresTotal.Add(1)
		if !errors.Is(err, context.Canceled) {
			slog.Error("renew alert rule leases", "member_id", e.memberID, "count", len(snapshot), "error", err)
		}
		return
	}
	if len(renewed) > 0 {
		e.leaseRenewalsTotal.Add(uint64(len(renewed)))
	}
	if len(stale) > 0 {
		e.leaseLossesTotal.Add(uint64(len(stale)))
	}
	if len(renewed) == 0 && len(stale) == 0 {
		return
	}
	e.mu.Lock()
	for _, updated := range renewed {
		if current, ok := e.leases[updated.RuleID]; ok && current.Epoch == updated.Epoch {
			e.leases[updated.RuleID] = updated
		}
	}
	for _, lost := range stale {
		if current, ok := e.leases[lost.RuleID]; ok && current.Epoch == lost.Epoch {
			delete(e.leases, lost.RuleID)
			delete(e.lastDue, lost.RuleID)
			delete(e.wakeAt, lost.RuleID)
		}
	}
	e.mu.Unlock()
	for _, lost := range stale {
		slog.Warn("discard alert rule lease after renewal fence rejection", "rule_id", lost.RuleID,
			"member_id", e.memberID, "lease_epoch", lost.Epoch)
	}
}

func (e *RuleEvaluator) runEvaluations(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.tickInterval)
	defer ticker.Stop()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic recovered in alert evaluation cycle", "member_id", e.memberID, "panic", r)
				}
			}()
			e.evaluateCycle(ctx)
		}()
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
	cycleNow := e.now().UTC()
	cycleStart := time.Now()
	defer func() {
		e.lastCycleDuration.Store(time.Since(cycleStart).Nanoseconds())
	}()
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
	e.evaluateRules(ctx, rules, memberIDs, cycleNow)
}

func (e *RuleEvaluator) evaluateRules(
	ctx context.Context,
	rules []meta.AlertRuleView,
	memberIDs []ownership.MemberID,
	cycleNow time.Time,
) {
	if cycleNow.IsZero() {
		cycleNow = e.now().UTC()
	} else {
		cycleNow = cycleNow.UTC()
	}
	type evaluationTask struct {
		rule    *meta.AlertRuleView
		project *meta.ProjectInfo
		lease   meta.AlertRuleLease
		now     time.Time
	}
	owned := make(map[int]struct{})
	var dueTasks []evaluationTask
	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled || rule.EvaluationEngine != "native" {
			continue
		}
		owner, ownerErr := ownership.Owner(
			ownership.RuleKey{ProjectID: rule.Project.ID, Signal: rule.Signal, RuleID: rule.ID},
			memberIDs,
		)
		if ownerErr != nil || string(owner) != e.memberID {
			continue
		}
		lease, leaseErr := e.store.AcquireAlertRuleLease(ctx, rule.ID, e.memberID, e.leaseTTL)
		if errors.Is(leaseErr, meta.ErrRuleLeaseHeld) || errors.Is(leaseErr, meta.ErrInactiveAlertEngineMember) {
			e.leaseLossesTotal.Add(1)
			e.mu.Lock()
			knownEpoch := e.leases[rule.ID].Epoch
			delete(e.leases, rule.ID)
			delete(e.lastDue, rule.ID)
			delete(e.wakeAt, rule.ID)
			e.mu.Unlock()
			slog.Warn("skip alert rule evaluation after lease rejection", "rule_id", rule.ID,
				"member_id", e.memberID, "lease_epoch", knownEpoch, "error", leaseErr)
			continue
		}
		if leaseErr != nil {
			slog.Error("acquire alert rule lease", "rule_id", rule.ID, "member_id", e.memberID, "error", leaseErr)
			continue
		}
		// Reload the rule after acquiring the lease so updates committed
		// between AlertEvaluationRules and AcquireAlertRuleLease cannot cause
		// a newly fenced lease to evaluate a stale expression or disabled rule.
		fresh, loadErr := e.store.AlertRuleByID(ctx, rule.ID)
		if loadErr != nil {
			if meta.IsNotFound(loadErr) {
				e.discardAcquiredLease(ctx, lease, "deleted")
			} else {
				e.discardAcquiredLease(ctx, lease, "reload_failed")
				if !errors.Is(loadErr, context.Canceled) {
					slog.Error("reload alert rule after acquiring lease", "rule_id", rule.ID,
						"member_id", e.memberID, "lease_epoch", lease.Epoch, "error", loadErr)
				}
			}
			continue
		}
		if !fresh.Enabled || fresh.EvaluationEngine != "native" {
			e.discardAcquiredLease(ctx, lease, "ineligible")
			continue
		}
		freshOwner, freshOwnerErr := ownership.Owner(
			ownership.RuleKey{ProjectID: fresh.Project.ID, Signal: fresh.Signal, RuleID: fresh.ID},
			memberIDs,
		)
		if freshOwnerErr != nil || string(freshOwner) != e.memberID {
			e.discardAcquiredLease(ctx, lease, "ownership_changed")
			continue
		}
		owned[fresh.ID] = struct{}{}
		interval := e.ruleInterval(&fresh)
		slack := e.dueSlack()
		e.mu.Lock()
		e.leases[fresh.ID] = lease
		last, hasLast := e.lastDue[fresh.ID]
		var evalNow time.Time
		if !hasLast || last.IsZero() {
			evalNow = cycleNow
		} else {
			dueAt := last.Add(interval)
			if wake, hasWake := e.wakeAt[fresh.ID]; hasWake && !wake.IsZero() && wake.Before(dueAt) {
				dueAt = wake
			}
			if !cycleNow.Add(slack).Before(dueAt) {
				evalNow = cycleNow
				if evalNow.Before(dueAt) {
					evalNow = dueAt
				}
			}
		}
		if !evalNow.IsZero() {
			e.lastDue[fresh.ID] = evalNow
			delete(e.wakeAt, fresh.ID)
		}
		e.mu.Unlock()
		if !evalNow.IsZero() {
			freshCopy := fresh
			dueTasks = append(dueTasks, evaluationTask{
				rule: &freshCopy, project: &freshCopy.Project, lease: lease, now: evalNow,
			})
		}
	}
	e.releaseUnowned(ctx, owned)
	if len(dueTasks) == 0 {
		return
	}
	workers := e.maxConcurrency
	if workers > len(dueTasks) {
		workers = len(dueTasks)
	}
	if workers <= 1 {
		for _, task := range dueTasks {
			e.evaluateRuleSafe(ctx, task.rule, task.project, task.lease, task.now)
		}
		return
	}
	sem := make(chan struct{}, workers)
	var batch sync.WaitGroup
	for _, task := range dueTasks {
		if ctx.Err() != nil {
			break
		}
		batch.Add(1)
		sem <- struct{}{}
		go func(t evaluationTask) {
			defer batch.Done()
			defer func() { <-sem }()
			e.evaluateRuleSafe(ctx, t.rule, t.project, t.lease, t.now)
		}(task)
	}
	batch.Wait()
}

func (e *RuleEvaluator) evaluateRuleSafe(
	ctx context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	lease meta.AlertRuleLease,
	now time.Time,
) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered evaluating native alert rule", "rule_id", rule.ID,
				"member_id", e.memberID, "lease_epoch", lease.Epoch, "panic", r)
			e.recordRuleOutcome(rule.ID, now, time.Since(start), errors.New("alert rule evaluation panicked"))
		}
	}()
	e.evaluateRule(ctx, rule, project, lease, now)
}

func (e *RuleEvaluator) discardAcquiredLease(ctx context.Context, lease meta.AlertRuleLease, reason string) {
	e.mu.Lock()
	delete(e.leases, lease.RuleID)
	delete(e.lastDue, lease.RuleID)
	delete(e.wakeAt, lease.RuleID)
	e.mu.Unlock()
	released, err := e.store.ReleaseAlertRuleLease(ctx, lease.RuleID, e.memberID, lease.Epoch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("release alert rule lease failed", "rule_id", lease.RuleID,
				"member_id", e.memberID, "lease_epoch", lease.Epoch, "reason", reason, "error", err)
		}
	} else if !released {
		e.leaseLossesTotal.Add(1)
		slog.Warn("release alert rule lease rejected after losing ownership", "rule_id", lease.RuleID,
			"member_id", e.memberID, "lease_epoch", lease.Epoch, "reason", reason)
	}
}

func (e *RuleEvaluator) evaluateRule(
	ctx context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	lease meta.AlertRuleLease,
	now time.Time,
) {
	start := time.Now()
	queryCtx, cancel := context.WithTimeout(ctx, e.queryTimeout)
	samples, err := e.query.Evaluate(queryCtx, rule, project, now)
	cancel()
	if err != nil {
		e.recordRuleOutcome(rule.ID, now, time.Since(start), err)
		slog.Error("query native alert rule", "rule_id", rule.ID, "duration_ms", time.Since(start).Milliseconds(),
			"error", err)
		return
	}
	previous, err := e.store.AlertRuleStates(ctx, rule.ID)
	if err != nil {
		e.recordRuleOutcome(rule.ID, now, time.Since(start), err)
		slog.Error("read alert rule state", "rule_id", rule.ID, "error", err)
		return
	}
	states, err := advanceRuleStates(rule, previous, samples, now)
	if err != nil {
		e.recordRuleOutcome(rule.ID, now, time.Since(start), err)
		slog.Error("advance native alert state", "rule_id", rule.ID, "error", err)
		return
	}
	keptFingerprints := make(map[string]struct{}, len(states))
	for _, state := range states {
		keptFingerprints[state.Fingerprint] = struct{}{}
	}
	var pruneFingerprints []string
	for _, old := range previous {
		if _, exists := keptFingerprints[old.Fingerprint]; !exists {
			pruneFingerprints = append(pruneFingerprints, old.Fingerprint)
		}
	}
	states, notifications, inhibited, planErr := e.planNativeNotifications(ctx, rule, project, previous, states, now)
	if planErr != nil {
		e.recordRuleOutcome(rule.ID, now, time.Since(start), planErr)
		slog.Error("plan native alert notifications", "rule_id", rule.ID, "error", planErr)
		return
	}
	if err := e.store.CommitNativeAlertEvaluation(ctx, meta.NativeAlertEvaluation{
		RuleID: rule.ID, MemberID: e.memberID, LeaseEpoch: lease.Epoch,
		States: states, PruneFingerprints: pruneFingerprints,
		InhibitedFingerprints: inhibited, Notifications: notifications,
	}); err != nil {
		if errors.Is(err, meta.ErrStaleRuleFence) {
			e.leaseLossesTotal.Add(1)
			e.mu.Lock()
			if current, ok := e.leases[rule.ID]; ok && current.Epoch == lease.Epoch {
				delete(e.leases, rule.ID)
				delete(e.lastDue, rule.ID)
				delete(e.wakeAt, rule.ID)
			}
			e.mu.Unlock()
			slog.Warn("discard native alert evaluation after losing rule lease", "rule_id", rule.ID,
				"member_id", e.memberID, "lease_epoch", lease.Epoch)
		} else {
			e.recordRuleOutcome(rule.ID, now, time.Since(start), err)
			slog.Error("commit native alert evaluation", "rule_id", rule.ID, "error", err)
		}
		return
	}
	if wake := earliestGroupWaitWake(states, e.groupWait, now); !wake.IsZero() {
		e.mu.Lock()
		if current, ok := e.leases[rule.ID]; ok && current.Epoch == lease.Epoch {
			e.wakeAt[rule.ID] = wake
		}
		e.mu.Unlock()
	}
	e.recordRuleOutcome(rule.ID, now, time.Since(start), nil)
	if len(notifications) != 0 && e.wakeOutbox != nil {
		e.wakeOutbox()
	}
}

func (e *RuleEvaluator) dueSlack() time.Duration {
	if e == nil || e.tickInterval <= 0 {
		return 0
	}
	slack := e.tickInterval / 10
	if slack > maxEvaluationDueSlack {
		return maxEvaluationDueSlack
	}
	return slack
}

func (e *RuleEvaluator) defaultIntervalForSignal(signal string) time.Duration {
	if signal == "logs" {
		if e != nil && e.logsInterval > 0 {
			return e.logsInterval
		}
		return defaultEvaluatorLogsInterval
	}
	if e != nil && e.metricsInterval > 0 {
		return e.metricsInterval
	}
	return defaultEvaluatorMetricsInterval
}

func (e *RuleEvaluator) ruleInterval(rule *meta.AlertRuleView) time.Duration {
	signal := ""
	if rule != nil {
		signal = rule.Signal
		if raw := strings.TrimSpace(rule.EvaluationInterval); raw != "" {
			d, err := prommodel.ParseDuration(raw)
			if err == nil && time.Duration(d) > 0 {
				e.clearInvalidIntervalWarning(rule.ID)
				return time.Duration(d)
			}
			if e.shouldWarnInvalidInterval(rule.ID, rule.EvaluationInterval) {
				slog.Warn(
					"invalid alert rule evaluation_interval; falling back to signal default",
					"rule_id", rule.ID,
					"value", rule.EvaluationInterval,
				)
			}
		} else {
			e.clearInvalidIntervalWarning(rule.ID)
		}
	}
	return e.defaultIntervalForSignal(signal)
}

func (e *RuleEvaluator) shouldWarnInvalidInterval(ruleID int, raw string) bool {
	if e == nil {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.warnedIntervals == nil {
		e.warnedIntervals = make(map[int]string)
	}
	if prev, ok := e.warnedIntervals[ruleID]; ok && prev == raw {
		return false
	}
	if len(e.warnedIntervals) >= maxWarnedInvalidIntervals {
		clear(e.warnedIntervals)
	}
	e.warnedIntervals[ruleID] = raw
	return true
}

func (e *RuleEvaluator) clearInvalidIntervalWarning(ruleID int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	delete(e.warnedIntervals, ruleID)
	e.mu.Unlock()
}

func earliestGroupWaitWake(states []meta.AlertRuleState, groupWait time.Duration, now time.Time) time.Time {
	if groupWait <= 0 {
		return time.Time{}
	}
	var earliest time.Time
	for _, st := range states {
		if st.Status != "firing" || st.FiringSince == nil || st.LastNotifiedAt != nil {
			continue
		}
		readyAt := st.FiringSince.Add(groupWait)
		if !now.Before(readyAt) {
			continue
		}
		if earliest.IsZero() || readyAt.Before(earliest) {
			earliest = readyAt
		}
	}
	return earliest
}

func (e *RuleEvaluator) releaseUnowned(ctx context.Context, owned map[int]struct{}) {
	e.mu.Lock()
	var release []meta.AlertRuleLease
	for ruleID, lease := range e.leases {
		if _, ok := owned[ruleID]; !ok {
			release = append(release, lease)
			delete(e.leases, ruleID)
			delete(e.lastDue, ruleID)
			delete(e.wakeAt, ruleID)
			delete(e.warnedIntervals, ruleID)
			delete(e.ruleStatuses, ruleID)
		}
	}
	e.mu.Unlock()
	for _, lease := range release {
		released, err := e.store.ReleaseAlertRuleLease(ctx, lease.RuleID, e.memberID, lease.Epoch)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("release alert rule lease failed", "rule_id", lease.RuleID,
					"member_id", e.memberID, "lease_epoch", lease.Epoch, "error", err)
			}
		} else if !released {
			e.leaseLossesTotal.Add(1)
			slog.Warn("release alert rule lease rejected after losing ownership", "rule_id", lease.RuleID,
				"member_id", e.memberID, "lease_epoch", lease.Epoch)
		}
	}
}

func (e *RuleEvaluator) releaseAll(ctx context.Context) {
	e.mu.Lock()
	leases := make([]meta.AlertRuleLease, 0, len(e.leases))
	for _, lease := range e.leases {
		leases = append(leases, lease)
	}
	e.leases = make(map[int]meta.AlertRuleLease)
	e.lastDue = make(map[int]time.Time)
	e.wakeAt = make(map[int]time.Time)
	e.warnedIntervals = make(map[int]string)
	e.mu.Unlock()
	for _, lease := range leases {
		released, err := e.store.ReleaseAlertRuleLease(ctx, lease.RuleID, e.memberID, lease.Epoch)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("release alert rule lease failed", "rule_id", lease.RuleID,
					"member_id", e.memberID, "lease_epoch", lease.Epoch, "error", err)
			}
		} else if !released {
			e.leaseLossesTotal.Add(1)
			slog.Warn("release alert rule lease rejected after losing ownership", "rule_id", lease.RuleID,
				"member_id", e.memberID, "lease_epoch", lease.Epoch)
		}
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
		// Only transition firing states to recovered once; pending states that
		// vanished before firing and already-recovered rows are pruned out of
		// alert_rule_states without touching alert_events again.
		if old.Status != "firing" {
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
	candidates := make([]AlertCandidate, 0, len(states))
	for i := range states {
		state := &states[i]
		old, existed := prior[state.Fingerprint]
		switch state.Status {
		case "firing":
			candidates = append(candidates, AlertCandidate{
				Alert:          alertFromRuleState(*state, "firing", time.Time{}),
				FiringSince:    state.FiringSince,
				LastNotifiedAt: state.LastNotifiedAt,
				WasFiring:      existed && old.Status == "firing",
			})
		case "recovered":
			if existed && old.Status == "firing" {
				candidates = append(candidates, AlertCandidate{
					Alert: alertFromRuleState(*state, "resolved", now),
				})
			}
		}
	}
	if len(candidates) == 0 {
		return states, nil, nil, nil
	}
	policySnap, err := e.store.AlertPolicySnapshot(ctx, p.ID, now)
	if err != nil {
		return nil, nil, nil, err
	}
	stateSources, err := e.store.ProjectFiringAlertRuleStates(ctx, p.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	platformSources, err := e.store.ProjectFiringPlatformAlertStates(ctx, p.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	ruleID := rule.ID
	compiledPolicy := CompilePolicyFromMeta(
		policySnap,
		stateSources,
		platformSources,
		nil,
		nil,
		nativeGroupKey(rule.ID),
	)
	plan := PlanNotifications(PlanConfig{
		ProjectSlug:    p.Slug,
		ProjectID:      p.ID,
		RuleName:       rule.Name,
		RuleID:         &ruleID,
		GroupKey:       nativeGroupKey(rule.ID),
		Severity:       rule.Severity,
		GroupWait:      e.groupWait,
		RepeatInterval: e.repeatInterval,
		StormThreshold: e.stormThreshold,
		Now:            now,
	}, compiledPolicy, candidates)

	for i := range states {
		if _, notified := plan.NotifiedFingerprints[states[i].Fingerprint]; notified {
			marked := now
			states[i].LastNotifiedAt = &marked
		}
	}
	outbox := BuildOutboxEntries(p.ID, &ruleID, rule.EnabledChannelIDs, plan.Notifications, now, e.repeatInterval)
	return states, outbox, plan.InhibitedFingerprints, nil
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
	ruleID := rule.ID
	return BuildOutboxEntries(p.ID, &ruleID, rule.EnabledChannelIDs, notifications, now, repeat)
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
		labels["alertname"] = rule.Name
		labels["severity"] = rule.Severity
		for key, label := range rule.Labels {
			if key == "project_id" {
				continue
			}
			labels[key] = label
		}
		// Enforce trusted project isolation label last so custom rule labels
		// or scraped metrics can never spoof another project.
		labels["project_id"] = project.Slug
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
