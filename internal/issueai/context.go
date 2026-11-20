package issueai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/query"
)

const UntrustedTelemetryTag = "untrusted_telemetry"

// Event holds a decoded symbolicated error event alongside its raw payload and
// extracted context map for issue investigation.
type Event struct {
	Event    errortrack.Event
	Raw      map[string]any
	Contexts map[string]any
}

// CollectContext gathers project-isolated, bounded telemetry evidence for an
// issue and packs it inside a delimiter-escaped <untrusted_telemetry> block.
func (s *Service) CollectContext(
	ctx context.Context,
	project meta.ProjectInfo,
	row meta.IssueView,
) (string, error) {
	events, err := s.Events(ctx, project, row.Fingerprint, 100)
	if err != nil {
		return "", err
	}
	sections := []ai.UntrustedSection{
		{Name: "RECENT SYMBOLICATED STACKS", Text: FormatStacks(events), Cap: 10500},
		{Name: "ISSUE", Text: FormatIssue(row), Cap: 1800},
		{Name: "OBSERVED RELEASE/DEPLOYMENT TIMELINE", Text: FormatReleases(events), Cap: 1200},
		{Name: "TAGS AND CONTEXT DISTRIBUTIONS", Text: FormatDistributions(events), Cap: 2400},
	}
	traceID := ""
	traceAt := row.LastSeen
	if len(events) > 0 {
		traceAt = events[0].Event.Timestamp
		if events[0].Event.Contexts != nil && events[0].Event.Contexts.Trace != nil {
			traceID = events[0].Event.Contexts.Trace.TraceID
		}
	}
	traceSummary, logSummary := "No trace_id on the latest event.", "No trace_id on the latest event."
	if traceID != "" {
		traceSummary = s.TraceSummary(ctx, project, traceID)
		logSummary = s.TraceLogs(ctx, project, traceID, traceAt)
	}
	sections = append(
		sections,
		ai.UntrustedSection{Name: "TRACE SUMMARY", Text: traceSummary, Cap: 3000},
		ai.UntrustedSection{Name: "TRACE ERROR/WARN LOGS (+/-2m, max 30)", Text: logSummary, Cap: 2200},
		ai.UntrustedSection{
			Name: "METRIC CHANGE QUICK CHECK (+/-30m around first_seen, max 40 points/series)",
			Text: s.MetricsQuickCheck(ctx, project, row.FirstSeen),
			Cap:  2200,
		},
	)
	return FitContextSections(sections, ContextBudget), nil
}

// FitContextSections packs prioritized untrusted telemetry sections inside
// <untrusted_telemetry>...</untrusted_telemetry> within budget bytes.
func FitContextSections(sections []ai.UntrustedSection, budget int) string {
	return ai.BuildUntrustedSections(UntrustedTelemetryTag, sections, budget)
}

// FormatIssue renders the issue metadata as compact JSON.
func FormatIssue(row meta.IssueView) string {
	value := map[string]any{
		"id":           row.ID,
		"title":        row.Title,
		"level":        row.Level,
		"status":       row.Status,
		"count":        row.Count,
		"first_seen":   row.FirstSeen,
		"last_seen":    row.LastSeen,
		"regression":   row.RegressedAt != nil,
		"regressed_at": row.RegressedAt,
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// FormatStacks renders at most the three newest symbolicated error events.
func FormatStacks(events []Event) string {
	var out strings.Builder
	if len(events) > 3 {
		events = events[:3]
	}
	for i, item := range events {
		event := item.Event
		fmt.Fprintf(
			&out,
			"event %d id=%s at=%s level=%s release=%s exception=%s: %s\n",
			i+1,
			event.EventID,
			event.Timestamp.UTC().Format(time.RFC3339Nano),
			event.Level,
			event.Release,
			event.Exception.Type,
			event.Exception.Value,
		)
		for _, frame := range event.Exception.Stacktrace.Frames {
			line := "?"
			if frame.Lineno != nil {
				line = strconv.Itoa(*frame.Lineno)
			}
			fmt.Fprintf(
				&out,
				"  at %s (%s:%s) in_app=%t symbolicated=%t\n",
				frame.Function,
				frame.Filename,
				line,
				frame.InApp,
				frame.Symbolicated,
			)
		}
	}
	if out.Len() == 0 {
		return "No recent events were available."
	}
	return out.String()
}

// FormatDistributions computes bounded tag, release, browser, and os distributions.
func FormatDistributions(events []Event) string {
	distributions := make(map[string]map[string]int)
	add := func(name, value string) {
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" || value == "" {
			return
		}
		if distributions[name] == nil {
			distributions[name] = make(map[string]int)
		}
		distributions[name][value]++
	}
	for _, item := range events {
		for key, value := range item.Event.Tags {
			add("tag."+key, value)
		}
		add("release", item.Event.Release)
		for _, name := range []string{"browser", "os"} {
			if value, ok := item.Contexts[name]; ok {
				encoded, _ := json.Marshal(value)
				add("context."+name, string(encoded))
			}
		}
	}
	keys := make([]string, 0, len(distributions))
	for key := range distributions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 20 {
		keys = keys[:20]
	}
	compact := make(map[string]map[string]int, len(keys))
	for _, key := range keys {
		compact[key] = topStringCounts(distributions[key], 10)
	}
	encoded, _ := json.Marshal(compact)
	return string(encoded)
}

// FormatReleases returns the observed release timeline from recent events.
func FormatReleases(events []Event) string {
	type releaseObservation struct {
		Release string    `json:"release"`
		SeenAt  time.Time `json:"seen_at"`
	}
	observations := make([]releaseObservation, 0, min(len(events), 20))
	seen := make(map[string]bool)
	for _, item := range events {
		release := strings.TrimSpace(item.Event.Release)
		if release == "" || seen[release] {
			continue
		}
		seen[release] = true
		observations = append(observations, releaseObservation{Release: release, SeenAt: item.Event.Timestamp.UTC()})
		if len(observations) == 20 {
			break
		}
	}
	if len(observations) == 0 {
		return "No release or deployment identifier was observed in recent events."
	}
	encoded, _ := json.Marshal(observations)
	return string(encoded)
}

func topStringCounts(values map[string]int, limit int) map[string]int {
	type pair struct {
		key   string
		count int
	}
	pairs := make([]pair, 0, len(values))
	for key, count := range values {
		pairs = append(pairs, pair{key, count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].count > pairs[j].count || (pairs[i].count == pairs[j].count && pairs[i].key < pairs[j].key)
	})
	if len(pairs) > limit {
		pairs = pairs[:limit]
	}
	result := make(map[string]int, len(pairs))
	for _, item := range pairs {
		result[item.key] = item.count
	}
	return result
}

// Events fetches bounded deduplicated error events for an issue via ProjectQuerier.
func (s *Service) Events(
	ctx context.Context,
	project meta.ProjectInfo,
	fingerprint string,
	limit int,
) ([]Event, error) {
	if s == nil || s.querier == nil {
		return nil, fmt.Errorf("project querier is unavailable")
	}
	expr := query.IssueEventsLogsQL(fingerprint, "", limit)
	body, err := s.querier.Query(
		ctx,
		project,
		query.SignalLogs,
		"/select/logsql/query",
		url.Values{"query": {expr}, "limit": {strconv.Itoa(limit)}},
	)
	if err != nil {
		return nil, err
	}
	result := make([]Event, 0, limit)
	seen := make(map[string]struct{})
	err = query.ReadVLogsLines(bytes.NewReader(body), s.maxErrorLineBytes, func(line []byte) {
		var record map[string]json.RawMessage
		if json.Unmarshal(line, &record) != nil {
			return
		}
		var symbolicated, rawText string
		_ = json.Unmarshal(record["symbolicated_event"], &symbolicated)
		_ = json.Unmarshal(record["raw_event"], &rawText)
		var event errortrack.Event
		if json.Unmarshal([]byte(symbolicated), &event) != nil {
			return
		}
		if event.EventID != "" {
			if _, dup := seen[event.EventID]; dup {
				return
			}
			seen[event.EventID] = struct{}{}
		}
		var raw map[string]any
		_ = json.Unmarshal([]byte(rawText), &raw)
		contexts, _ := raw["contexts"].(map[string]any)
		result = append(result, Event{Event: event, Raw: raw, Contexts: contexts})
	})
	return result, err
}

// TraceSummary fetches and summarizes a trace using strict hex trace_id validation.
func (s *Service) TraceSummary(ctx context.Context, project meta.ProjectInfo, traceID string) string {
	traceID = strings.TrimSpace(traceID)
	if !query.ValidateTraceID(traceID) {
		return "Trace lookup skipped: invalid trace_id."
	}
	if s == nil || s.querier == nil {
		return "Trace lookup unavailable."
	}
	body, err := s.querier.Query(
		ctx,
		project,
		query.SignalTraces,
		"/select/jaeger/api/traces/"+url.PathEscape(traceID),
		nil,
	)
	if err != nil {
		return "Trace lookup unavailable."
	}
	var response struct {
		Data []struct {
			Spans []struct {
				SpanID        string                             `json:"spanID"`
				OperationName string                             `json:"operationName"`
				Duration      int64                              `json:"duration"`
				References    []struct{ RefType, SpanID string } `json:"references"`
				Tags          []struct {
					Key   string
					Value any
				} `json:"tags"`
			} `json:"spans"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Data) == 0 {
		return "Trace lookup returned no decodable trace."
	}
	var out strings.Builder
	fmt.Fprintf(&out, "trace_id=%s spans=%d\n", traceID, len(response.Data[0].Spans))
	for _, span := range response.Data[0].Spans {
		parent, isError := "root", false
		for _, ref := range span.References {
			if strings.EqualFold(ref.RefType, "CHILD_OF") {
				parent = ref.SpanID
			}
		}
		for _, tag := range span.Tags {
			if tag.Key == "error" && fmt.Sprint(tag.Value) == "true" ||
				tag.Key == "otel.status_code" && strings.EqualFold(fmt.Sprint(tag.Value), "ERROR") {
				isError = true
			}
		}
		fmt.Fprintf(
			&out,
			"- span=%s parent=%s operation=%q duration_us=%d error=%t\n",
			span.SpanID,
			parent,
			span.OperationName,
			span.Duration,
			isError,
		)
	}
	return out.String()
}

// TraceLogs fetches up to 30 error/warn logs around the trace timestamp.
func (s *Service) TraceLogs(ctx context.Context, project meta.ProjectInfo, traceID string, at time.Time) string {
	if s == nil || s.querier == nil {
		return "Log lookup unavailable."
	}
	expr := `(level:="error" OR level:="warn" OR severity_text:="ERROR" OR severity_text:="WARN") AND trace_id:=` +
		strconv.Quote(traceID)
	values := url.Values{
		"query": {expr},
		"limit": {"30"},
		"start": {at.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)},
		"end":   {at.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)},
	}
	body, err := s.querier.Query(ctx, project, query.SignalLogs, "/select/logsql/query", values)
	if err != nil {
		return "Log lookup unavailable."
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) > 30 {
		lines = lines[:30]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "No error/warn logs found for this trace window."
	}
	return strings.Join(lines, "\n")
}

// MetricsQuickCheck fetches downsampled HTTP error and request rate series
// around firstSeen.
func (s *Service) MetricsQuickCheck(ctx context.Context, project meta.ProjectInfo, firstSeen time.Time) string {
	start, end := firstSeen.Add(-30*time.Minute), firstSeen.Add(30*time.Minute)
	queries := []struct{ name, expr string }{
		{"error_rate", `sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))`},
		{"request_rate", `sum(rate(http_server_request_duration_seconds_count[5m]))`},
	}
	result := make(map[string]any, len(queries))
	for _, item := range queries {
		if s == nil || s.querier == nil {
			result[item.name] = map[string]string{"unavailable": "query failed"}
			continue
		}
		body, err := s.querier.Query(
			ctx,
			project,
			query.SignalMetrics,
			"/api/v1/query_range",
			url.Values{
				"query": {item.expr},
				"start": {start.UTC().Format(time.RFC3339)},
				"end":   {end.UTC().Format(time.RFC3339)},
				"step":  {"90"},
			},
		)
		if err != nil {
			result[item.name] = map[string]string{"unavailable": "query failed"}
			continue
		}
		result[item.name] = CompactMetricResponse(body, 40)
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

// CompactMetricResponse downsamples Prometheus range query series to at most
// maxPoints points per series while keeping both endpoints.
func CompactMetricResponse(body []byte, maxPoints int) any {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || response.Status != "success" {
		return map[string]string{"unavailable": "invalid metrics response"}
	}
	for i := range response.Data.Result {
		values := response.Data.Result[i].Values
		if len(values) <= maxPoints {
			continue
		}
		step := float64(len(values)-1) / float64(maxPoints-1)
		downsampled := make([][]any, 0, maxPoints)
		for point := 0; point < maxPoints; point++ {
			downsampled = append(downsampled, values[int(float64(point)*step)])
		}
		response.Data.Result[i].Values = downsampled
	}
	return response.Data.Result
}
