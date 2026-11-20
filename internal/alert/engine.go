package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert/matcher"
)

type SilenceProvider func(context.Context, string, time.Time) ([][]Matcher, error)
type InhibitionProvider func(context.Context, string) ([]InhibitionRule, error)
type PolicyProvider func(
	ctx context.Context,
	project string,
	now time.Time,
	localSources []Alert,
	localGroups []GroupSummary,
	excludeGroupKey string,
) (CompiledPolicy, error)
type NotifyFunc func(context.Context, Notification) error

type EngineConfig struct {
	GroupWait       time.Duration
	RepeatInterval  time.Duration
	StormThreshold  int
	Now             func() time.Time
	Silences        SilenceProvider
	InhibitionRules InhibitionProvider
	Policy          PolicyProvider
	Notify          NotifyFunc
}

type group struct {
	project, rule string
	alerts        map[string]Alert
	timer         *time.Timer
	lastSent      time.Time
}

type stormState struct {
	timer    *time.Timer
	lastSent time.Time
}

type Engine struct {
	mu     sync.Mutex
	groups map[string]*group
	storms map[string]*stormState
	cfg    EngineConfig
}

func NewEngine(cfg EngineConfig) *Engine {
	if cfg.GroupWait <= 0 {
		cfg.GroupWait = 30 * time.Second
	}
	if cfg.RepeatInterval <= 0 {
		cfg.RepeatInterval = 4 * time.Hour
	}
	if cfg.StormThreshold <= 0 {
		cfg.StormThreshold = 10
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{groups: make(map[string]*group), storms: make(map[string]*stormState), cfg: cfg}
}

func Fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (e *Engine) Add(ctx context.Context, project, rule string, a Alert) {
	if a.Fingerprint == "" {
		a.Fingerprint = Fingerprint(a.Labels)
	}
	key := project + "\x00" + rule
	if a.Status == "resolved" {
		e.mu.Lock()
		if g := e.groups[key]; g != nil {
			delete(g.alerts, a.Fingerprint)
			if len(g.alerts) == 0 {
				stopTimer(g.timer)
				delete(e.groups, key)
			}
		}
		e.updateStormLocked(project)
		// Resolving an inhibition source makes all still-firing targets eligible
		// for a fresh notification evaluation.
		if e.storms[project] == nil {
			e.scheduleProjectLocked(project, true)
		}
		e.mu.Unlock()
		e.deliver(ctx, Notification{Project: project, Rule: rule, Status: "resolved", Alerts: []Alert{a}})
		return
	}

	e.mu.Lock()
	g := e.groups[key]
	if g == nil {
		g = &group{project: project, rule: rule, alerts: make(map[string]Alert)}
		e.groups[key] = g
	}
	if _, duplicate := g.alerts[a.Fingerprint]; duplicate {
		if e.storms[project] == nil && !g.lastSent.IsZero() && e.cfg.Now().Sub(g.lastSent) >= e.cfg.RepeatInterval &&
			g.timer == nil {
			e.scheduleGroupLocked(key, g)
		}
		e.mu.Unlock()
		return
	}
	g.alerts[a.Fingerprint] = a
	e.updateStormLocked(project)
	if e.storms[project] == nil && g.timer == nil {
		e.scheduleGroupLocked(key, g)
	}
	e.mu.Unlock()
}

func (e *Engine) scheduleGroupLocked(key string, g *group) {
	g.timer = time.AfterFunc(e.cfg.GroupWait, func() { e.flush(key) })
}

func (e *Engine) scheduleProjectLocked(project string, force bool) {
	for key, g := range e.groups {
		if g.project != project || g.timer != nil {
			continue
		}
		if force || g.lastSent.IsZero() || e.cfg.Now().Sub(g.lastSent) >= e.cfg.RepeatInterval {
			e.scheduleGroupLocked(key, g)
		}
	}
}

func (e *Engine) updateStormLocked(project string) {
	count := e.projectGroupCountLocked(project)
	state := e.storms[project]
	if state == nil && count > e.cfg.StormThreshold {
		state = &stormState{}
		e.storms[project] = state
		for _, g := range e.groups {
			if g.project == project {
				stopTimer(g.timer)
				g.timer = nil
			}
		}
		e.scheduleStormLocked(project, state, e.cfg.GroupWait)
		return
	}
	if state != nil && count <= e.cfg.StormThreshold {
		stopTimer(state.timer)
		delete(e.storms, project)
		e.scheduleProjectLocked(project, false)
	}
}

func (e *Engine) projectGroupCountLocked(project string) int {
	count := 0
	for _, g := range e.groups {
		if g.project == project && len(g.alerts) > 0 {
			count++
		}
	}
	return count
}

func (e *Engine) scheduleStormLocked(project string, state *stormState, delay time.Duration) {
	state.timer = time.AfterFunc(delay, func() { e.flushStorm(project) })
}

func (e *Engine) flushStorm(project string) {
	e.mu.Lock()
	state := e.storms[project]
	if state == nil {
		e.mu.Unlock()
		return
	}
	state.timer = nil
	summary := e.stormSummaryLocked(project)
	e.mu.Unlock()

	if summary == nil {
		return
	}
	if e.cfg.Notify == nil ||
		e.cfg.Notify(
			context.Background(),
			Notification{Project: project, Rule: "alert_storm", Status: "firing", Storm: summary},
		) == nil {
		e.mu.Lock()
		if current := e.storms[project]; current != nil {
			current.lastSent = e.cfg.Now()
			e.scheduleStormLocked(project, current, e.cfg.RepeatInterval)
		}
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	if current := e.storms[project]; current != nil && current.timer == nil {
		e.scheduleStormLocked(project, current, e.cfg.GroupWait)
	}
	e.mu.Unlock()
}

func (e *Engine) projectGroupsLocked(project string) []GroupSummary {
	groups := make([]GroupSummary, 0)
	for _, g := range e.groups {
		if g.project != project || len(g.alerts) == 0 {
			continue
		}
		severity := "unknown"
		for _, a := range g.alerts {
			if a.Labels["severity"] != "" {
				severity = a.Labels["severity"]
			}
			break
		}
		groups = append(groups, GroupSummary{
			Key:      platformGroupKey(g.rule),
			Name:     g.rule,
			Severity: severity,
		})
	}
	return groups
}

func (e *Engine) stormSummaryLocked(project string) *StormSummary {
	return BuildStormSummary(e.projectGroupsLocked(project), e.cfg.StormThreshold)
}

type deliveryResult uint8

const (
	deliverySent deliveryResult = iota
	deliverySuppressed
	deliveryInhibited
	deliveryFailed
)

func (e *Engine) flush(key string) {
	e.mu.Lock()
	g := e.groups[key]
	if g == nil || e.storms[g.project] != nil {
		e.mu.Unlock()
		return
	}
	g.timer = nil
	alerts := make([]Alert, 0, len(g.alerts))
	for _, a := range g.alerts {
		alerts = append(alerts, a)
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Fingerprint < alerts[j].Fingerprint })
	project, rule := g.project, g.rule
	e.mu.Unlock()

	result := e.deliver(
		context.Background(),
		Notification{Project: project, Rule: rule, Status: "firing", Alerts: alerts},
	)
	e.mu.Lock()
	defer e.mu.Unlock()
	current := e.groups[key]
	if current == nil {
		return
	}
	switch result {
	case deliverySent:
		current.lastSent = e.cfg.Now()
		for _, a := range alerts {
			if a.Transient {
				delete(current.alerts, a.Fingerprint)
			}
		}
		if len(current.alerts) == 0 {
			delete(e.groups, key)
			e.updateStormLocked(project)
		}
	case deliverySuppressed:
		// Do not advance lastSent when alerts were silenced so un-silenced
		// persistent alerts are not delayed for the full repeat interval.
		for _, a := range alerts {
			if a.Transient {
				delete(current.alerts, a.Fingerprint)
			}
		}
		if len(current.alerts) == 0 {
			delete(e.groups, key)
			e.updateStormLocked(project)
		}
	case deliveryFailed:
		e.scheduleGroupLocked(key, current)
	case deliveryInhibited:
		// Keep it unsent. A source resolution calls scheduleProjectLocked.
	}
}

func (e *Engine) deliver(ctx context.Context, n Notification) deliveryResult {
	if e.cfg.Notify == nil {
		return deliverySent
	}
	now := e.cfg.Now()
	groupKey := platformGroupKey(n.Rule)
	policy, err := e.loadPolicy(ctx, n.Project, now, groupKey)
	if err != nil {
		return deliveryFailed
	}

	candidates := make([]AlertCandidate, 0, len(n.Alerts))
	for _, a := range n.Alerts {
		if a.Status == "" {
			a.Status = n.Status
		}
		candidates = append(candidates, AlertCandidate{
			Alert:       a,
			DueOverride: true,
		})
	}

	// Only pass storm threshold to planner when backed by a project-wide
	// PolicyProvider; standalone Engine instances manage local storms via
	// updateStormLocked/flushStorm.
	stormThreshold := 0
	if e.cfg.Policy != nil {
		stormThreshold = e.cfg.StormThreshold
	}
	plan := PlanNotifications(PlanConfig{
		ProjectSlug:    n.Project,
		RuleName:       n.Rule,
		GroupKey:       groupKey,
		GroupWait:      e.cfg.GroupWait,
		RepeatInterval: e.cfg.RepeatInterval,
		StormThreshold: stormThreshold,
		Now:            now,
	}, policy, candidates)

	if len(plan.Notifications) == 0 {
		if n.Status == "resolved" && plan.ResolvedSuppressed {
			return deliverySuppressed
		}
		switch plan.FiringDisposition {
		case DispositionInhibited:
			return deliveryInhibited
		case DispositionSuppressed:
			return deliverySuppressed
		default:
			return deliverySuppressed
		}
	}

	for _, planned := range plan.Notifications {
		if err := e.cfg.Notify(ctx, planned); err != nil {
			return deliveryFailed
		}
	}
	return deliverySent
}

func (e *Engine) loadPolicy(
	ctx context.Context,
	project string,
	now time.Time,
	excludeGroupKey string,
) (CompiledPolicy, error) {
	sources, groups := e.activeSnapshot(project)
	if e.cfg.Policy != nil {
		return e.cfg.Policy(ctx, project, now, sources, groups, excludeGroupKey)
	}
	var silences [][]Matcher
	if e.cfg.Silences != nil {
		sets, err := e.cfg.Silences(ctx, project, now)
		if err != nil {
			return CompiledPolicy{}, err
		}
		silences = sets
	}
	var inhibitions []InhibitionRule
	if e.cfg.InhibitionRules != nil {
		rules, err := e.cfg.InhibitionRules(ctx, project)
		if err != nil {
			return CompiledPolicy{}, err
		}
		inhibitions = rules
	}
	return CompilePolicy(silences, inhibitions, sources, groups), nil
}

func (e *Engine) activeSnapshot(project string) ([]Alert, []GroupSummary) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var alerts []Alert
	for _, g := range e.groups {
		if g.project == project {
			for _, a := range g.alerts {
				alerts = append(alerts, a)
			}
		}
	}
	return alerts, e.projectGroupsLocked(project)
}

// Remove drops a group without delivering a resolved notification. It is used
// when a platform alert configuration is disabled or reset.
func (e *Engine) Remove(project, rule string) {
	key := project + "\x00" + rule
	e.mu.Lock()
	defer e.mu.Unlock()
	if g := e.groups[key]; g != nil {
		stopTimer(g.timer)
		delete(e.groups, key)
		e.updateStormLocked(project)
	}
}

// Reevaluate schedules active groups that have never been sent. It is used
// when inhibition configuration changes without a new alert transition.
func (e *Engine) Reevaluate(project string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.storms[project] == nil {
		e.scheduleProjectLocked(project, false)
	}
}

func IsInhibited(target Alert, sources []Alert, rules []InhibitionRule) bool {
	policy := CompilePolicy(nil, rules, sources, nil)
	return matcher.IsInhibited(
		matcher.AlertRef{Fingerprint: target.Fingerprint, Status: target.Status, Labels: target.Labels},
		policy.Sources,
		policy.Inhibitions,
	)
}

func matchesAny(labels map[string]string, sets [][]Matcher) bool {
	return matcher.MatchesAny(labels, matcher.CompileSetsLenient(sets))
}

// CompileMatcherRegex compiles a Prometheus/Alertmanager matcher regex anchored
// to match the entire label value (`^(?:...)$`) via the bounded regex cache.
func CompileMatcherRegex(pattern string) (*regexp.Regexp, error) {
	return matcher.CompileRegex(pattern)
}

// ValidateMatchers verifies that a matcher slice is non-empty and every entry
// has a non-empty label name, supported operator (`=` or `=~`), and valid regex.
func ValidateMatchers(matchers []Matcher) error {
	return matcher.ValidateSet(matchers)
}

func MatchLabels(labels map[string]string, matchers []Matcher) bool {
	set, err := matcher.CompileSet(matchers)
	if err != nil {
		return false
	}
	return set.Matches(labels)
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}

func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, g := range e.groups {
		stopTimer(g.timer)
	}
	for _, state := range e.storms {
		stopTimer(state.timer)
	}
}
