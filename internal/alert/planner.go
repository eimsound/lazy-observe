package alert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert/matcher"
	"github.com/eimsound/lazy-observe/internal/meta"
)

// GroupSummary represents a single active firing alert group in a project for
// storm threshold detection and summary generation.
type GroupSummary struct {
	Key      string
	Name     string
	Severity string
}

// CompiledPolicy is a precompiled, immutable snapshot of project-level alert
// routing policies (silences, inhibition rules, active inhibition sources, and
// active firing groups). Both native rules and platform alerts plan deliveries
// against the same CompiledPolicy.
type CompiledPolicy struct {
	Silences     []matcher.Set
	Inhibitions  []matcher.InhibitionRule
	Sources      []matcher.AlertRef
	ActiveGroups []GroupSummary
}

// CompilePolicy compiles in-memory silences, inhibition rules, active firing
// alerts, and active groups into a CompiledPolicy.
func CompilePolicy(
	silences [][]Matcher,
	inhibitions []InhibitionRule,
	sources []Alert,
	activeGroups []GroupSummary,
) CompiledPolicy {
	compiledSilences := matcher.CompileSetsLenient(silences)
	compiledInhibitions := make([]matcher.InhibitionRule, 0, len(inhibitions))
	for _, rule := range inhibitions {
		compiled, err := matcher.CompileInhibitionRule(rule.SourceMatchers, rule.TargetMatchers, rule.EqualLabels)
		if err != nil {
			slog.Warn("skip invalid alert inhibition rule", "error", err)
			continue
		}
		compiledInhibitions = append(compiledInhibitions, compiled)
	}
	refs := make([]matcher.AlertRef, 0, len(sources))
	for _, src := range sources {
		refs = append(refs, matcher.AlertRef{
			Fingerprint: src.Fingerprint,
			Status:      src.Status,
			Labels:      src.Labels,
		})
	}
	return CompiledPolicy{
		Silences:     compiledSilences,
		Inhibitions:  compiledInhibitions,
		Sources:      refs,
		ActiveGroups: slices.Clone(activeGroups),
	}
}

// CompilePolicyFromMeta builds a CompiledPolicy from persisted store snapshots
// across both native rules and durable platform alerts.
func CompilePolicyFromMeta(
	policy meta.AlertPolicySnapshot,
	firingRuleStates []meta.AlertRuleState,
	firingPlatformStates []meta.PlatformAlertState,
	extraSources []Alert,
	extraGroups []GroupSummary,
	excludeGroupKey string,
) CompiledPolicy {
	compiledSilences := matcher.CompileSetsFromMapsLenient(policy.Silences)
	compiledInhibitions := make([]matcher.InhibitionRule, 0, len(policy.InhibitionRules))
	for _, item := range policy.InhibitionRules {
		compiled, err := matcher.CompileInhibitionRule(
			matcher.SpecsFromMaps(item.SourceMatchers),
			matcher.SpecsFromMaps(item.TargetMatchers),
			item.EqualLabels,
		)
		if err != nil {
			slog.Warn("skip invalid persisted inhibition rule", "error", err)
			continue
		}
		compiledInhibitions = append(compiledInhibitions, compiled)
	}

	sources := make(
		[]matcher.AlertRef,
		0,
		len(firingRuleStates)+len(policy.FiringEvents)+len(firingPlatformStates)+len(extraSources),
	)
	groupsByKey := make(map[string]GroupSummary)

	for _, state := range firingRuleStates {
		a := alertFromRuleState(state, "firing", time.Time{})
		sources = append(sources, matcher.AlertRef{
			Fingerprint: a.Fingerprint,
			Status:      "firing",
			Labels:      a.Labels,
		})
		key := nativeGroupKey(state.RuleID)
		if key == excludeGroupKey {
			continue
		}
		if _, exists := groupsByKey[key]; !exists {
			name := state.Labels["alertname"]
			if name == "" {
				name = strconv.Itoa(state.RuleID)
			}
			severity := state.Labels["severity"]
			if severity == "" {
				severity = "unknown"
			}
			groupsByKey[key] = GroupSummary{Key: key, Name: name, Severity: severity}
		}
	}
	for _, event := range policy.FiringEvents {
		sources = append(sources, matcher.AlertRef{
			Fingerprint: event.Fingerprint,
			Status:      "firing",
			Labels:      event.Labels,
		})
	}
	for _, st := range firingPlatformStates {
		labels := platformLabels(st.ProjectSlug, st.Kind)
		fp := Fingerprint(labels)
		sources = append(sources, matcher.AlertRef{
			Fingerprint: fp,
			Status:      "firing",
			Labels:      labels,
		})
		key := platformGroupKey(st.Kind)
		if key == excludeGroupKey {
			continue
		}
		if _, exists := groupsByKey[key]; !exists {
			groupsByKey[key] = GroupSummary{Key: key, Name: st.Kind, Severity: "warning"}
		}
	}
	for _, src := range extraSources {
		sources = append(sources, matcher.AlertRef{
			Fingerprint: src.Fingerprint,
			Status:      src.Status,
			Labels:      src.Labels,
		})
	}
	for _, g := range extraGroups {
		if g.Key == "" || g.Key == excludeGroupKey {
			continue
		}
		if _, exists := groupsByKey[g.Key]; !exists {
			if g.Severity == "" {
				g.Severity = "unknown"
			}
			groupsByKey[g.Key] = g
		}
	}
	groups := make([]GroupSummary, 0, len(groupsByKey))
	for _, g := range groupsByKey {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Key < groups[j].Key })

	return CompiledPolicy{
		Silences:     compiledSilences,
		Inhibitions:  compiledInhibitions,
		Sources:      sources,
		ActiveGroups: groups,
	}
}

func nativeGroupKey(ruleID int) string {
	return "rule:" + strconv.Itoa(ruleID)
}

func platformGroupKey(kind string) string {
	return "platform:" + kind
}

// AlertCandidate is a single alert entering the pure notification planner.
type AlertCandidate struct {
	Alert          Alert
	FiringSince    *time.Time
	LastNotifiedAt *time.Time
	WasFiring      bool
	DueOverride    bool
}

// PlanConfig parameterizes one pure policy-planning pass for a single rule or
// platform alert group.
type PlanConfig struct {
	ProjectSlug    string
	ProjectID      int
	RuleName       string
	RuleID         *int
	GroupKey       string
	Severity       string
	GroupWait      time.Duration
	RepeatInterval time.Duration
	StormThreshold int
	Now            time.Time
}

// FiringDisposition describes the outcome for firing candidates in a plan pass.
type FiringDisposition uint8

const (
	DispositionNone FiringDisposition = iota
	DispositionNotify
	DispositionStorm
	DispositionSuppressed
	DispositionInhibited
)

// PlanResult is the deterministic output of PlanNotifications.
type PlanResult struct {
	Notifications         []Notification
	NotifiedFingerprints  map[string]struct{}
	InhibitedFingerprints map[string]bool
	FiringDisposition     FiringDisposition
	ResolvedSuppressed    bool
}

// IsFiringDue enforces uniform group_wait and repeat_interval semantics across
// native rules and platform alerts.
func IsFiringDue(
	firingSince, lastNotifiedAt *time.Time,
	wasFiring bool,
	groupWait, repeatInterval time.Duration,
	now time.Time,
) bool {
	if firingSince == nil {
		return false
	}
	if groupWait > 0 && now.Before(firingSince.Add(groupWait)) {
		return false
	}
	if !wasFiring || lastNotifiedAt == nil {
		return true
	}
	return !now.Before(lastNotifiedAt.Add(repeatInterval))
}

// BuildStormSummary returns a StormSummary when the number of active groups in
// a project strictly exceeds threshold; otherwise it returns nil.
func BuildStormSummary(groups []GroupSummary, threshold int) *StormSummary {
	if threshold <= 0 || len(groups) <= threshold {
		return nil
	}
	severityCounts := make(map[string]int, len(groups))
	ruleCounts := make(map[string]int, len(groups))
	for _, g := range groups {
		severity := g.Severity
		if severity == "" {
			severity = "unknown"
		}
		severityCounts[severity]++
		ruleCounts[g.Name]++
	}
	return &StormSummary{
		GroupCount: len(groups),
		Severities: topCounts(severityCounts),
		Rules:      topCounts(ruleCounts),
	}
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

// PlanNotifications is the single pure-function policy planner shared by native
// rule evaluation and platform alerts. It applies silencing, inhibition,
// group_wait/repeat_interval due checks, and storm summarization consistently.
func PlanNotifications(cfg PlanConfig, policy CompiledPolicy, candidates []AlertCandidate) PlanResult {
	result := PlanResult{
		NotifiedFingerprints:  make(map[string]struct{}),
		InhibitedFingerprints: make(map[string]bool),
	}
	if len(candidates) == 0 {
		return result
	}

	var firingCandidates []AlertCandidate
	var resolvedCandidates []AlertCandidate
	for _, cand := range candidates {
		switch cand.Alert.Status {
		case "firing":
			firingCandidates = append(firingCandidates, cand)
		case "resolved":
			resolvedCandidates = append(resolvedCandidates, cand)
		}
	}

	if len(firingCandidates) > 0 {
		// Combine project-wide sources with this group's firing candidates so
		// same-group or cross-group inhibition works identically everywhere.
		sources := make([]matcher.AlertRef, 0, len(policy.Sources)+len(firingCandidates))
		sources = append(sources, policy.Sources...)
		for _, cand := range firingCandidates {
			sources = append(sources, matcher.AlertRef{
				Fingerprint: cand.Alert.Fingerprint,
				Status:      "firing",
				Labels:      cand.Alert.Labels,
			})
		}

		kept := make([]Alert, 0, len(firingCandidates))
		keptHasDue := false
		silencedCount := 0
		inhibitedCount := 0
		for _, cand := range firingCandidates {
			ref := matcher.AlertRef{
				Fingerprint: cand.Alert.Fingerprint,
				Status:      "firing",
				Labels:      cand.Alert.Labels,
			}
			inhibited := matcher.IsInhibited(ref, sources, policy.Inhibitions)
			result.InhibitedFingerprints[cand.Alert.Fingerprint] = inhibited

			silenced := matcher.MatchesAny(cand.Alert.Labels, policy.Silences)
			if silenced {
				silencedCount++
				continue
			}
			if inhibited {
				inhibitedCount++
				continue
			}
			kept = append(kept, cand.Alert)
			due := cand.DueOverride
			if !due {
				firingSince := cand.FiringSince
				if firingSince == nil && !cand.Alert.StartsAt.IsZero() {
					ts := cand.Alert.StartsAt
					firingSince = &ts
				}
				due = IsFiringDue(
					firingSince,
					cand.LastNotifiedAt,
					cand.WasFiring,
					cfg.GroupWait,
					cfg.RepeatInterval,
					cfg.Now,
				)
			}
			if due {
				keptHasDue = true
			}
		}

		switch {
		case len(kept) == 0 && silencedCount == len(firingCandidates):
			result.FiringDisposition = DispositionSuppressed
		case len(kept) == 0 && inhibitedCount > 0:
			result.FiringDisposition = DispositionInhibited
		case len(kept) == 0:
			result.FiringDisposition = DispositionSuppressed
		case keptHasDue:
			groups := make([]GroupSummary, 0, len(policy.ActiveGroups)+1)
			hasCurrent := false
			for _, g := range policy.ActiveGroups {
				if cfg.GroupKey != "" && g.Key == cfg.GroupKey {
					hasCurrent = true
				}
				groups = append(groups, g)
			}
			if !hasCurrent {
				severity := cfg.Severity
				if kept[0].Labels["severity"] != "" {
					severity = kept[0].Labels["severity"]
				}
				if severity == "" {
					severity = "unknown"
				}
				groups = append(groups, GroupSummary{
					Key:      cfg.GroupKey,
					Name:     cfg.RuleName,
					Severity: severity,
				})
			}
			if storm := BuildStormSummary(groups, cfg.StormThreshold); storm != nil {
				result.Notifications = append(result.Notifications, Notification{
					Project: cfg.ProjectSlug,
					Rule:    "alert_storm",
					Status:  "firing",
					Storm:   storm,
				})
				result.FiringDisposition = DispositionStorm
			} else {
				result.Notifications = append(result.Notifications, Notification{
					Project: cfg.ProjectSlug,
					Rule:    cfg.RuleName,
					Status:  "firing",
					Alerts:  kept,
				})
				result.FiringDisposition = DispositionNotify
			}
			// Only mark alerts that actually entered a queued notification so
			// silenced or inhibited alerts trigger prompt delivery once lifted.
			for _, a := range kept {
				result.NotifiedFingerprints[a.Fingerprint] = struct{}{}
			}
		}
	}

	if len(resolvedCandidates) > 0 {
		resolved := make([]Alert, 0, len(resolvedCandidates))
		for _, cand := range resolvedCandidates {
			if !matcher.MatchesAny(cand.Alert.Labels, policy.Silences) {
				resolved = append(resolved, cand.Alert)
				result.NotifiedFingerprints[cand.Alert.Fingerprint] = struct{}{}
			}
		}
		if len(resolved) > 0 {
			result.Notifications = append(result.Notifications, Notification{
				Project: cfg.ProjectSlug,
				Rule:    cfg.RuleName,
				Status:  "resolved",
				Alerts:  resolved,
			})
		} else {
			result.ResolvedSuppressed = true
		}
	}

	return result
}

// BuildOutboxEntries constructs deterministic, idempotent notification_outbox
// rows for native rules, platform alerts, and storm summaries alike.
func BuildOutboxEntries(
	projectID int,
	ruleID *int,
	channelIDs []int,
	notifications []Notification,
	now time.Time,
	repeatInterval time.Duration,
) []meta.NotificationOutboxEnqueue {
	return BuildOutboxEntriesWithLifecycle(projectID, ruleID, 0, channelIDs, notifications, now, repeatInterval)
}

// BuildOutboxEntriesWithLifecycle constructs deterministic, idempotent
// notification_outbox rows, pinning durable issue_lifecycle_events deliveries
// (lifecycleID > 0) to `issue-lifecycle\x00<lifecycleID>\x00<rule>\x00<status>`
// per ADR 0007 so retries after partial failure or restart deduplicate cleanly.
func BuildOutboxEntriesWithLifecycle(
	projectID int,
	ruleID *int,
	lifecycleID int64,
	channelIDs []int,
	notifications []Notification,
	now time.Time,
	repeatInterval time.Duration,
) []meta.NotificationOutboxEnqueue {
	now = now.UTC()
	return buildOutboxEntries(
		projectID,
		ruleID,
		channelIDs,
		notifications,
		func(n Notification, payload []byte) []byte {
			return outboxIdempotencyPayload(projectID, ruleID, lifecycleID, n, payload, now, repeatInterval)
		},
	)
}

// PlatformStateRef identifies one durable platform_alert_states transition:
// project, kind, active_cycle, and the firing or resolved status.
type PlatformStateRef struct {
	ProjectID int
	Kind      string
	Cycle     int64
	Status    string
}

func platformStateKeyPayload(ref PlatformStateRef) []byte {
	return []byte(
		fmt.Sprintf("platform-state\x00%d\x00%s\x00%d\x00%s", ref.ProjectID, ref.Kind, ref.Cycle, ref.Status),
	)
}

// PlatformStateOutboxKey returns the deterministic notification_outbox
// idempotency key for one channel on a durable platform_alert_states
// transition.
func PlatformStateOutboxKey(ref PlatformStateRef, channelID int) string {
	return notificationOutboxKey(channelID, platformStateKeyPayload(ref))
}

// BuildPlatformStateOutboxEntries constructs notification_outbox rows for a
// durable platform_alert_states transition. The idempotency key is pinned to
// `platform-state\x00<project_id>\x00<kind>\x00<cycle>\x00<status>`
// per channel and deliberately excludes the payload, so the replica that
// committed the transition and any replica replaying it after a crash produce
// the same key even though their rendered rate annotations differ.
func BuildPlatformStateOutboxEntries(
	ref PlatformStateRef,
	channelIDs []int,
	notifications []Notification,
) []meta.NotificationOutboxEnqueue {
	keyPayload := platformStateKeyPayload(ref)
	return buildOutboxEntries(ref.ProjectID, nil, channelIDs, notifications, func(Notification, []byte) []byte {
		return keyPayload
	})
}

func buildOutboxEntries(
	projectID int,
	ruleID *int,
	channelIDs []int,
	notifications []Notification,
	keyPayloadFor func(Notification, []byte) []byte,
) []meta.NotificationOutboxEnqueue {
	if len(notifications) == 0 || len(channelIDs) == 0 {
		return nil
	}
	out := make([]meta.NotificationOutboxEnqueue, 0, len(notifications)*len(channelIDs))
	for _, notification := range notifications {
		payload, err := json.Marshal(notification)
		if err != nil {
			continue
		}
		keyPayload := keyPayloadFor(notification, payload)
		priority := 0
		if notification.Status == "resolved" {
			priority = 100
		}
		for _, channelID := range channelIDs {
			var entryRuleID *int
			if ruleID != nil {
				id := *ruleID
				entryRuleID = &id
			}
			out = append(out, meta.NotificationOutboxEnqueue{
				IdempotencyKey: notificationOutboxKey(channelID, keyPayload),
				ProjectID:      projectID,
				RuleID:         entryRuleID,
				ChannelID:      channelID,
				Payload:        payload,
				Priority:       priority,
			})
		}
	}
	return out
}

func outboxIdempotencyPayload(
	projectID int,
	ruleID *int,
	lifecycleID int64,
	notification Notification,
	payload []byte,
	now time.Time,
	repeatInterval time.Duration,
) []byte {
	if notification.Storm != nil {
		bucket := now.UnixNano()
		if repeatInterval > 0 {
			bucket = now.UnixNano() / repeatInterval.Nanoseconds()
		}
		return []byte(fmt.Sprintf("storm\x00%d\x00%d", projectID, bucket))
	}
	if ruleID != nil {
		generation := now.UnixNano()
		if notification.Status == "firing" && repeatInterval > 0 && len(notification.Alerts) > 0 {
			generation = now.Sub(notification.Alerts[0].StartsAt).Nanoseconds() / repeatInterval.Nanoseconds()
		}
		return append(slices.Clone(payload), []byte(fmt.Sprintf("\x00%d\x00%d", *ruleID, generation))...)
	}
	if lifecycleID > 0 {
		// Pin durable issue_lifecycle_events deliveries to their committed row ID
		// so retries after partial failure or worker restart deduplicate cleanly
		// even if optional AI annotations arrive or time advances between attempts.
		return []byte(
			fmt.Sprintf("issue-lifecycle\x00%d\x00%s\x00%s", lifecycleID, notification.Rule, notification.Status),
		)
	}
	// Platform notification: include rule, status, and repeat generation so
	// concurrent replicas deduplicate the same event while repeat deliveries
	// and subsequent spike cycles are never permanently swallowed.
	keyPayload := append(
		slices.Clone(payload),
		[]byte(fmt.Sprintf("\x00platform\x00%s\x00%s", notification.Rule, notification.Status))...,
	)
	if notification.Status == "firing" && repeatInterval > 0 && len(notification.Alerts) > 0 &&
		!notification.Alerts[0].StartsAt.IsZero() {
		elapsed := now.Sub(notification.Alerts[0].StartsAt.UTC())
		var generation int64
		if elapsed > 0 {
			generation = elapsed.Nanoseconds() / repeatInterval.Nanoseconds()
		}
		keyPayload = append(keyPayload, []byte(fmt.Sprintf("\x00%d", generation))...)
	}
	return keyPayload
}
