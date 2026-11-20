package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	issueAIContextBudget = 24 << 10
	issueAIRateLimit     = 6
	issueAIQueueSize     = 128
	issueAITimeout       = 30 * time.Second
	issueAIEvidenceWait  = 5 * time.Second
)

type issueAICause struct {
	Cause      string `json:"cause"`
	Confidence string `json:"confidence"`
	Evidence   string `json:"evidence"`
}

type issueAIAnalysis struct {
	Summary          string         `json:"summary"`
	ProbableCauses   []issueAICause `json:"probable_causes"`
	SuggestedActions []string       `json:"suggested_actions"`
}

type issueAIService struct {
	gateway  *Gateway
	outcomes metric.Int64Counter
	jobs     chan issueAIJob
	cancel   context.CancelFunc
	done     chan struct{}
	enabled  bool
	close    sync.Once
}

func newIssueAIService(g *Gateway) *issueAIService {
	service := &issueAIService{
		gateway:  g,
		done:     make(chan struct{}),
		outcomes: newIssueAIOutcomeCounter(g.meterProvider),
	}
	state := g.aiState()
	dynamicAI := g.ai != nil && g.ai.store != nil
	if g.issues == nil || g.store == nil || (!dynamicAI && (!state.enabled || state.client == nil)) {
		close(service.done)
		return service
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.jobs = make(chan issueAIJob, issueAIQueueSize)
	service.cancel = cancel
	service.enabled = true
	go service.run(ctx)
	return service
}

type issueAIJob struct {
	projectID int
	issueID   int
}

// HandleIssueChange keeps AI outside the ingest path: the committed new Issue
// is queued without waiting for storage queries or the configured LLM. A full
// queue degrades to manual generation instead of blocking error ingestion.
func (s *issueAIService) HandleIssueChange(_ context.Context, change errortrack.IssueChange) error {
	if s == nil || !s.enabled || change.Kind != "new_issue" {
		return nil
	}
	state := s.gateway.aiState()
	if !state.enabled || state.client == nil {
		return nil
	}
	select {
	case s.jobs <- issueAIJob{projectID: change.ProjectID, issueID: change.IssueID}:
	default:
		s.record("queue_full")
		s.gateway.logger.Warn(
			"automatic issue AI analysis queue full",
			"project_id",
			change.ProjectID,
			"issue_id",
			change.IssueID,
		)
	}
	return nil
}

func (s *issueAIService) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			jobCtx, cancel := context.WithTimeout(ctx, issueAITimeout)
			s.process(jobCtx, job)
			cancel()
		}
	}
}

func (s *issueAIService) process(ctx context.Context, job issueAIJob) {
	state := s.gateway.aiState()
	if !state.enabled || state.client == nil {
		return
	}
	if !s.allow(ctx, job.projectID) {
		s.record("rate_limited")
		s.gateway.logger.Warn(
			"automatic issue AI analysis skipped",
			"project_id",
			job.projectID,
			"issue_id",
			job.issueID,
			"stage",
			"rate_limit",
		)
		return
	}
	projectRow, err := s.gateway.store.GetProject(ctx, job.projectID)
	if err != nil {
		s.gateway.logger.Warn(
			"automatic issue AI analysis failed",
			"project_id",
			job.projectID,
			"issue_id",
			job.issueID,
			"stage",
			"project",
			"error",
			err,
		)
		s.record("failed")
		return
	}
	project := meta.ProjectInfo{ID: projectRow.ID, Name: projectRow.Name, Slug: projectRow.Slug}
	row, err := s.gateway.issues.GetIssue(ctx, project.ID, job.issueID)
	if err != nil {
		s.gateway.logger.Warn(
			"automatic issue AI analysis failed",
			"project_id",
			project.ID,
			"issue_id",
			job.issueID,
			"stage",
			"issue",
			"error",
			err,
		)
		s.record("failed")
		return
	}
	if row.AIAnalysisAt != nil && row.AIAnalysis != nil {
		return
	}
	s.waitForEvidence(ctx, project, row.Fingerprint)
	if _, _, err = s.gateway.generateAndStoreIssueAI(ctx, project, row); err != nil {
		s.gateway.logger.Warn(
			"automatic issue AI analysis failed",
			"project_id",
			project.ID,
			"issue_id",
			row.ID,
			"stage",
			"analysis",
			"error",
			err,
		)
		s.record("failed")
		return
	}
	s.record("completed")
	s.gateway.logger.Info("automatic issue AI analysis completed", "project_id", project.ID, "issue_id", row.ID)
}

func (s *issueAIService) waitForEvidence(ctx context.Context, project meta.ProjectInfo, fingerprint string) {
	deadline := time.NewTimer(issueAIEvidenceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if events, err := s.gateway.issueAIEvents(ctx, project, fingerprint, 1); err == nil && len(events) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func (s *issueAIService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.close.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gateway) HandleIssueChange(ctx context.Context, change errortrack.IssueChange) error {
	if g.issueAI != nil {
		_ = g.issueAI.HandleIssueChange(ctx, change)
	}
	if g.alerts != nil {
		return g.alerts.HandleIssueChange(ctx, change)
	}
	return nil
}

func (g *Gateway) HandleErrorEvent(ctx context.Context, occurrence errortrack.ErrorOccurrence) error {
	if g.alerts == nil {
		return nil
	}
	return g.alerts.RecordErrorEvent(ctx, occurrence.ProjectID, occurrence.OccurredAt)
}

func (s *issueAIService) allow(ctx context.Context, projectID int) bool {
	allowed, err := s.gateway.limits.Allow(
		ctx,
		"issue-ai:project:"+strconv.Itoa(projectID),
		issueAIRateLimit,
		time.Minute,
	)
	return err == nil && allowed
}

func (g *Gateway) getIssueAIAnalysis(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	row, err := g.issues.GetIssue(c.Context(), currentProject(c).ID, id)
	if err != nil {
		return apiError(c, err)
	}
	if row.AIAnalysisAt == nil || row.AIAnalysis == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "AI analysis not found"})
	}
	g.auditIssueAI(c, id, true, false)
	return c.JSON(issueAIResponse(row, true))
}

func (g *Gateway) postIssueAIAnalysis(c fiber.Ctx) error {
	if !g.assistantReady(c) || !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	project := currentProject(c)
	row, err := g.issues.GetIssue(c.Context(), project.ID, id)
	if err != nil {
		return apiError(c, err)
	}
	refresh := c.Query("refresh") == "1"
	if !refresh && row.AIAnalysisAt != nil && row.AIAnalysis != nil {
		g.auditIssueAI(c, id, true, false)
		return c.JSON(issueAIResponse(row, true))
	}
	if !g.issueAI.allow(c.Context(), project.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": "Issue AI analysis rate limit exceeded"})
	}
	row, raw, err := g.generateAndStoreIssueAI(c.Context(), project, row)
	if err != nil {
		var contractErr *issueAIContractError
		if errors.As(err, &contractErr) {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": "AI output did not match the analysis contract", "raw_output": firstRunes(raw, 500)})
		}
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI issue analysis failed"})
	}
	g.auditIssueAI(c, id, false, refresh)
	return c.JSON(issueAIResponse(row, false))
}

func (g *Gateway) generateAndStoreIssueAI(
	ctx context.Context,
	project meta.ProjectInfo,
	row meta.IssueView,
) (meta.IssueView, string, error) {
	contextText, err := g.collectIssueAIContext(ctx, project, row)
	if err != nil {
		return meta.IssueView{}, "", err
	}
	analysis, raw, err := g.generateIssueAIAnalysis(ctx, contextText)
	if err != nil {
		return meta.IssueView{}, raw, err
	}
	encoded, _ := json.Marshal(analysis)
	var stored map[string]any
	_ = json.Unmarshal(encoded, &stored)
	storedRow, err := g.issues.SaveIssueAnalysis(ctx, project.ID, row.ID, stored, g.now().UTC())
	return storedRow, raw, err
}

func (g *Gateway) auditIssueAI(c fiber.Ctx, issueID int, cached, refresh bool) {
	user, _ := c.Locals(userLocal).(meta.UserInfo)
	project := currentProject(c)
	g.logger.Info(
		"issue AI analysis accessed",
		"user_id",
		user.ID,
		"username",
		user.Username,
		"project_id",
		project.ID,
		"project",
		project.Slug,
		"issue_id",
		issueID,
		"cached",
		cached,
		"refresh",
		refresh,
	)
}

func issueAIResponse(row meta.IssueView, cached bool) fiber.Map {
	response := fiber.Map{"cached": cached, "ai_analysis_at": row.AIAnalysisAt}
	for key, value := range row.AIAnalysis {
		response[key] = value
	}
	if row.AIAnalysisAt != nil && row.LastSeen.After(*row.AIAnalysisAt) {
		response["stale"] = true
	}
	return response
}

type issueAIContractError struct{}

func (*issueAIContractError) Error() string { return "invalid AI issue analysis" }

func (g *Gateway) generateIssueAIAnalysis(ctx context.Context, contextText string) (issueAIAnalysis, string, error) {
	const system = `You are an SRE investigation assistant for lazy-observe. Correlate error events, symbolicated stacks, observed release/deployment changes, tags, trace spans, nearby logs, and metric changes. Distinguish facts from hypotheses. Return only one JSON object with exactly this shape: {"summary":"at most 150 characters","probable_causes":[{"cause":"...","confidence":"high|medium|low","evidence":"..."}],"suggested_actions":["..."]}. Do not use Markdown, do not invent evidence, and do not include secrets or remediation code changes.`
	messages := []ai.Message{{Role: "user", Content: contextText}}
	state := g.aiState()
	if !state.enabled || state.client == nil {
		return issueAIAnalysis{}, "", errors.New("AI integration is disabled")
	}
	var raw string
	for attempt := 0; attempt < 2; attempt++ {
		response, err := state.client.Chat(ctx, ai.ChatRequest{System: system, Messages: messages, Temperature: 0})
		if err != nil {
			return issueAIAnalysis{}, raw, err
		}
		raw = response.Content
		analysis, parseErr := parseIssueAIAnalysis(raw)
		if parseErr == nil {
			return analysis, raw, nil
		}
		messages = append(
			messages,
			ai.Message{Role: "assistant", Content: firstRunes(raw, 1000)},
			ai.Message{
				Role:    "user",
				Content: "Your response violated the JSON contract. Return only one corrected JSON object with summary, probable_causes, and suggested_actions.",
			},
		)
	}
	return issueAIAnalysis{}, raw, &issueAIContractError{}
}

func parseIssueAIAnalysis(raw string) (issueAIAnalysis, error) {
	for i, r := range raw {
		if r != '{' {
			continue
		}
		var result issueAIAnalysis
		decoder := json.NewDecoder(strings.NewReader(raw[i:]))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil {
			continue
		}
		result.Summary = strings.TrimSpace(result.Summary)
		if result.Summary == "" || utf8.RuneCountInString(result.Summary) > 150 || len(result.ProbableCauses) == 0 ||
			len(result.ProbableCauses) > 5 ||
			len(result.SuggestedActions) == 0 ||
			len(result.SuggestedActions) > 8 {
			continue
		}
		valid := true
		for j := range result.ProbableCauses {
			cause := &result.ProbableCauses[j]
			cause.Cause, cause.Confidence, cause.Evidence = strings.TrimSpace(
				cause.Cause,
			), strings.TrimSpace(
				cause.Confidence,
			), strings.TrimSpace(
				cause.Evidence,
			)
			if cause.Cause == "" || utf8.RuneCountInString(cause.Cause) > 300 || cause.Evidence == "" ||
				utf8.RuneCountInString(cause.Evidence) > 500 ||
				(cause.Confidence != "high" && cause.Confidence != "medium" && cause.Confidence != "low") {
				valid = false
			}
		}
		for j := range result.SuggestedActions {
			result.SuggestedActions[j] = strings.TrimSpace(result.SuggestedActions[j])
			valid = valid && result.SuggestedActions[j] != "" &&
				utf8.RuneCountInString(result.SuggestedActions[j]) <= 500
		}
		if valid {
			return result, nil
		}
	}
	return issueAIAnalysis{}, errors.New("no valid issue AI analysis JSON object")
}

type issueAIEvent struct {
	Event    errortrack.Event
	Raw      map[string]any
	Contexts map[string]any
}

func (g *Gateway) collectIssueAIContext(
	ctx context.Context,
	project meta.ProjectInfo,
	row meta.IssueView,
) (string, error) {
	// Use a bounded recent sample for distributions, while retaining stacks for
	// only the latest three events below.
	events, err := g.issueAIEvents(ctx, project, row.Fingerprint, 100)
	if err != nil {
		return "", err
	}
	sections := []contextSection{
		{"RECENT SYMBOLICATED STACKS", issueAIStacks(events), 10500},
		{"ISSUE", issueAIIssue(row), 1800},
		{"OBSERVED RELEASE/DEPLOYMENT TIMELINE", issueAIReleases(events), 1200},
		{"TAGS AND CONTEXT DISTRIBUTIONS", issueAIDistributions(events), 2400},
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
		traceSummary = g.issueAITrace(ctx, project, traceID)
		logSummary = g.issueAILogs(ctx, project, traceID, traceAt)
	}
	sections = append(
		sections,
		contextSection{"TRACE SUMMARY", traceSummary, 3000},
		contextSection{"TRACE ERROR/WARN LOGS (+/-2m, max 30)", logSummary, 2200},
		contextSection{
			"METRIC CHANGE QUICK CHECK (+/-30m around first_seen, max 40 points/series)",
			g.issueAIMetrics(ctx, project, row.FirstSeen),
			2200,
		},
	)
	return fitContextSections(sections, issueAIContextBudget), nil
}

type contextSection struct {
	name string
	text string
	cap  int
}

func fitContextSections(sections []contextSection, budget int) string {
	var result strings.Builder
	for _, section := range sections {
		piece := "\n== " + section.name + " ==\n" + clipUTF8(section.text, section.cap)
		remaining := budget - result.Len()
		if remaining <= 0 {
			break
		}
		result.WriteString(clipUTF8(piece, remaining))
	}
	return strings.TrimSpace(result.String())
}

func clipUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	const suffix = "\n...[truncated]"
	limit -= len(suffix)
	if limit < 0 {
		return suffix[:max(0, limit+len(suffix))]
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + suffix
}

func issueAIIssue(row meta.IssueView) string {
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

func issueAIStacks(events []issueAIEvent) string {
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

func issueAIDistributions(events []issueAIEvent) string {
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

func issueAIReleases(events []issueAIEvent) string {
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

func (g *Gateway) issueAIEvents(
	ctx context.Context,
	project meta.ProjectInfo,
	fingerprint string,
	limit int,
) ([]issueAIEvent, error) {
	query := `_stream:{event_type="error"} AND fingerprint:=` + strconv.Quote(fingerprint)
	body, err := g.isolatedQuery(
		ctx,
		project,
		"logs",
		"/select/logsql/query",
		url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}},
	)
	if err != nil {
		return nil, err
	}
	result := make([]issueAIEvent, 0, limit)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), maxErrorEventSize*2)
	for scanner.Scan() {
		var record map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		var symbolicated, rawText string
		_ = json.Unmarshal(record["symbolicated_event"], &symbolicated)
		_ = json.Unmarshal(record["raw_event"], &rawText)
		var event errortrack.Event
		if json.Unmarshal([]byte(symbolicated), &event) != nil {
			continue
		}
		var raw map[string]any
		_ = json.Unmarshal([]byte(rawText), &raw)
		contexts, _ := raw["contexts"].(map[string]any)
		result = append(result, issueAIEvent{Event: event, Raw: raw, Contexts: contexts})
	}
	return result, scanner.Err()
}

func (g *Gateway) issueAITrace(ctx context.Context, project meta.ProjectInfo, traceID string) string {
	body, err := g.isolatedQuery(ctx, project, "traces", "/select/jaeger/api/traces/"+url.PathEscape(traceID), nil)
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

func (g *Gateway) issueAILogs(ctx context.Context, project meta.ProjectInfo, traceID string, at time.Time) string {
	query := `(level:="error" OR level:="warn" OR severity_text:="ERROR" OR severity_text:="WARN") AND trace_id:=` + strconv.Quote(
		traceID,
	)
	values := url.Values{
		"query": {query},
		"limit": {"30"},
		"start": {at.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)},
		"end":   {at.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)},
	}
	body, err := g.isolatedQuery(ctx, project, "logs", "/select/logsql/query", values)
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

func (g *Gateway) issueAIMetrics(ctx context.Context, project meta.ProjectInfo, firstSeen time.Time) string {
	start, end := firstSeen.Add(-30*time.Minute), firstSeen.Add(30*time.Minute)
	queries := []struct{ name, expr string }{
		{"error_rate", `sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))`},
		{"request_rate", `sum(rate(http_server_request_duration_seconds_count[5m]))`},
	}
	result := make(map[string]any, len(queries))
	for _, query := range queries {
		body, err := g.isolatedQuery(
			ctx,
			project,
			"metrics",
			"/api/v1/query_range",
			url.Values{
				"query": {query.expr},
				"start": {start.UTC().Format(time.RFC3339)},
				"end":   {end.UTC().Format(time.RFC3339)},
				"step":  {"90"},
			},
		)
		if err != nil {
			result[query.name] = map[string]string{"unavailable": "query failed"}
			continue
		}
		result[query.name] = compactMetricResponse(body, 40)
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

func compactMetricResponse(body []byte, maxPoints int) any {
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

// newIssueAIOutcomeCounter counts automatic analyses by outcome so dropped or
// failing work is visible without reading logs. One low-cardinality series per
// outcome: completed, queue_full, rate_limited, failed.
func newIssueAIOutcomeCounter(provider metric.MeterProvider) metric.Int64Counter {
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	counter, err := provider.Meter("github.com/eimsound/lazy-observe/internal/gateway").Int64Counter(
		"lazy_observe.issue_ai.automatic_outcomes",
		metric.WithDescription("Automatic Issue AI analyses by outcome."),
	)
	if err != nil {
		counter, _ = noop.NewMeterProvider().Meter("").Int64Counter("lazy_observe.issue_ai.automatic_outcomes")
	}
	return counter
}

func (s *issueAIService) record(outcome string) {
	s.outcomes.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
