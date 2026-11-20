package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpBearerTransport struct {
	base    http.RoundTripper
	token   string
	project string
}

func (t mcpBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	if t.project != "" {
		clone.Header.Set("X-LO-Project", t.project)
	}
	return t.base.RoundTrip(clone)
}

// TestMCPRealGatewayGetIssueContract verifies the MCP get_issue and list_issues
// tools against the real Gateway HTTP handlers and DTOs (not canned stubs).
func TestMCPRealGatewayGetIssueContract(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Checkout", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	ingestToken, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(ctx, "mcp-reader", "password-1234", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	_, userToken, err := store.CreateUserAPIToken(ctx, user.ID, "mcp-contract")
	if err != nil {
		t.Fatal(err)
	}

	vlogs := httptest.NewServer(&errorStorage{})
	defer vlogs.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + ln.Addr().String()

	gw := newMetadataTestGateway(store, Config{
		Tokens:     store,
		Store:      store,
		Logs:       vlogs.URL,
		Metrics:    vlogs.URL,
		Traces:     vlogs.URL,
		MCPBaseURL: baseURL,
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- gw.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = gw.Drain(shutdownCtx)
	})

	now := time.Now().UTC()
	event := map[string]any{
		"event_id":  "mcp-ev-1",
		"timestamp": now.Format(time.RFC3339Nano),
		"platform":  "javascript",
		"release":   "v2.4.0",
		"level":     "error",
		"message":   "checkout submit crashed",
		"exception": map[string]any{
			"type":  "TypeError",
			"value": "cannot read properties of undefined",
			"stacktrace": map[string]any{
				"frames": []any{
					map[string]any{
						"function":     "submitOrder",
						"filename":     "src/checkout.ts",
						"lineno":       87,
						"in_app":       true,
						"symbolicated": true,
					},
					map[string]any{
						"function":     "formatPrice",
						"filename":     "src/price.ts",
						"lineno":       19,
						"in_app":       true,
						"symbolicated": true,
					},
				},
			},
		},
		"contexts": map[string]any{
			"trace": map[string]any{"trace_id": "trace-mcp-1", "span_id": "span-1"},
		},
	}
	postErrorsTest(t, gw.App(), ingestToken, []any{event}, http.StatusOK)

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: mcpBearerTransport{
			base:    http.DefaultTransport,
			token:   userToken,
			project: "checkout",
		},
	}
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "real-gateway-contract", Version: "1"}, nil).Connect(
		ctx,
		&sdkmcp.StreamableClientTransport{
			Endpoint:             baseURL + "/mcp",
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("connect real gateway /mcp: %v", err)
	}
	defer session.Close()

	callTool := func(name string, args map[string]any) (map[string]any, string) {
		t.Helper()
		res, callErr := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		if callErr != nil || res == nil || res.IsError || len(res.Content) != 1 {
			t.Fatalf("%s res=%#v err=%v", name, res, callErr)
		}
		text := res.Content[0].(*sdkmcp.TextContent).Text
		var decoded map[string]any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatalf("%s invalid JSON: %v\n%s", name, err, text)
		}
		return decoded, text
	}

	listed, listedText := callTool("list_issues", map[string]any{})
	if strings.Contains(listedText, `"fingerprint"`) || strings.Contains(listedText, `"project_id"`) {
		t.Fatalf("list_issues leaked internal fields: %s", listedText)
	}
	issues, _ := listed["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("list_issues count=%d body=%s", len(issues), listedText)
	}
	issueID := int(issues[0].(map[string]any)["id"].(float64))

	detail, detailText := callTool("get_issue", map[string]any{"issue_id": issueID})
	if strings.Contains(detailText, `"fingerprint"`) || strings.Contains(detailText, `"project_id"`) {
		t.Fatalf("get_issue leaked internal fields: %s", detailText)
	}
	events, _ := detail["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("get_issue events=%#v body=%s", detail["events"], detailText)
	}
	ev := events[0].(map[string]any)
	if ev["event_id"] != "mcp-ev-1" ||
		ev["level"] != "error" ||
		ev["message"] != "checkout submit crashed" ||
		ev["exception_type"] != "TypeError" ||
		ev["exception_value"] != "cannot read properties of undefined" {
		t.Fatalf("get_issue event missing exception/message/level fields: %#v", ev)
	}
	frames, _ := ev["stack_frames"].([]any)
	if len(frames) != 2 {
		t.Fatalf("get_issue stack_frames=%#v", ev["stack_frames"])
	}
	innermost := frames[1].(map[string]any)
	if innermost["function"] != "formatPrice" ||
		innermost["filename"] != "src/price.ts" ||
		innermost["line"] != float64(19) ||
		innermost["in_app"] != true ||
		innermost["symbolicated"] != true {
		t.Fatalf("innermost frame mismatch: %#v", innermost)
	}
}
