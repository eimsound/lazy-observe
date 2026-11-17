package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type addTokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t addTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func TestStreamableHTTPToolDiscoveryAndCall(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer project-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "up" {
			t.Fatalf("API request = %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			w,
			`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up"},"value":[1,"1"]}]}}`,
		)
	}))
	defer api.Close()
	mcpHTTP := httptest.NewServer(NewHTTPHandler(api.URL, api.Client()))
	defer mcpHTTP.Close()
	httpClient := &http.Client{Transport: addTokenTransport{base: http.DefaultTransport, token: "project-token"}}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(
		context.Background(),
		&sdkmcp.StreamableClientTransport{Endpoint: mcpHTTP.URL, HTTPClient: httpClient, DisableStandaloneSSE: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 14 {
		t.Fatalf("tool count = %d, want 14", len(tools.Tools))
	}
	result, err := session.CallTool(
		context.Background(),
		&sdkmcp.CallToolParams{Name: "query_metrics", Arguments: map[string]any{"query": "up"}},
	)
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("call result=%#v err=%v", result, err)
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok || !strings.Contains(text.Text, `"resultType":"vector"`) ||
		!strings.Contains(text.Text, `"truncated":false`) {
		t.Fatalf("call content = %#v", result.Content)
	}
}

func TestReplayMetadataToolsAreBoundedAndPrivacySafe(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer project-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/replay/sessions":
			if q := r.URL.Query(); q.Get("has_error") != "true" || q.Get("page") != "2" || q.Get("start") == "" ||
				q.Get("end") == "" {
				t.Fatalf("Replay list query = %v", q)
			}
			_, _ = io.WriteString(
				w,
				`{"sessions":[{"session_id":"session-1","started_at":1000,"last_seen_at":2000,"segment_count":2,"total_bytes":300,"has_error":true,"release":"v1","page_url":"https://alice:password@example.test/checkout?token=secret#card","project_id":42}],"total":51,"page":2,"page_size":50}`,
			)
		case "/api/replay/sessions/session-1":
			_, _ = io.WriteString(
				w,
				`{"session_id":"session-1","started_at":1000,"last_seen_at":2000,"segment_count":1,"total_bytes":300,"has_error":true,"release":"v1","page_url":"https://example.test/checkout?token=secret","project_id":42,"segments":[{"seq":0,"started_at":1000,"event_count":4,"size_bytes":300,"blob_key":"private"}],"markers":[{"type":"error","time":1100,"issue_id":"7","event_id":"event-1","title":"boom","level":"error","raw":"private"},{"type":"http","time":1200,"method":"POST","path":"https://example.test/api/orders?token=secret","status":503,"duration_ms":12.5,"request_body":"private"},{"type":"vital","time":1300,"name":"LCP","value":2500,"rating":"needs-improvement"}],"error_markers":[]}`,
			)
		default:
			t.Fatalf("unexpected API path %s", r.URL.Path)
		}
	}))
	defer api.Close()

	mcpHTTP := httptest.NewServer(NewHTTPHandler(api.URL, api.Client()))
	defer mcpHTTP.Close()
	httpClient := &http.Client{Transport: addTokenTransport{base: http.DefaultTransport, token: "project-token"}}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "replay-test", Version: "1"}, nil)
	session, err := client.Connect(
		context.Background(),
		&sdkmcp.StreamableClientTransport{Endpoint: mcpHTTP.URL, HTTPClient: httpClient, DisableStandaloneSSE: true},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	call := func(name string, arguments map[string]any) string {
		t.Helper()
		result, callErr := session.CallTool(
			context.Background(),
			&sdkmcp.CallToolParams{Name: name, Arguments: arguments},
		)
		if callErr != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("%s result=%#v err=%v", name, result, callErr)
		}
		content, ok := result.Content[0].(*sdkmcp.TextContent)
		if !ok {
			t.Fatalf("%s content=%#v", name, result.Content)
		}
		var decoded any
		if err := json.Unmarshal([]byte(content.Text), &decoded); err != nil {
			t.Fatalf("%s returned invalid JSON: %v", name, err)
		}
		return content.Text
	}

	list := call(
		"list_replays",
		map[string]any{"has_error": true, "start": "2026-07-18T00:00:00Z", "end": "2026-07-18T01:00:00Z", "page": 2},
	)
	if !strings.Contains(list, `"page_path":"/checkout"`) || strings.Contains(list, "secret") ||
		strings.Contains(list, "password") ||
		strings.Contains(list, "project_id") {
		t.Fatalf("unsafe Replay list result: %s", list)
	}
	detail := call("get_replay_metadata", map[string]any{"session_id": "session-1"})
	for _, want := range []string{`"type":"error"`, `"type":"http"`, `"type":"vital"`, `"path":"/api/orders"`, `"event_count":4`} {
		if !strings.Contains(detail, want) {
			t.Fatalf("Replay detail missing %s: %s", want, detail)
		}
	}
	for _, forbidden := range []string{"secret", "private", "project_id", "blob_key", "request_body", "error_markers"} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("Replay detail exposed %q: %s", forbidden, detail)
		}
	}
}
