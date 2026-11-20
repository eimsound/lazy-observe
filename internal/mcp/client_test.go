package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestLogsTruncationIsExplicit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if got := r.URL.Query().Get("limit"); got != "101" {
			t.Fatalf("upstream limit = %q, want 101", got)
		}
		for i := 0; i < 101; i++ {
			fmt.Fprintf(w, "{\"_msg\":\"row-%d\"}\n", i)
		}
	}))
	defer upstream.Close()
	api, err := NewAPIClient(upstream.URL, "secret", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := api.logs(context.Background(), url.Values{"query": {"*"}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := result["rows"].([]any)
	if len(rows) != MaxLogRows || result["truncated"] != true || result["limit"] != MaxLogRows {
		t.Fatalf("unexpected truncation result: %#v", result)
	}
}

func TestTraceTruncationAndCompactShape(t *testing.T) {
	spans := make([]any, MaxTraceSpans+1)
	for i := range spans {
		span := map[string]any{
			"spanID":        fmt.Sprintf("%04d", i),
			"operationName": fmt.Sprintf("op-%d", i),
			"duration":      float64(i + 1),
			"startTime":     float64(1000 + i),
			"processID":     "p",
			"tags":          []any{},
		}
		if i > 0 {
			span["references"] = []any{map[string]any{"refType": "CHILD_OF", "spanID": fmt.Sprintf("%04d", i-1)}}
		}
		spans[i] = span
	}
	raw := map[string]any{
		"data": []any{
			map[string]any{
				"traceID":   "trace",
				"spans":     spans,
				"processes": map[string]any{"p": map[string]any{"serviceName": "checkout"}},
			},
		},
	}
	result, err := compactTrace(raw)
	if err != nil {
		t.Fatal(err)
	}
	compact, _ := result["spans"].([]traceSpan)
	if len(compact) != MaxTraceSpans || result["truncated"] != true ||
		result["original_span_count"] != MaxTraceSpans+1 {
		t.Fatalf("unexpected trace truncation: %#v", result)
	}
	encoded, _ := json.Marshal(compact[1])
	if string(
		encoded,
	) != `{"span_id":"0001","parent_span_id":"0000","name":"op-1","service":"checkout","duration_us":2,"depth":1,"error":false,"start_time_us":1001}` {
		t.Fatalf("compact span = %s", encoded)
	}
}

func TestParseDSN(t *testing.T) {
	api, err := ParseDSN("https://observe.example/p/default@token-value")
	if err != nil {
		t.Fatal(err)
	}
	if api.BaseURL != "https://observe.example" || api.Token != "token-value" {
		t.Fatalf("parsed DSN = %#v", api)
	}
}

func TestParseDSNSelectsProjectOnEveryQuery(t *testing.T) {
	seen := make(chan [2]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- [2]string{r.Header.Get("Authorization"), r.Header.Get("X-LO-Project")}
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	defer server.Close()
	api, err := ParseDSN(server.URL + "/p/checkout@lou_reader")
	if err != nil {
		t.Fatal(err)
	}
	api.HTTP = server.Client()
	_, _ = api.get(t.Context(), "/api/v1/issues", nil)
	got := <-seen
	if got[0] != "Bearer lou_reader" || got[1] != "checkout" {
		t.Fatalf("Authorization=%q X-LO-Project=%q", got[0], got[1])
	}
}

func TestAPIClientDefaultsToBoundedTimeoutAndHandlesNilHTTP(t *testing.T) {
	api, err := NewAPIClient("https://observe.example", "lou_reader", nil)
	if err != nil {
		t.Fatal(err)
	}
	if api.HTTP == nil || api.HTTP.Timeout <= 0 {
		t.Fatalf("default HTTP client missing timeout: %#v", api.HTTP)
	}
	parsed, err := ParseDSN("https://observe.example/p/default@lou_reader")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.HTTP == nil || parsed.HTTP.Timeout <= 0 {
		t.Fatalf("ParseDSN HTTP client missing timeout: %#v", parsed.HTTP)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// Manually constructed APIClient with HTTP == nil must not panic.
	bare := &APIClient{BaseURL: server.URL, Token: "lou_reader"}
	if _, err := bare.json(t.Context(), "/api/v1/issues", nil); err != nil {
		t.Fatalf("bare APIClient.json with nil HTTP failed: %v", err)
	}
}
