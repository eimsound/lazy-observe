package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type SilenceProvider func(context.Context, string, time.Time) ([][]Matcher, error)
type InhibitionProvider func(context.Context, string) ([]InhibitionRule, error)
type NotifyFunc func(context.Context, Notification) error

type EngineConfig struct {
	GroupWait       time.Duration
	RepeatInterval  time.Duration
	StormThreshold  int
	Now             func() time.Time
	Silences        SilenceProvider
	InhibitionRules InhibitionProvider
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

	if e.cfg.Notify == nil ||
		e.cfg.Notify(
			context.Background(),
			Notification{Project: project, Rule: "alert_storm", Status: "firing", Storm: &summary},
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

func (e *Engine) stormSummaryLocked(project string) StormSummary {
	severityCounts := make(map[string]int)
	ruleCounts := make(map[string]int)
	groups := 0
	for _, g := range e.groups {
		if g.project != project || len(g.alerts) == 0 {
			continue
		}
		groups++
		ruleCounts[g.rule]++
		severity := "unknown"
		for _, a := range g.alerts {
			if a.Labels["severity"] != "" {
				severity = a.Labels["severity"]
			}
			break
		}
		severityCounts[severity]++
	}
	return StormSummary{GroupCount: groups, Severities: topCounts(severityCounts), Rules: topCounts(ruleCounts)}
}

func topCounts(values map[string]int) []Count {
	out := make([]Count, 0, len(values))
	for name, count := range values {
		out = append(out, Count{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out
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
	case deliverySent, deliverySuppressed:
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
	if e.cfg.Silences != nil {
		sets, err := e.cfg.Silences(ctx, n.Project, e.cfg.Now())
		if err != nil {
			return deliveryFailed
		}
		kept := n.Alerts[:0]
		for _, a := range n.Alerts {
			if !matchesAny(a.Labels, sets) {
				kept = append(kept, a)
			}
		}
		n.Alerts = kept
		if len(n.Alerts) == 0 {
			return deliverySuppressed
		}
	}
	if n.Status == "firing" && e.cfg.InhibitionRules != nil {
		rules, err := e.cfg.InhibitionRules(ctx, n.Project)
		if err != nil {
			return deliveryFailed
		}
		sources := e.activeAlerts(n.Project)
		kept := n.Alerts[:0]
		for _, a := range n.Alerts {
			if !IsInhibited(a, sources, rules) {
				kept = append(kept, a)
			}
		}
		n.Alerts = kept
		if len(n.Alerts) == 0 {
			return deliveryInhibited
		}
	}
	if err := e.cfg.Notify(ctx, n); err != nil {
		return deliveryFailed
	}
	return deliverySent
}

func (e *Engine) activeAlerts(project string) []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Alert
	for _, g := range e.groups {
		if g.project == project {
			for _, a := range g.alerts {
				out = append(out, a)
			}
		}
	}
	return out
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
	for _, rule := range rules {
		if !MatchLabels(target.Labels, rule.TargetMatchers) {
			continue
		}
		for _, source := range sources {
			if source.Status == "resolved" || source.Fingerprint == target.Fingerprint ||
				!MatchLabels(source.Labels, rule.SourceMatchers) {
				continue
			}
			equal := true
			for _, label := range rule.EqualLabels {
				sv, sok := source.Labels[label]
				tv, tok := target.Labels[label]
				if !sok || !tok || sv != tv {
					equal = false
					break
				}
			}
			if equal {
				return true
			}
		}
	}
	return false
}

func matchesAny(labels map[string]string, sets [][]Matcher) bool {
	for _, matchers := range sets {
		if MatchLabels(labels, matchers) {
			return true
		}
	}
	return false
}

func MatchLabels(labels map[string]string, matchers []Matcher) bool {
	for _, m := range matchers {
		value, ok := labels[m.Name]
		if !ok {
			return false
		}
		switch strings.TrimSpace(m.Op) {
		case "=":
			if value != m.Value {
				return false
			}
		case "=~":
			re, err := regexp.Compile(m.Value)
			if err != nil || !re.MatchString(value) {
				return false
			}
		default:
			return false
		}
	}
	return len(matchers) > 0
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
