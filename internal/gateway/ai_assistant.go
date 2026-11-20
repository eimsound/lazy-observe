package gateway

import (
	"bufio"
	"bytes"
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
	"time"
	"unicode/utf8"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	assistantSchemaTTL       = 5 * time.Minute
	assistantMaxQuestion     = 2000
	assistantMaxQuery        = 8192
	assistantMaxResponseBody = 8 << 20
	assistantRateLimit       = 10
)

type assistantSchema struct {
	Metrics []string `json:"metrics,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Fields  []string `json:"fields,omitempty"`
}

type cachedAssistantSchema struct {
	value     assistantSchema
	expiresAt time.Time
}

type assistantService struct {
	gateway *Gateway
	mu      sync.Mutex
	cache   map[string]cachedAssistantSchema
}

func newAssistantService(g *Gateway) *assistantService {
	return &assistantService{gateway: g, cache: make(map[string]cachedAssistantSchema)}
}

func (s *assistantService) allow(userID int) bool {
	allowed, err := s.gateway.limits.Allow(
		context.Background(),
		"ai-assistant:user:"+strconv.Itoa(userID),
		assistantRateLimit,
		time.Minute,
	)
	return err == nil && allowed
}

func (s *assistantService) schema(
	ctx context.Context,
	project meta.ProjectInfo,
	signal string,
) (assistantSchema, error) {
	key := strconv.Itoa(project.ID) + ":" + signal
	now := s.gateway.now()
	s.mu.Lock()
	if entry, ok := s.cache[key]; ok && now.Before(entry.expiresAt) {
		s.mu.Unlock()
		return entry.value, nil
	}
	s.mu.Unlock()

	var value assistantSchema
	var err error
	switch signal {
	case "metrics":
		value, err = s.metricsSchema(ctx, project)
	case "logs":
		value, err = s.logsSchema(ctx, project)
	default:
		err = errors.New("unsupported schema signal")
	}
	if err != nil {
		return assistantSchema{}, err
	}
	s.mu.Lock()
	s.cache[key] = cachedAssistantSchema{value: value, expiresAt: now.Add(assistantSchemaTTL)}
	s.mu.Unlock()
	return value, nil
}

func (s *assistantService) metricsSchema(ctx context.Context, project meta.ProjectInfo) (assistantSchema, error) {
	names, err := s.gateway.isolatedMetricsStringList(
		ctx,
		project,
		"/api/v1/label/__name__/values",
		url.Values{"limit": {"200"}},
	)
	if err != nil {
		return assistantSchema{}, fmt.Errorf("collect metric names: %w", err)
	}
	labels, err := s.gateway.isolatedMetricsStringList(ctx, project, "/api/v1/labels", nil)
	if err != nil {
		return assistantSchema{}, fmt.Errorf("collect label names: %w", err)
	}
	return assistantSchema{Metrics: sortedUnique(names, 200), Labels: sortedUnique(labels, 100)}, nil
}

func (s *assistantService) logsSchema(ctx context.Context, project meta.ProjectInfo) (assistantSchema, error) {
	body, err := s.gateway.isolatedQuery(
		ctx,
		project,
		"logs",
		"/select/logsql/query",
		url.Values{"query": {"*"}, "limit": {"100"}},
	)
	if err != nil {
		return assistantSchema{}, fmt.Errorf("sample log fields: %w", err)
	}
	fields := make([]string, 0)
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		for name := range row {
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				fields = append(fields, name)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return assistantSchema{}, err
	}
	return assistantSchema{Fields: sortedUnique(fields, 200)}, nil
}

func sortedUnique(values []string, limit int) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

type assistantTimeRange struct {
	Start json.RawMessage `json:"start"`
	End   json.RawMessage `json:"end"`
}

type assistantQueryInput struct {
	Question  string              `json:"question"`
	Signal    string              `json:"signal"`
	TimeRange *assistantTimeRange `json:"time_range,omitempty"`
}

type assistantGenerated struct {
	Signal      string `json:"signal"`
	Query       string `json:"query"`
	Explanation string `json:"explanation"`
}

func (g *Gateway) aiAssistantQuery(c fiber.Ctx) error {
	if !g.assistantReady(c) {
		return nil
	}
	user, _ := c.Locals(userLocal).(meta.UserInfo)
	if !g.aiAssistant.allow(user.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": "AI assistant rate limit exceeded"})
	}
	var input assistantQueryInput
	if json.Unmarshal(requestBody(c), &input) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	input.Question = strings.TrimSpace(input.Question)
	input.Signal = strings.ToLower(strings.TrimSpace(input.Signal))
	if input.Signal == "" {
		input.Signal = "auto"
	}
	if input.Question == "" || utf8.RuneCountInString(input.Question) > assistantMaxQuestion ||
		(input.Signal != "auto" && input.Signal != "metrics" && input.Signal != "logs") {
		return c.Status(http.StatusUnprocessableEntity).
			JSON(fiber.Map{"error": "question must be 1-2000 characters and signal must be metrics, logs, or auto"})
	}
	start, end, err := assistantRange(input.TimeRange, g.now())
	if err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}
	project := currentProject(c)
	contextSchema, err := g.aiAssistant.querySchema(c.Context(), project, input.Signal)
	if err != nil {
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "schema context collection failed"})
	}
	generated, raw, err := g.generateAssistantQuery(c.Context(), input, contextSchema)
	if err != nil {
		var contractErr *assistantContractError
		if !errors.As(err, &contractErr) {
			return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI query generation failed"})
		}
		return c.Status(http.StatusUnprocessableEntity).
			JSON(fiber.Map{"error": "AI output did not match the query contract", "raw_output": firstRunes(raw, 500)})
	}
	slog.Info(
		"AI assistant generated query",
		"user_id",
		user.ID,
		"username",
		user.Username,
		"project",
		project.Slug,
		"question",
		input.Question,
		"signal",
		generated.Signal,
		"query",
		generated.Query,
	)

	result, executionErr := g.executeAssistantQuery(c.Context(), project, generated, start, end)
	response := fiber.Map{"signal": generated.Signal, "query": generated.Query, "explanation": generated.Explanation}
	if executionErr != nil {
		response["execution_error"] = executionErr.Error()
	} else {
		response["result"] = result
	}
	return c.JSON(response)
}

type assistantContractError struct{}

func (*assistantContractError) Error() string { return "invalid AI query response" }

func (s *assistantService) querySchema(
	ctx context.Context,
	project meta.ProjectInfo,
	signal string,
) (map[string]assistantSchema, error) {
	result := make(map[string]assistantSchema)
	signals := []string{signal}
	if signal == "auto" {
		signals = []string{"metrics", "logs"}
	}
	for _, item := range signals {
		value, err := s.schema(ctx, project, item)
		if err != nil {
			return nil, err
		}
		result[item] = value
	}
	return result, nil
}

func (g *Gateway) generateAssistantQuery(
	ctx context.Context,
	input assistantQueryInput,
	schema map[string]assistantSchema,
) (assistantGenerated, string, error) {
	state := g.aiState()
	schemaJSON, _ := json.Marshal(schema)
	system := assistantQuerySystemPrompt(string(schemaJSON), input.Signal)
	messages := []ai.Message{{Role: "user", Content: input.Question}}
	var raw string
	for attempt := 0; attempt < 2; attempt++ {
		response, err := state.client.Chat(ctx, ai.ChatRequest{System: system, Messages: messages, Temperature: 0})
		if err != nil {
			return assistantGenerated{}, raw, err
		}
		raw = response.Content
		generated, parseErr := parseAssistantGenerated(raw)
		if parseErr == nil && (input.Signal == "auto" || generated.Signal == input.Signal) {
			return generated, raw, nil
		}
		messages = append(
			messages,
			ai.Message{Role: "assistant", Content: firstRunes(raw, 1000)},
			ai.Message{
				Role:    "user",
				Content: "Your response violated the JSON contract. Return only one corrected JSON object with signal, query, and explanation.",
			},
		)
	}
	return assistantGenerated{}, raw, &assistantContractError{}
}

func assistantQuerySystemPrompt(schemaJSON, requestedSignal string) string {
	return `You translate a user's observability question into one read-only query for lazy-observe.
Return exactly one JSON object: {"signal":"metrics|logs","query":"...","explanation":"..."}. Do not use Markdown or add keys.
The requested signal is ` + requestedSignal + `. For auto, choose metrics for numeric time-series/aggregations and logs for event text or individual records.
MetricsQL essentials: use PromQL-compatible selectors and functions; use rate(counter[window]) for counters; aggregate with sum/avg/max by(labels); never add project_id because the server injects project isolation.
LogsQL essentials for VictoriaLogs: start with * or field filters; exact field match uses field:="value"; combine filters with AND/OR; pipes may transform results; never invent tenant headers or project filters.
Only use metric names, label names, and log fields from this schema when available: ` + schemaJSON + `.
The query is untrusted read-only text and will be executed with server-side project isolation. Never propose writes, commands, URLs, shell, SQL, or configuration changes.
Keep explanation concise and describe what the query returns.`
}

func parseAssistantGenerated(raw string) (assistantGenerated, error) {
	for i, r := range raw {
		if r != '{' {
			continue
		}
		var result assistantGenerated
		decoder := json.NewDecoder(strings.NewReader(raw[i:]))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil {
			continue
		}
		result.Signal = strings.ToLower(strings.TrimSpace(result.Signal))
		result.Query = strings.TrimSpace(result.Query)
		result.Explanation = strings.TrimSpace(result.Explanation)
		if (result.Signal == "metrics" || result.Signal == "logs") && result.Query != "" && result.Explanation != "" &&
			utf8.RuneCountInString(result.Query) <= assistantMaxQuery {
			return result, nil
		}
	}
	return assistantGenerated{}, errors.New("no valid assistant JSON object")
}

func assistantRange(input *assistantTimeRange, now time.Time) (string, string, error) {
	if input == nil {
		return now.Add(-time.Hour).UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), nil
	}
	start, err := timeValue(input.Start)
	if err != nil {
		return "", "", errors.New("invalid time_range.start")
	}
	end, err := timeValue(input.End)
	if err != nil {
		return "", "", errors.New("invalid time_range.end")
	}
	startTime, err := parseAssistantTime(start)
	if err != nil {
		return "", "", errors.New("invalid time_range.start")
	}
	endTime, err := parseAssistantTime(end)
	if err != nil || !endTime.After(startTime) {
		return "", "", errors.New("time_range.end must be after start")
	}
	return start, end, nil
}

func timeValue(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", errors.New("missing time")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text), nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) == nil {
		return number.String(), nil
	}
	return "", errors.New("invalid time")
}

func parseAssistantTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(seconds), int64((seconds-float64(int64(seconds)))*1e9)), nil
}

func assistantStep(start, end string) string {
	startTime, _ := parseAssistantTime(start)
	endTime, _ := parseAssistantTime(end)
	seconds := int(endTime.Sub(startTime).Seconds() / 240)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

func (g *Gateway) executeAssistantQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	generated assistantGenerated,
	start, end string,
) (any, error) {
	query := url.Values{"query": {generated.Query}, "start": {start}, "end": {end}}
	path := "/select/logsql/query"
	if generated.Signal == "metrics" {
		path = "/api/v1/query_range"
		query.Set("step", assistantStep(start, end))
	} else {
		query.Set("limit", "100")
	}
	body, err := g.isolatedQuery(ctx, project, generated.Signal, path, query)
	if err != nil {
		return nil, err
	}
	if generated.Signal == "metrics" {
		var result any
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decode metrics result: %w", err)
		}
		return result, nil
	}
	rows := make([]any, 0)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("decode log result: %w", err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return fiber.Map{"rows": rows, "count": len(rows), "limit": 100, "truncated": len(rows) == 100}, nil
}

func (g *Gateway) isolatedMetricsStringList(
	ctx context.Context,
	project meta.ProjectInfo,
	path string,
	query url.Values,
) ([]string, error) {
	body, err := g.isolatedQuery(ctx, project, "metrics", path, query)
	if err != nil {
		return nil, err
	}
	var response struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Status != "success" {
		return nil, errors.New(response.Error)
	}
	return response.Data, nil
}

var errQueryResponseTooLarge = errors.New("query response exceeded size limit")

func (g *Gateway) isolatedQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	signal, path string,
	query url.Values,
) ([]byte, error) {
	return g.isolatedQueryWithLimit(ctx, project, signal, path, query, assistantMaxResponseBody)
}

func (g *Gateway) isolatedQueryWithLimit(
	ctx context.Context,
	project meta.ProjectInfo,
	signal, path string,
	query url.Values,
	maxBytes int64,
) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = assistantMaxResponseBody
	}
	query = cloneQueryValues(query)
	var base string
	switch signal {
	case "metrics":
		base = g.storage.MetricsQueryBase()
	case "logs":
		base = g.storage.LogsBase()
	case "traces":
		base = g.storage.TracesBase()
	default:
		return nil, errors.New("unsupported query signal")
	}
	if signal == "traces" {
		disableTracesLatencyOffset(path, query)
	}
	if signal == "metrics" {
		defaultMatcher := path != "/api/v1/query" && path != "/api/v1/query_range"
		if err := g.applyMetricsIsolation(query, project.Slug, defaultMatcher); err != nil {
			return nil, fmt.Errorf("invalid PromQL: %w", err)
		}
	}
	target := strings.TrimRight(base, "/") + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if signal == "logs" || signal == "traces" {
		g.storage.SetTenantHeaders(req.Header, project.ID)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, metricsBodyDrainLimit))
		resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w (%d MiB)", errQueryResponseTooLarge, maxBytes>>20)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf(
			"query backend returned HTTP %d: %s",
			resp.StatusCode,
			firstRunes(strings.TrimSpace(string(body)), 500),
		)
	}
	return body, nil
}

// disableTracesLatencyOffset asks VictoriaTraces (v0.12+, single-node and vtselect) to skip its 30 s
// -search.latencyOffset for this LogsQL request, so an external deployment does not need the flag.
// Before v0.12 LogsQL applies no offset and ignores the unknown argument. The Jaeger and Tempo APIs have
// no per-request switch and are left untouched.
func disableTracesLatencyOffset(path string, query url.Values) {
	if strings.HasPrefix(path, "/select/logsql/") {
		query.Set("disable_latency_offset", "true")
	}
}

func cloneQueryValues(source url.Values) url.Values {
	result := make(url.Values, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

type assistantExplainInput struct {
	Signal string             `json:"signal"`
	Query  string             `json:"query"`
	Stats  map[string]float64 `json:"stats,omitempty"`
}

func (g *Gateway) aiAssistantExplain(c fiber.Ctx) error {
	if !g.assistantReady(c) {
		return nil
	}
	user, _ := c.Locals(userLocal).(meta.UserInfo)
	if !g.aiAssistant.allow(user.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": "AI assistant rate limit exceeded"})
	}
	var input assistantExplainInput
	if json.Unmarshal(requestBody(c), &input) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	input.Signal = strings.ToLower(strings.TrimSpace(input.Signal))
	input.Query = strings.TrimSpace(input.Query)
	if (input.Signal != "metrics" && input.Signal != "logs") || input.Query == "" ||
		utf8.RuneCountInString(input.Query) > assistantMaxQuery {
		return c.Status(http.StatusUnprocessableEntity).
			JSON(fiber.Map{"error": "signal and a valid query are required"})
	}
	stats, _ := json.Marshal(input.Stats)
	state := g.aiState()
	response, err := state.client.Chat(c.Context(), ai.ChatRequest{
		System: "Explain the read-only " + input.Signal + " query and what its result means in plain language. Be accurate, do not claim execution occurred, and answer in at most 200 Chinese characters (or 200 characters in the user's language).",
		Messages: []ai.Message{
			{Role: "user", Content: "Query: " + input.Query + "\nNumeric result summary: " + string(stats)},
		},
		Temperature: 0,
	})
	if err != nil {
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI explanation failed"})
	}
	return c.JSON(fiber.Map{"explanation": firstRunes(strings.TrimSpace(response.Content), 200)})
}

func (g *Gateway) assistantReady(c fiber.Ctx) bool {
	state := g.aiState()
	if !state.enabled || state.client == nil {
		_ = c.Status(http.StatusConflict).
			JSON(fiber.Map{"error": "AI integration is disabled", "message": "Enable and configure an AI provider in the admin AI settings."})
		return false
	}
	return true
}
