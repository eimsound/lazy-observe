package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolHarness serves canned platform API responses and calls MCP tools
// through the real streamable HTTP handler, so each test checks what an agent
// actually receives.
type toolHarness struct {
	t        *testing.T
	session  *sdkmcp.ClientSession
	mu       sync.Mutex
	requests []*http.Request
}

func newToolHarness(t *testing.T, routes map[string]func(*http.Request) (int, any)) *toolHarness {
	t.Helper()
	h := &toolHarness{t: t}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Clone(context.Background()))
		h.mu.Unlock()
		route, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		status, body := route(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(api.Close)
	mcpHTTP := httptest.NewServer(NewHTTPHandler(api.URL, api.Client()))
	t.Cleanup(mcpHTTP.Close)
	httpClient := &http.Client{Transport: addTokenTransport{base: http.DefaultTransport, token: "lou_reader"}}
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "contract", Version: "1"}, nil).Connect(
		context.Background(),
		&sdkmcp.StreamableClientTransport{Endpoint: mcpHTTP.URL, HTTPClient: httpClient, DisableStandaloneSSE: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	h.session = session
	return h
}

func (h *toolHarness) call(name string, arguments map[string]any) (map[string]any, string) {
	h.t.Helper()
	result, err := h.session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil || result.IsError || len(result.Content) != 1 {
		h.t.Fatalf("%s result=%#v err=%v", name, result, err)
	}
	text := result.Content[0].(*sdkmcp.TextContent).Text
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		h.t.Fatalf("%s returned non-object JSON: %v\n%s", name, err, text)
	}
	return decoded, text
}

func (h *toolHarness) requestsTo(path string) []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*http.Request
	for _, r := range h.requests {
		if r.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func issuePage(n, offset int) map[string]any {
	issues := make([]any, 0, n)
	for i := range n {
		issues = append(issues, map[string]any{
			"id": offset + i + 1, "title": "panic", "project_id": 7, "fingerprint": "secret-fp",
		})
	}
	return map[string]any{"issues": issues}
}

func TestListIssuesStripsInternalFieldsAndDetectsTruncation(t *testing.T) {
	h := newToolHarness(t, map[string]func(*http.Request) (int, any){
		"/api/v1/issues": func(r *http.Request) (int, any) {
			if r.URL.Query().Get("page") == "2" {
				return http.StatusOK, issuePage(1, 50)
			}
			return http.StatusOK, issuePage(50, 0)
		},
	})
	got, text := h.call("list_issues", map[string]any{"status": "unresolved", "q": "panic"})
	if strings.Contains(text, "project_id") || strings.Contains(text, "secret-fp") {
		t.Fatalf("project-internal fields leaked: %s", text)
	}
	if got["count"] != float64(50) || got["truncated"] != true || got["limit"] != float64(50) {
		t.Fatalf("count/truncated/limit=%v/%v/%v", got["count"], got["truncated"], got["limit"])
	}
	first := h.requestsTo("/api/v1/issues")[0].URL.Query()
	if first.Get("status") != "unresolved" || first.Get("q") != "panic" {
		t.Fatalf("filters not forwarded: %v", first)
	}
}

func TestListIssuesUnderLimitIsNotTruncatedAndSkipsSecondPage(t *testing.T) {
	h := newToolHarness(t, map[string]func(*http.Request) (int, any){
		"/api/v1/issues": func(*http.Request) (int, any) { return http.StatusOK, issuePage(3, 0) },
	})
	got, _ := h.call("list_issues", map[string]any{})
	if got["count"] != float64(3) || got["truncated"] != false {
		t.Fatalf("count/truncated=%v/%v", got["count"], got["truncated"])
	}
	if n := len(h.requestsTo("/api/v1/issues")); n != 1 {
		t.Fatalf("issue page requests=%d want 1", n)
	}
}

func TestGetIssueCapsEventsAndKeepsInnermostFrames(t *testing.T) {
	frames := make([]any, 0, 40)
	for i := range 40 {
		frames = append(frames, map[string]any{
			"function":     fmt.Sprintf("f%d", i),
			"filename":     "a.js",
			"lineno":       i,
			"in_app":       true,
			"symbolicated": true,
		})
	}
	eventSummaries := make([]any, 0, 7)
	routes := map[string]func(*http.Request) (int, any){}
	for i := range 7 {
		eid := fmt.Sprintf("e%d", i)
		eventSummaries = append(eventSummaries, map[string]any{
			"event_id":  eid,
			"timestamp": 1700000000000 + i,
			"trace_id":  "trace-1",
		})
		routes["/api/v1/issues/9/events/"+eid] = func(*http.Request) (int, any) {
			return http.StatusOK, map[string]any{
				"event_id":  eid,
				"timestamp": 1700000000000 + i,
				"level":     "error",
				"release":   "v1.2.3",
				"message":   "boom",
				"exception": map[string]any{
					"type":       "TypeError",
					"value":      "x is undefined",
					"stacktrace": map[string]any{"frames": frames},
				},
			}
		}
	}
	// Real gateway /api/v1/issues/:id DTO omits symbolicated_event and exposes fingerprint.
	routes["/api/v1/issues/9"] = func(*http.Request) (int, any) {
		return http.StatusOK, map[string]any{
			"id":          9,
			"project_id":  7,
			"fingerprint": "secret-issue-fp",
			"level":       "error",
			"events":      eventSummaries,
		}
	}
	h := newToolHarness(t, routes)
	got, text := h.call("get_issue", map[string]any{"issue_id": 9})
	if strings.Contains(text, "project_id") || strings.Contains(text, "secret-issue-fp") {
		t.Fatalf("internal fields leaked: %s", text)
	}
	summaries := got["events"].([]any)
	if len(summaries) != 5 || got["events_truncated"] != true {
		t.Fatalf("events=%d truncated=%v", len(summaries), got["events_truncated"])
	}
	first := summaries[0].(map[string]any)
	stack := first["stack_frames"].([]any)
	if len(stack) != 30 || first["stack_truncated"] != true || first["exception_type"] != "TypeError" ||
		first["level"] != "error" || first["message"] != "boom" {
		t.Fatalf("first event summary=%#v", first)
	}
	// Frames are ordered outermost→innermost; the crash site must survive the cap.
	if last := stack[29].(map[string]any); last["function"] != "f39" || last["symbolicated"] != true {
		t.Fatalf("innermost frame=%#v want f39 symbolicated=true", last)
	}
	// Only the capped 5 events should be fetched, not e5 or e6.
	if len(h.requestsTo("/api/v1/issues/9/events/e5")) != 0 {
		t.Fatal("fetched event beyond cap")
	}
}

func TestMCPEmptyAndNilResponseSafetyAndFallbackHandler(t *testing.T) {
	if got := sanitizeIssues(nil, false); got == nil {
		t.Fatal("sanitizeIssues(nil) returned nil")
	}
	if got := summarizeIssue(nil); got == nil {
		t.Fatal("summarizeIssue(nil) returned nil")
	}
	if got := summarizeTraceSearch(nil, 10); got["count"] != 0 {
		t.Fatalf("summarizeTraceSearch(nil) = %#v", got)
	}
	if got := flamegraphSummary(nil, 10); got["count"] != 0 {
		t.Fatalf("flamegraphSummary(nil) = %#v", got)
	}

	// NewHTTPHandler with nil client and missing auth header must not panic when calling tools.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer api.Close()
	mcpHTTP := httptest.NewServer(NewHTTPHandler(api.URL, nil))
	defer mcpHTTP.Close()
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "unauth", Version: "1"}, nil).Connect(
		context.Background(),
		&sdkmcp.StreamableClientTransport{Endpoint: mcpHTTP.URL, DisableStandaloneSSE: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, callErr := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "list_issues",
		Arguments: map[string]any{},
	})
	if callErr == nil && (res == nil || !res.IsError) {
		t.Fatalf("expected error from unauthorized upstream without panic, got res=%#v err=%v", res, callErr)
	}
}

func TestSearchTracesSummarizesAndCapsResults(t *testing.T) {
	trace := func(id string) map[string]any {
		return map[string]any{
			"traceID": id,
			"processes": map[string]any{
				"p1": map[string]any{"serviceName": "checkout"},
				"p2": map[string]any{"serviceName": "db"},
			},
			"spans": []any{
				map[string]any{
					"spanID":        "root",
					"operationName": "GET /pay",
					"processID":     "p1",
					"startTime":     100,
					"duration":      50,
				},
				map[string]any{
					"spanID": "child", "operationName": "SELECT", "processID": "p2", "startTime": 110, "duration": 60,
					"references": []any{map[string]any{"refType": "CHILD_OF", "spanID": "root"}},
					"tags":       []any{map[string]any{"key": "otel.status_code", "value": "ERROR"}},
				},
			},
		}
	}
	h := newToolHarness(t, map[string]func(*http.Request) (int, any){
		"/api/traces/search": func(*http.Request) (int, any) {
			return http.StatusOK, map[string]any{"data": []any{trace("t1"), trace("t2"), trace("t3")}}
		},
	})
	got, text := h.call("search_traces", map[string]any{"service": "checkout", "start": 1, "end": 2, "limit": 2})
	if strings.Contains(text, `"spans"`) {
		t.Fatalf("full spans leaked into summaries: %s", text)
	}
	if got["count"] != float64(2) || got["truncated"] != true {
		t.Fatalf("count/truncated=%v/%v", got["count"], got["truncated"])
	}
	summary := got["traces"].([]any)[0].(map[string]any)
	want := map[string]any{
		"trace_id": "t1", "root_name": "GET /pay", "root_service": "checkout",
		"span_count": float64(2), "service_count": float64(2), "error_count": float64(1),
		"start_time_us": float64(100), "duration_us": float64(70),
	}
	for key, value := range want {
		if summary[key] != value {
			t.Errorf("%s=%v want %v", key, summary[key], value)
		}
	}
	// One extra trace is requested to detect truncation.
	if limit := h.requestsTo("/api/traces/search")[0].URL.Query().Get("limit"); limit != "3" {
		t.Fatalf("requested limit=%s want 3", limit)
	}
}

func TestListServicesUnionsTracesAndProfilesAndToleratesProfilesDown(t *testing.T) {
	h := newToolHarness(t, map[string]func(*http.Request) (int, any){
		"/api/traces/services": func(*http.Request) (int, any) {
			return http.StatusOK, map[string]any{"data": []any{"checkout", "api", "checkout"}}
		},
		"/api/profiles/services": func(*http.Request) (int, any) {
			return http.StatusServiceUnavailable, map[string]any{"error": "profiles unavailable"}
		},
	})
	got, _ := h.call("list_services", map[string]any{})
	services := got["services"].([]any)
	if len(services) != 2 || services[0] != "api" || services[1] != "checkout" || got["truncated"] != false {
		t.Fatalf("services=%v truncated=%v", services, got["truncated"])
	}
}

func TestFlamegraphSummaryRanksHotspotsAndCapsTopN(t *testing.T) {
	tree := map[string]any{"name": "root", "value": 100, "children": []any{
		map[string]any{"name": "main.main", "value": 100, "children": []any{
			map[string]any{"name": "main.hot", "value": 70},
			map[string]any{"name": "main.warm", "value": 20, "truncated": true},
		}},
	}}
	h := newToolHarness(t, map[string]func(*http.Request) (int, any){
		"/api/profiles/flamegraph": func(*http.Request) (int, any) { return http.StatusOK, tree },
	})
	got, _ := h.call("get_flamegraph_summary", map[string]any{"service": "checkout", "top_n": 2})
	hotspots := got["hotspots"].([]any)
	if len(hotspots) != 2 || got["truncated"] != true || got["source_truncated"] != true {
		t.Fatalf(
			"hotspots=%d truncated=%v source_truncated=%v",
			len(hotspots),
			got["truncated"],
			got["source_truncated"],
		)
	}
	top := hotspots[0].(map[string]any)
	if top["name"] != "main.main" || top["percent"] != float64(100) ||
		hotspots[1].(map[string]any)["name"] != "main.hot" {
		t.Fatalf("hotspots=%v", hotspots)
	}
	if !strings.HasPrefix(got["summary"].(string), "1. main.main") {
		t.Fatalf("summary=%q", got["summary"])
	}
}

func TestGetTraceRejectsInvalidTraceID(t *testing.T) {
	validID := "0123456789abcdef0123456789abcdef"
	h := newToolHarness(t, map[string]func(*http.Request) (int, any){
		"/api/traces/" + validID: func(*http.Request) (int, any) {
			return http.StatusOK, map[string]any{
				"data": []any{
					map[string]any{
						"traceID":   validID,
						"processes": map[string]any{"p1": map[string]any{"serviceName": "api"}},
						"spans": []any{
							map[string]any{
								"spanID": "s1", "operationName": "GET /", "processID": "p1",
								"startTime": 1000, "duration": 50,
							},
						},
					},
				},
			}
		},
	})
	for _, bad := range []string{"", "   ", "services", "search", "../v1/issues", "trace-1", strings.Repeat("a", 33)} {
		res, err := h.session.CallTool(context.Background(), &sdkmcp.CallToolParams{
			Name:      "get_trace",
			Arguments: map[string]any{"trace_id": bad},
		})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("get_trace(%q) unexpectedly succeeded: %#v", bad, res)
		}
	}
	h.mu.Lock()
	reqs := len(h.requests)
	h.mu.Unlock()
	if reqs != 0 {
		t.Fatalf("invalid trace_ids issued %d upstream requests", reqs)
	}

	got, _ := h.call("get_trace", map[string]any{"trace_id": validID})
	if got["trace_id"] != validID {
		t.Fatalf("valid get_trace returned %#v", got)
	}
}
