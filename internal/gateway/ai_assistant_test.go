package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

type assistantFakeAI struct {
	mu        sync.Mutex
	responses []string
	requests  []ai.ChatRequest
}

func (f *assistantFakeAI) Chat(_ context.Context, request ai.ChatRequest) (ai.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if len(f.responses) == 0 {
		return ai.ChatResponse{}, fmt.Errorf("no fake response")
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return ai.ChatResponse{Content: response}, nil
}

func (*assistantFakeAI) ChatStream(context.Context, ai.ChatRequest) (ai.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

func TestAssistantSchemaContextAndCache(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		if got := r.URL.Query().Get("extra_filters[]"); got != `{project_id="alpha"}` {
			t.Errorf("missing metrics isolation: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "__name__") {
			names := make([]string, 0, 205)
			for i := 204; i >= 0; i-- {
				names = append(names, fmt.Sprintf("metric_%03d", i))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": names})
			return
		}
		_ = json.NewEncoder(w).
			Encode(map[string]any{"status": "success", "data": []string{"service", "job", "service"}})
	}))
	defer backend.Close()

	now := time.Unix(1000, 0)
	g := newTestGateway(Config{Metrics: backend.URL, Logs: backend.URL, Now: func() time.Time { return now }})
	project := meta.ProjectInfo{ID: 7, Slug: "alpha"}
	first, err := g.aiAssistant.schema(context.Background(), project, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Metrics) != 200 || len(first.Labels) != 2 || first.Labels[0] != "job" {
		t.Fatalf("schema=%#v", first)
	}
	if _, err := g.aiAssistant.schema(context.Background(), project, "metrics"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if calls["/api/v1/label/__name__/values"] != 1 || calls["/api/v1/labels"] != 1 {
		t.Fatalf("cache miss calls=%v", calls)
	}
	mu.Unlock()
	now = now.Add(assistantSchemaTTL + time.Second)
	if _, err := g.aiAssistant.schema(context.Background(), project, "metrics"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/api/v1/label/__name__/values"] != 2 || calls["/api/v1/labels"] != 2 {
		t.Fatalf("expired cache not refreshed calls=%v", calls)
	}
}

func TestAssistantLogsSchemaContextAndCache(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != "9" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("logs request headers=%v query=%v", r.Header, r.URL.Query())
		}
		_, _ = io.WriteString(
			w,
			`{"_time":"now","service_name":"api"}`+"\n"+`{"level":"error","service_name":"api"}`+"\n",
		)
	}))
	defer backend.Close()
	g := newTestGateway(Config{Metrics: backend.URL, Logs: backend.URL})
	project := meta.ProjectInfo{ID: 9, Slug: "logs"}
	value, err := g.aiAssistant.schema(context.Background(), project, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(value.Fields, ",") != "_time,level,service_name" {
		t.Fatalf("fields=%v", value.Fields)
	}
	_, _ = g.aiAssistant.schema(context.Background(), project, "logs")
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestParseAssistantGeneratedTolerance(t *testing.T) {
	valid := `{"signal":"metrics","query":"rate(http_requests_total[5m])","explanation":"request rate"}`
	for _, input := range []string{valid, "```json\n" + valid + "\n```", "Here is the result: " + valid + " trailing text"} {
		got, err := parseAssistantGenerated(input)
		if err != nil || got.Signal != "metrics" || got.Query == "" {
			t.Fatalf("input=%q got=%#v err=%v", input, got, err)
		}
	}
	for _, input := range []string{`{"signal":"traces","query":"x","explanation":"x"}`, `{"signal":"logs","query":"","explanation":"x"}`, "not json", `{"signal":"logs","query":"x"}`, `{"signal":"logs","query":"*","explanation":"x","command":"rm"}`} {
		if _, err := parseAssistantGenerated(input); err == nil {
			t.Fatalf("expected rejection for %q", input)
		}
	}
}

func TestAssistantExecutionFailureStillReturnsGeneratedQuery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/label/__name__/values":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": []string{"seed_metric"}})
		case "/api/v1/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": []string{"job"}})
		default:
			http.Error(w, "bad generated query", http.StatusUnprocessableEntity)
		}
	}))
	defer backend.Close()
	fake := &assistantFakeAI{
		responses: []string{`{"signal":"metrics","query":"seed_metric","explanation":"seed values"}`},
	}
	store := testStore(t)
	mustCreateDefaultProject(t, store)
	cookie := sessionCookie(t, store, "assistant-execution-error", "password", "viewer")
	app := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, Metrics: backend.URL, Logs: backend.URL, AIEnabled: true, AIClient: fake},
	).App()
	resp, body := assistantRequest(
		t,
		app,
		cookie,
		"/api/ai/assistant/query",
		`{"question":"show seed","signal":"metrics"}`,
	)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"query":"seed_metric"`)) ||
		!bytes.Contains(body, []byte(`"execution_error"`)) ||
		bytes.Contains(body, []byte(`"result"`)) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAssistantGenerationRetry(t *testing.T) {
	fake := &assistantFakeAI{
		responses: []string{"not json", `{"signal":"metrics","query":"up","explanation":"availability"}`},
	}
	g := newTestGateway(Config{AIEnabled: true, AIClient: fake})
	got, _, err := g.generateAssistantQuery(
		context.Background(),
		assistantQueryInput{Question: "is it up", Signal: "metrics"},
		map[string]assistantSchema{"metrics": {Metrics: []string{"up"}}},
	)
	if err != nil || got.Query != "up" || len(fake.requests) != 2 {
		t.Fatalf("got=%#v requests=%d err=%v", got, len(fake.requests), err)
	}
	if len(fake.requests[1].Messages) != 3 || !strings.Contains(fake.requests[1].Messages[2].Content, "violated") {
		t.Fatalf("retry request=%#v", fake.requests[1])
	}
}

func TestAssistantInvalidOutputReturns422(t *testing.T) {
	backend := assistantMetricsBackend(t, func(*http.Request) {})
	defer backend.Close()
	fake := &assistantFakeAI{responses: []string{"first invalid", "second invalid"}}
	store := testStore(t)
	mustCreateDefaultProject(t, store)
	cookie := sessionCookie(t, store, "assistant-422", "password", "viewer")
	app := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, Metrics: backend.URL, Logs: backend.URL, AIEnabled: true, AIClient: fake},
	).App()
	resp, body := assistantRequest(
		t,
		app,
		cookie,
		"/api/ai/assistant/query",
		`{"question":"show availability","signal":"metrics"}`,
	)
	if resp.StatusCode != http.StatusUnprocessableEntity ||
		!bytes.Contains(body, []byte(`"raw_output":"second invalid"`)) ||
		len(fake.requests) != 2 {
		t.Fatalf("status=%d body=%s calls=%d", resp.StatusCode, body, len(fake.requests))
	}
}

func TestAssistantMetricsE2EAndMaliciousSelectorIsolation(t *testing.T) {
	var executed bool
	backend := assistantMetricsBackend(t, func(r *http.Request) {
		executed = true
		q := r.URL.Query()
		if q.Get("query") != `seed_metric{project_id="other"}` ||
			q.Get("extra_filters[]") != `{project_id="default"}` ||
			q.Get("step") == "" {
			t.Errorf("execution query=%v", q)
		}
	})
	defer backend.Close()
	provider := fakeOpenAI(
		t,
		`{"signal":"metrics","query":"seed_metric{project_id=\"other\"}","explanation":"seed values"}`,
	)
	defer provider.Close()
	client, err := ai.New(
		ai.Config{
			Provider:  "openai",
			BaseURL:   provider.URL,
			APIKey:    "test-key",
			Model:     "fake",
			MaxTokens: 500,
			Timeout:   time.Second,
			Client:    provider.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	mustCreateDefaultProject(t, store)
	cookie := sessionCookie(t, store, "assistant-metrics", "password", "viewer")
	app := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, Metrics: backend.URL, Logs: backend.URL, AIEnabled: true, AIClient: client},
	).App()
	resp, body := assistantRequest(
		t,
		app,
		cookie,
		"/api/ai/assistant/query",
		`{"question":"show the seeded metric","signal":"metrics"}`,
	)
	if resp.StatusCode != http.StatusOK || !executed {
		t.Fatalf("status=%d executed=%v body=%s", resp.StatusCode, executed, body)
	}
	var result map[string]any
	if json.Unmarshal(body, &result) != nil || result["execution_error"] != nil || result["result"] == nil ||
		result["signal"] != "metrics" {
		t.Fatalf("response=%s", body)
	}
	resultJSON, _ := json.Marshal(result["result"])
	if !bytes.Contains(resultJSON, []byte(`"value":"default-only"`)) ||
		bytes.Contains(resultJSON, []byte("other-only")) {
		t.Fatalf("isolation result=%s", resultJSON)
	}
}

func TestAssistantLogsE2EIsolation(t *testing.T) {
	store := testStore(t)
	mustCreateDefaultProject(t, store)
	project, err := store.ProjectBySlug(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	var executionCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != fmt.Sprint(project.ID) {
			t.Errorf("tenant headers=%v", r.Header)
		}
		if r.URL.Query().Get("query") == "*" {
			_, _ = io.WriteString(w, `{"service_name":"default","message":"schema"}`+"\n")
			return
		}
		executionCalls++
		if r.URL.Query().Get("limit") != "100" {
			t.Errorf("limit=%q", r.URL.Query().Get("limit"))
		}
		_, _ = io.WriteString(w, `{"service_name":"default","message":"only this tenant"}`+"\n")
	}))
	defer backend.Close()
	provider := fakeOpenAI(t, `{"signal":"logs","query":"service_name:=\"default\"","explanation":"matching logs"}`)
	defer provider.Close()
	client, err := ai.New(
		ai.Config{
			Provider:  "openai",
			BaseURL:   provider.URL,
			Model:     "fake",
			MaxTokens: 500,
			Timeout:   time.Second,
			Client:    provider.Client(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "assistant-logs", "password", "viewer")
	app := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, Metrics: backend.URL, Logs: backend.URL, AIEnabled: true, AIClient: client},
	).App()
	resp, body := assistantRequest(
		t,
		app,
		cookie,
		"/api/ai/assistant/query",
		`{"question":"find default service logs","signal":"logs"}`,
	)
	if resp.StatusCode != http.StatusOK || executionCalls != 1 || !bytes.Contains(body, []byte("only this tenant")) {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, executionCalls, body)
	}
}

func TestAssistantDisabledRateLimitAndExplain(t *testing.T) {
	store := testStore(t)
	mustCreateDefaultProject(t, store)
	cookie := sessionCookie(t, store, "assistant-explain", "password", "viewer")
	disabled := newMetadataTestGateway(store, Config{Tokens: store, Store: store}).App()
	resp, body := assistantRequest(t, disabled, cookie, "/api/ai/assistant/query", `{"question":"hello"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("disabled status=%d body=%s", resp.StatusCode, body)
	}

	responses := make([]string, assistantRateLimit)
	for i := range responses {
		responses[i] = "This query reports request rate."
	}
	fake := &assistantFakeAI{responses: responses}
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, AIEnabled: true, AIClient: fake}).App()
	for i := 0; i < assistantRateLimit; i++ {
		resp, body = assistantRequest(
			t,
			app,
			cookie,
			"/api/ai/assistant/explain",
			`{"signal":"metrics","query":"rate(requests_total[5m])","stats":{"max":12}}`,
		)
		if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("request rate")) {
			t.Fatalf("explain %d status=%d body=%s", i, resp.StatusCode, body)
		}
	}
	resp, body = assistantRequest(t, app, cookie, "/api/ai/assistant/explain", `{"signal":"metrics","query":"up"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate status=%d body=%s", resp.StatusCode, body)
	}
}

func assistantMetricsBackend(t *testing.T, onExecute func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/label/__name__/values":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": []string{"seed_metric"}})
		case "/api/v1/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": []string{"job", "project_id"}})
		case "/api/v1/query_range":
			onExecute(r)
			_ = json.NewEncoder(w).
				Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"project_id": "default"}, "value": "default-only"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func fakeOpenAI(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("provider path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).
			Encode(map[string]any{"model": "fake", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
	}))
}

func assistantRequest(t *testing.T, app httpTestApp, cookie *http.Cookie, path, body string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LO-Project", "default")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

type httpTestApp interface {
	Test(*http.Request, ...fiber.TestConfig) (*http.Response, error)
}

func mustCreateDefaultProject(t *testing.T, store *meta.Store) {
	t.Helper()
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
}
