package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/secret"
)

func TestParseIssueAIAnalysisToleranceAndValidation(t *testing.T) {
	valid := `{"summary":"checkout fails","probable_causes":[{"cause":"database timeout","confidence":"high","evidence":"trace error span"}],"suggested_actions":["inspect database latency"]}`
	for _, input := range []string{valid, "```json\n" + valid + "\n```", "result: " + valid + " trailing"} {
		got, err := parseIssueAIAnalysis(input)
		if err != nil || got.Summary != "checkout fails" || got.ProbableCauses[0].Confidence != "high" {
			t.Fatalf("input=%q got=%#v err=%v", input, got, err)
		}
	}
	tooLong := strings.Repeat("界", 151)
	invalid := []string{
		`{"summary":"","probable_causes":[],"suggested_actions":[]}`,
		`{"summary":"x","probable_causes":[{"cause":"x","confidence":"certain","evidence":"y"}],"suggested_actions":["z"]}`,
		`{"summary":"x","probable_causes":[{"cause":"x","confidence":"low","evidence":"y"}],"suggested_actions":["z"],"extra":true}`,
		`{"summary":"` + tooLong + `","probable_causes":[{"cause":"x","confidence":"low","evidence":"y"}],"suggested_actions":["z"]}`,
		`{"summary":"x","probable_causes":[{"cause":"` + strings.Repeat(
			"c",
			301,
		) + `","confidence":"low","evidence":"y"}],"suggested_actions":["z"]}`,
		`{"summary":"x","probable_causes":[{"cause":"x","confidence":"low","evidence":"y"}],"suggested_actions":["` + strings.Repeat(
			"a",
			501,
		) + `"]}`,
	}
	for _, input := range invalid {
		if _, err := parseIssueAIAnalysis(input); err == nil {
			t.Fatalf("expected invalid contract: %s", input)
		}
	}
}

func TestIssueAIContextTruncationAndDownsampling(t *testing.T) {
	sections := []contextSection{
		{"STACKS", strings.Repeat("stack-frame\n", 3000), 11500},
		{"LOGS", strings.Repeat("log-line\n", 3000), 2200},
		{"METRICS", strings.Repeat("metric\n", 3000), 2200},
	}
	got := fitContextSections(sections, issueAIContextBudget)
	if len(got) > issueAIContextBudget || !strings.Contains(got, "STACKS") || !strings.Contains(got, "[truncated]") {
		t.Fatalf("context bytes=%d prefix=%q", len(got), got[:min(len(got), 100)])
	}
	events := make([]issueAIEvent, 4)
	for i := range events {
		events[i].Event.EventID = fmt.Sprintf("event-%d", i+1)
	}
	stackText := issueAIStacks(events)
	if strings.Contains(stackText, "event-4") || !strings.Contains(stackText, "event-3") {
		t.Fatalf("recent stack cap failed: %s", stackText)
	}
	values := make([][]any, 101)
	for i := range values {
		values[i] = []any{float64(i), strconv.Itoa(i)}
	}
	body, _ := json.Marshal(
		map[string]any{
			"status": "success",
			"data": map[string]any{
				"result": []any{map[string]any{"metric": map[string]string{"service": "api"}, "values": values}},
			},
		},
	)
	compact := compactMetricResponse(body, 40)
	encoded, _ := json.Marshal(compact)
	var series []struct {
		Values [][]any `json:"values"`
	}
	if json.Unmarshal(encoded, &series) != nil || len(series) != 1 || len(series[0].Values) != 40 ||
		series[0].Values[0][1] != "0" ||
		series[0].Values[39][1] != "100" {
		t.Fatalf("downsampled=%s", encoded)
	}
	lines := make([]string, 31)
	for i := range lines {
		lines[i] = fmt.Sprintf(`{"level":"warn","message":"line-%d"}`, i)
	}
	backend := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, strings.Join(lines, "\n")) },
		),
	)
	defer backend.Close()
	g := newTestGateway(Config{Logs: backend.URL})
	logs := g.issueAILogs(context.Background(), meta.ProjectInfo{ID: 1, Slug: "p"}, "trace", time.Now())
	if strings.Count(logs, "\n") != 29 || strings.Contains(logs, "line-30") {
		t.Fatalf("logs were not capped at 30: %s", logs)
	}
}

func TestIssueAIRateLimitPerProject(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newTestGateway(Config{Now: func() time.Time { return now }})
	for i := 0; i < issueAIRateLimit; i++ {
		if !g.issueAI.allow(context.Background(), 1) {
			t.Fatalf("request %d unexpectedly limited", i+1)
		}
	}
	if g.issueAI.allow(context.Background(), 1) {
		t.Fatal("seventh request should be limited")
	}
	if !g.issueAI.allow(context.Background(), 2) {
		t.Fatal("second project should have an independent limit")
	}
	now = now.Add(time.Minute)
	if !g.issueAI.allow(context.Background(), 1) {
		t.Fatal("limit should reset after one minute")
	}
}

type issueAIFake struct {
	mu       sync.Mutex
	requests []ai.ChatRequest
	output   string
}

type blockingIssueAIFake struct {
	started chan struct{}
	once    sync.Once
}

func (f *blockingIssueAIFake) Chat(ctx context.Context, _ ai.ChatRequest) (ai.ChatResponse, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return ai.ChatResponse{}, ctx.Err()
}

func (*blockingIssueAIFake) ChatStream(context.Context, ai.ChatRequest) (ai.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

func disableIssueAIAutomation(t *testing.T, g *Gateway) {
	t.Helper()
	g.issueAI.enabled = false
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.issueAI.Close(ctx); err != nil {
		t.Fatalf("stop automatic Issue AI worker: %v", err)
	}
}

func (f *issueAIFake) Chat(_ context.Context, request ai.ChatRequest) (ai.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return ai.ChatResponse{Content: f.output}, nil
}

func (*issueAIFake) ChatStream(context.Context, ai.ChatRequest) (ai.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

type issueAIBackend struct {
	errors    errorStorage
	mu        sync.Mutex
	logsWrote bool
	spanWrote bool
}

func (b *issueAIBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/insert/opentelemetry/v1/logs":
		b.mu.Lock()
		b.logsWrote = true
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/insert/opentelemetry/v1/traces":
		b.mu.Lock()
		b.spanWrote = true
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/select/logsql/query" && strings.Contains(r.URL.Query().Get("query"), "trace_id:="):
		if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") == "" || r.URL.Query().Get("limit") != "30" {
			http.Error(w, "missing log isolation", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		wrote := b.logsWrote
		b.mu.Unlock()
		if wrote {
			_, _ = io.WriteString(
				w,
				`{"timestamp":"2026-07-17T10:00:00Z","level":"warn","trace_id":"trace-ai","message":"db timeout near checkout"}`+"\n",
			)
		}
	case r.URL.Path == "/select/jaeger/api/traces/trace-ai":
		if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") == "" {
			http.Error(w, "missing trace isolation", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		wrote := b.spanWrote
		b.mu.Unlock()
		spans := []any{}
		if wrote {
			spans = append(
				spans,
				map[string]any{
					"spanID":        "root",
					"operationName": "POST /checkout",
					"duration":      120000,
					"references":    []any{},
					"tags":          []any{},
				},
				map[string]any{
					"spanID":        "db",
					"operationName": "db.query orders",
					"duration":      100000,
					"references":    []any{map[string]any{"refType": "CHILD_OF", "spanID": "root"}},
					"tags":          []any{map[string]any{"key": "error", "value": true}},
				},
			)
		}
		_ = json.NewEncoder(w).
			Encode(map[string]any{"data": []any{map[string]any{"traceID": "trace-ai", "spans": spans}}})
	case r.URL.Path == "/api/v1/query_range":
		if got := r.URL.Query().
			Get("extra_filters[]"); got != `{project_id="default"}` ||
			r.URL.Query().Get("step") != "90" {
			http.Error(w, "missing metrics isolation", http.StatusBadRequest)
			return
		}
		values := make([][]any, 45)
		for i := range values {
			values[i] = []any{float64(i), fmt.Sprintf("%d", i)}
		}
		_ = json.NewEncoder(w).
			Encode(map[string]any{"status": "success", "data": map[string]any{"result": []any{map[string]any{"metric": map[string]string{}, "values": values}}}})
	default:
		b.errors.ServeHTTP(w, r)
	}
}

func TestAutomaticIssueAIAnalysisStoresNewIssueSummary(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(context.Background(), "Automatic AI", "automatic-ai")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(&issueAIBackend{})
	defer backend.Close()
	fake := &issueAIFake{
		output: `{"summary":"new issue summarized automatically","probable_causes":[{"cause":"request failure","confidence":"medium","evidence":"new issue metadata"}],"suggested_actions":["inspect the failing request"]}`,
	}
	gw := newMetadataTestGateway(store,
		Config{
			Tokens:    store,
			Store:     store,
			Logs:      backend.URL,
			Metrics:   backend.URL,
			Traces:    backend.URL,
			AIEnabled: true,
			AIClient:  fake,
		},
	)
	t.Cleanup(func() { _ = gw.Shutdown() })

	now := time.Now().UTC()
	event := map[string]any{
		"event_id":  "automatic-event",
		"timestamp": now.Format(time.RFC3339Nano),
		"platform":  "go",
		"level":     "error",
		"exception": map[string]any{
			"type":  "AutomaticError",
			"value": "boom",
			"stacktrace": map[string]any{
				"frames": []any{map[string]any{"function": "automatic", "filename": "automatic.go", "in_app": true}},
			},
		},
	}
	postErrorsTest(t, gw.App(), token, []any{event}, http.StatusOK)
	var rows []meta.IssueView
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err = store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
		if err != nil || len(rows) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(rows) != 1 {
		t.Fatalf("issues=%#v err=%v", rows, err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for {
		row, getErr := store.GetIssue(context.Background(), project.ID, rows[0].ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if row.AIAnalysisAt != nil && row.AIAnalysis["summary"] == "new issue summarized automatically" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic analysis was not stored: %#v", row)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fake.mu.Lock()
	calls := len(fake.requests)
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("automatic AI calls=%d", calls)
	}
}

func TestAutomaticIssueAIAnalysisStartsAfterDynamicEnable(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(context.Background(), "Dynamic AI", "dynamic-ai")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "dynamic-ai-admin", "password", "admin")
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(&issueAIBackend{})
	defer backend.Close()
	fake := &issueAIFake{
		output: `{"summary":"dynamically enabled analysis","probable_causes":[{"cause":"request failure","confidence":"medium","evidence":"new issue metadata"}],"suggested_actions":["inspect the failing request"]}`,
	}
	gw := newMetadataTestGateway(store,
		Config{
			Tokens:          store,
			Store:           store,
			Logs:            backend.URL,
			Metrics:         backend.URL,
			Traces:          backend.URL,
			AISettingsStore: store,
			AISecretBox:     box,
			AIBuildClient: func(ai.Config) (ai.Client, error) {
				return fake, nil
			},
		},
	)
	t.Cleanup(func() { _ = gw.Shutdown() })

	resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true, "provider": "openai", "model": "dynamic-model", "api_key": "dynamic-key",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable dynamic AI status=%d body=%s", resp.StatusCode, body)
	}

	event := map[string]any{
		"event_id":  "dynamic-ai-event",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"platform":  "go",
		"level":     "error",
		"exception": map[string]any{
			"type":  "DynamicAIError",
			"value": "boom",
			"stacktrace": map[string]any{
				"frames": []any{map[string]any{"function": "dynamic", "filename": "dynamic.go", "in_app": true}},
			},
		},
	}
	postErrorsTest(t, gw.App(), token, []any{event}, http.StatusOK)

	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, listErr := store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(rows) == 1 && rows[0].AIAnalysisAt != nil &&
			rows[0].AIAnalysis["summary"] == "dynamically enabled analysis" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic analysis did not start after dynamic enable: %#v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAutomaticIssueAIDoesNotBlockErrorIngest(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(context.Background(), "Nonblocking AI", "nonblocking-ai")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(&issueAIBackend{})
	defer backend.Close()
	fake := &blockingIssueAIFake{started: make(chan struct{})}
	gw := newMetadataTestGateway(store,
		Config{
			Tokens:    store,
			Store:     store,
			Logs:      backend.URL,
			Metrics:   backend.URL,
			Traces:    backend.URL,
			AIEnabled: true,
			AIClient:  fake,
		},
	)

	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{"events": []any{
		map[string]any{
			"event_id":  "nonblocking-event",
			"timestamp": now.Format(time.RFC3339Nano),
			"platform":  "go",
			"level":     "error",
			"exception": map[string]any{
				"type":  "NonblockingError",
				"value": "boom",
				"stacktrace": map[string]any{
					"frames": []any{map[string]any{"function": "main", "filename": "main.go", "in_app": true}},
				},
			},
		},
	}})
	type responseResult struct {
		status int
		err    error
	}
	result := make(chan responseResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/events/errors", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, requestErr := gw.App().Test(req)
		if requestErr != nil {
			result <- responseResult{err: requestErr}
			return
		}
		resp.Body.Close()
		result <- responseResult{status: resp.StatusCode}
	}()
	select {
	case got := <-result:
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("ingest status=%d err=%v", got.status, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error ingest waited for the configured LLM")
	}
	select {
	case <-fake.started:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic AI worker did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := gw.Drain(shutdownCtx); err != nil {
		t.Fatalf("gateway drain: %v", err)
	}
}

func TestIssueAIAnalysisE2ECacheStaleRefreshIsolationAndGuards(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	p1, _ := store.CreateProject(ctx, "Default", "default")
	p2, _ := store.CreateProject(ctx, "P2", "p2")
	token1, _ := store.CreateProjectAPIToken(ctx, p1.ID)
	token2, _ := store.CreateProjectAPIToken(ctx, p2.ID)
	admin := sessionCookie(t, store, "issue-ai-admin", "password", "admin")
	viewer := sessionCookie(t, store, "issue-ai-viewer", "password", "viewer")
	backendState := &issueAIBackend{}
	backend := httptest.NewServer(backendState)
	defer backend.Close()
	now := time.Now().UTC().Truncate(time.Second)
	fake := &issueAIFake{
		output: `{"summary":"checkout database span timed out","probable_causes":[{"cause":"database latency","confidence":"high","evidence":"db.query error span and nearby db timeout log"}],"suggested_actions":["inspect database latency around the trace"]}`,
	}
	gw := newMetadataTestGateway(store,
		Config{
			Tokens:    store,
			Store:     store,
			Logs:      backend.URL,
			Metrics:   backend.URL,
			Traces:    backend.URL,
			AIEnabled: true,
			AIClient:  fake,
			Now:       func() time.Time { return now },
		},
	)
	disableIssueAIAutomation(t, gw)
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	postSignal := func(path, token, payload string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("signal ingest path=%s status=%d err=%v", path, resp.StatusCode, err)
		}
		resp.Body.Close()
	}
	postSignal("/v1/logs", token1, `{"trace_id":"trace-ai","level":"warn","message":"db timeout near checkout"}`)
	postSignal("/v1/traces", token1, `{"trace_id":"trace-ai","spans":[{"name":"POST /checkout"}]}`)

	event := func(id string, timestamp time.Time, typ string) map[string]any {
		return map[string]any{
			"event_id":  id,
			"timestamp": timestamp.Format(time.RFC3339Nano),
			"platform":  "go",
			"release":   "v42",
			"level":     "error",
			"message":   "checkout failed",
			"exception": map[string]any{
				"type":  typ,
				"value": "database timeout",
				"stacktrace": map[string]any{
					"frames": []any{
						map[string]any{
							"function":     "checkout.Service.PlaceOrder",
							"filename":     "/srv/checkout/service.go",
							"lineno":       88,
							"in_app":       true,
							"symbolicated": true,
						},
					},
				},
			},
			"tags": map[string]string{"browser": "Chrome 126", "os": "Linux"},
			"contexts": map[string]any{
				"trace":   map[string]any{"trace_id": "trace-ai", "span_id": "db"},
				"browser": map[string]any{"name": "Chrome", "version": "126"},
				"os":      map[string]any{"name": "Linux"},
			},
		}
	}
	postErrorsTest(t, app, token1, []any{event("ai-e1", now.Add(-time.Minute), "CheckoutError")}, http.StatusOK)
	issues := listIssuesTest(t, app, admin, "default")
	if len(issues) != 1 {
		t.Fatalf("issues=%#v", issues)
	}
	id := int(issues[0]["id"].(float64))

	request := func(method, target, project string, cookie *http.Cookie) (int, []byte) {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-LO-Project", project)
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, body
	}
	status, body := request(http.MethodGet, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis", "default", admin)
	if status != http.StatusNotFound {
		t.Fatalf("empty cache status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPost, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis", "default", admin)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"cached":false`)) ||
		!bytes.Contains(body, []byte(`"confidence":"high"`)) {
		t.Fatalf("generate status=%d body=%s", status, body)
	}
	fake.mu.Lock()
	if len(fake.requests) != 1 {
		t.Fatalf("AI calls=%d", len(fake.requests))
	}
	prompt := fake.requests[0].Messages[0].Content
	fake.mu.Unlock()
	for _, evidence := range []string{"checkout.Service.PlaceOrder", "db timeout near checkout", "POST /checkout", "error=true", "Chrome 126"} {
		if !strings.Contains(prompt, evidence) {
			t.Fatalf("prompt missing %q:\n%s", evidence, prompt)
		}
	}
	if len(prompt) > issueAIContextBudget {
		t.Fatalf("prompt context bytes=%d", len(prompt))
	}
	status, body = request(http.MethodPost, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis", "default", admin)
	fake.mu.Lock()
	cachedCalls := len(fake.requests)
	fake.mu.Unlock()
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"cached":true`)) || cachedCalls != 1 {
		t.Fatalf("POST cache status=%d calls=%d body=%s", status, cachedCalls, body)
	}

	status, body = request(http.MethodGet, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis", "default", admin)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"cached":true`)) ||
		bytes.Contains(body, []byte(`"stale":true`)) {
		t.Fatalf("cache status=%d body=%s", status, body)
	}
	now = now.Add(2 * time.Minute)
	postErrorsTest(t, app, token1, []any{event("ai-e2", now, "CheckoutError")}, http.StatusOK)
	waitForIssueCountTest(t, app, admin, "default", 2)
	status, body = request(http.MethodGet, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis", "default", admin)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"cached":true`)) ||
		!bytes.Contains(body, []byte(`"stale":true`)) {
		t.Fatalf("stale status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPost, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis?refresh=1", "default", admin)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"cached":false`)) ||
		bytes.Contains(body, []byte(`"stale":true`)) {
		t.Fatalf("refresh status=%d body=%s", status, body)
	}
	fake.mu.Lock()
	calls := len(fake.requests)
	fake.mu.Unlock()
	if calls != 2 {
		t.Fatalf("AI calls after refresh=%d", calls)
	}

	postErrorsTest(t, app, token2, []any{event("p2-ai", now, "P2Secret")}, http.StatusOK)
	p2Issues := listIssuesTest(t, app, admin, "p2")
	p2ID := int(p2Issues[0]["id"].(float64))
	status, _ = request(http.MethodPost, "/api/issues/"+strconv.Itoa(p2ID)+"/ai-analysis", "default", admin)
	if status != http.StatusNotFound {
		t.Fatalf("cross-project status=%d", status)
	}
	status, _ = request(http.MethodPost, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis?refresh=1", "default", viewer)
	if status != http.StatusForbidden {
		t.Fatalf("viewer status=%d", status)
	}
	disabled := newMetadataTestGateway(store,
		Config{
			Tokens:  store,
			Store:   store,
			Logs:    backend.URL,
			Metrics: backend.URL,
			Traces:  backend.URL,
		},
	)
	t.Cleanup(func() { _ = disabled.Shutdown() })
	req := httptest.NewRequest(http.MethodPost, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis?refresh=1", nil)
	req.Header.Set("X-LO-Project", "default")
	req.AddCookie(admin)
	resp, err := disabled.App().Test(req)
	if err != nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("AI disabled status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
}

func TestIssueAIInvalidOutputRetriesThenReturns422(t *testing.T) {
	store := testStore(t)
	p, _ := store.CreateProject(context.Background(), "Default", "default")
	token, _ := store.CreateProjectAPIToken(context.Background(), p.ID)
	cookie := sessionCookie(t, store, "issue-ai-422", "password", "member")
	backendState := &issueAIBackend{}
	backend := httptest.NewServer(backendState)
	defer backend.Close()
	fake := &assistantFakeAI{
		responses: []string{"not json", `{"summary":"still invalid","probable_causes":[],"suggested_actions":[]}`},
	}
	gw := newMetadataTestGateway(store,
		Config{
			Tokens:    store,
			Store:     store,
			Logs:      backend.URL,
			Metrics:   backend.URL,
			Traces:    backend.URL,
			AIEnabled: true,
			AIClient:  fake,
		},
	)
	disableIssueAIAutomation(t, gw)
	t.Cleanup(func() { _ = gw.Shutdown() })
	now := time.Now().UTC()
	event := map[string]any{
		"event_id":  "bad-ai",
		"timestamp": now.Format(time.RFC3339Nano),
		"platform":  "go",
		"level":     "error",
		"exception": map[string]any{
			"type":  "BadAI",
			"value": "x",
			"stacktrace": map[string]any{
				"frames": []any{map[string]any{"function": "main", "filename": "main.go", "in_app": true}},
			},
		},
	}
	postErrorsTest(t, gw.App(), token, []any{event}, http.StatusOK)
	id := int(listIssuesTest(t, gw.App(), cookie, "default")[0]["id"].(float64))
	req := httptest.NewRequest(http.MethodPost, "/api/issues/"+strconv.Itoa(id)+"/ai-analysis", nil)
	req.Header.Set("X-LO-Project", "default")
	req.AddCookie(cookie)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || len(fake.requests) != 2 ||
		!bytes.Contains(body, []byte(`"raw_output"`)) {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, len(fake.requests), body)
	}
}
