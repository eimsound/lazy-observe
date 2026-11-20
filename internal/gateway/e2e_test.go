package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres/v2"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	alertpkg "github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/embeddedpg"
	"github.com/eimsound/lazy-observe/internal/meta"
)

// TestStorageGatewayE2E is opt-in because it starts the three release binaries.
// Run with LO_E2E_BINARY=/path/to/lazy-observe LO_E2E_STORAGE_BIN_DIR=/path/to/storage-bins
// go test ./internal/gateway -run StorageGatewayE2E -v.
func TestStorageGatewayE2E(t *testing.T) {
	binary := os.Getenv("LO_E2E_BINARY")
	if binary == "" {
		t.Skip("LO_E2E_BINARY is not set")
	}
	binDir := os.Getenv("LO_E2E_STORAGE_BIN_DIR")
	if binDir == "" {
		t.Skip("LO_E2E_STORAGE_BIN_DIR is not set")
	}
	dataDir := t.TempDir()
	var llmMu sync.Mutex
	var llmPrompts []string
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&request) != nil ||
			len(request.Messages) == 0 {
			http.Error(w, "invalid fake LLM request", http.StatusBadRequest)
			return
		}
		llmMu.Lock()
		llmPrompts = append(llmPrompts, request.Messages[len(request.Messages)-1].Content)
		llmMu.Unlock()
		_ = json.NewEncoder(w).
			Encode(map[string]any{"model": "fake-e2e", "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": `{"summary":"checkout trace shows a database warning","probable_causes":[{"cause":"database latency","confidence":"high","evidence":"trace-linked warning and checkout span"}],"suggested_actions":["inspect database latency"]}`}}}})
	}))
	defer fakeLLM.Close()
	metaDBURL := filepath.Join(dataDir, "meta", "lazy-observe.db")
	if os.Getenv("LO_TEST_PG") == "1" {
		metaDBURL = startEmbeddedPostgresE2E(t)
	}

	ctx := context.Background()
	store, err := meta.OpenStore(ctx, metaDBURL)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := store.CreateProject(ctx, "Default", "default")
	if err != nil {
		t.Fatal(err)
	}
	token1, err := store.CreateProjectAPIToken(ctx, p1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(ctx, "admin", "e2e-admin-password", "admin"); err != nil {
		t.Fatal(err)
	}
	// Project tokens are ingest-only; every read uses a user API token.
	readerToken := userQueryToken(t, store, "e2e-reader")
	read1 := e2eReader{token: readerToken, project: "default"}
	read2 := e2eReader{token: readerToken, project: "p2"}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	base := "http://" + addr
	var output lockedBuffer
	cmd := exec.Command(binary, "server", "--listen", addr, "--storage-bin-dir", binDir, "--data-dir", dataDir)
	cmd.Env = append(
		os.Environ(),
		"LO_META_DB_URL="+metaDBURL,
		"LO_SELF_MONITORING_ENABLED=false",
		"LO_SECURITY_OUTBOUND_ALLOWLIST=127.0.0.1/32",
	)
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server did not exit cleanly after SIGTERM: %v", err)
			}
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("server did not exit within 20s after SIGTERM")
		}
		if t.Failed() {
			t.Logf("server output:\n%s", output.String())
		}
	})

	client := &http.Client{Timeout: 5 * time.Second}
	waitFor(t, 30*time.Second, func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	adminCookie := loginE2E(t, client, base, "admin", "e2e-admin-password")

	resp, err := client.Get(base + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-session management status = %d, want 401", resp.StatusCode)
	}
	var p2 projectResponse
	jsonAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/projects",
		adminCookie,
		map[string]string{"name": "Project Two", "slug": "p2"},
		&p2,
		http.StatusOK,
	)
	var issued struct {
		Value string `json:"value"`
	}
	jsonAPI(
		t,
		client,
		http.MethodPost,
		fmt.Sprintf("%s/api/projects/%s/tokens", base, p2.ID),
		adminCookie,
		map[string]string{"name": "e2e-ingest"},
		&issued,
		http.StatusCreated,
	)
	token2 := issued.Value
	jsonAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/users",
		adminCookie,
		map[string]string{"username": "viewer", "password": "viewer-password", "role": "viewer"},
		nil,
		http.StatusCreated,
	)
	viewerCookie := loginE2E(t, client, base, "viewer", "viewer-password")
	jsonAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/projects",
		viewerCookie,
		map[string]string{"name": "Denied", "slug": "denied"},
		nil,
		http.StatusForbidden,
	)

	resp, err = client.Get(base + "/api/v1/query?query=up")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token query status = %d, want 401", resp.StatusCode)
	}

	errorEvent := func(id, typ, value string, line int) map[string]any {
		return map[string]any{
			"event_id":  id,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"platform":  "go",
			"level":     "error",
			"message":   "checkout failed",
			"exception": map[string]any{
				"type":  typ,
				"value": value,
				"stacktrace": map[string]any{
					"frames": []map[string]any{
						{
							"function": "checkout.Handle.func1",
							"filename": "/build/checkout/handler.go",
							"lineno":   line,
							"in_app":   true,
						},
					},
				},
			},
			"contexts": map[string]any{
				"trace": map[string]string{"trace_id": "e2e-trace-id", "span_id": "e2e-span-id"},
			},
			"session_id": "e2e-user",
		}
	}
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token1,
		map[string]any{
			"events": []any{
				errorEvent("error-1", "panic", "order 11", 10),
				errorEvent("error-2", "panic", "order 22", 20),
				errorEvent("error-3", "panic", "order 33", 30),
			},
		},
	)
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token1,
		map[string]any{"events": []any{errorEvent("error-other", "TypeError", "order 11", 10)}},
	)
	type issueWire struct {
		ID          int    `json:"id"`
		Count       int64  `json:"count"`
		Status      string `json:"status"`
		RegressedAt *int64 `json:"regressed_at"`
	}
	var issueList struct {
		Issues []issueWire `json:"issues"`
	}
	waitFor(t, 15*time.Second, func() bool {
		issueList.Issues = nil
		jsonProjectAPI(
			t,
			client,
			http.MethodGet,
			base+"/api/issues?sort=count",
			adminCookie,
			"default",
			nil,
			&issueList,
			http.StatusOK,
		)
		return len(issueList.Issues) == 2 && issueList.Issues[0].Count == 3
	})
	if len(issueList.Issues) != 2 || issueList.Issues[0].Count != 3 {
		t.Fatalf("error grouping: %#v", issueList.Issues)
	}
	grouped := issueList.Issues[0]
	jsonProjectAPI(
		t,
		client,
		http.MethodPatch,
		fmt.Sprintf("%s/api/issues/%d", base, grouped.ID),
		adminCookie,
		"default",
		map[string]string{"status": "resolved"},
		&grouped,
		http.StatusOK,
	)
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token1,
		map[string]any{"events": []any{errorEvent("error-4", "panic", "order 44", 99)}},
	)
	waitFor(t, 15*time.Second, func() bool {
		jsonProjectAPI(
			t,
			client,
			http.MethodGet,
			fmt.Sprintf("%s/api/issues/%d", base, grouped.ID),
			adminCookie,
			"default",
			nil,
			&grouped,
			http.StatusOK,
		)
		return grouped.Status == "open" && grouped.Count == 4 && grouped.RegressedAt != nil
	})
	if grouped.Status != "open" || grouped.Count != 4 || grouped.RegressedAt == nil {
		t.Fatalf("error regression: %#v", grouped)
	}
	jsonProjectAPI(
		t,
		client,
		http.MethodPatch,
		fmt.Sprintf("%s/api/issues/%d", base, grouped.ID),
		adminCookie,
		"default",
		map[string]string{"status": "ignored"},
		&grouped,
		http.StatusOK,
	)
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token1,
		map[string]any{"events": []any{errorEvent("error-5", "panic", "order 55", 101)}},
	)
	waitFor(t, 15*time.Second, func() bool {
		jsonProjectAPI(
			t,
			client,
			http.MethodGet,
			fmt.Sprintf("%s/api/issues/%d", base, grouped.ID),
			adminCookie,
			"default",
			nil,
			&grouped,
			http.StatusOK,
		)
		return grouped.Status == "ignored" && grouped.Count == 5
	})
	if grouped.Status != "ignored" || grouped.Count != 5 {
		t.Fatalf("ignored error: %#v", grouped)
	}
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token2,
		map[string]any{"events": []any{errorEvent("p2-error", "P2Only", "private", 1)}},
	)
	var p2IssueList struct {
		Issues []issueWire `json:"issues"`
	}
	waitFor(t, 15*time.Second, func() bool {
		p2IssueList.Issues = nil
		jsonProjectAPI(
			t,
			client,
			http.MethodGet,
			base+"/api/issues",
			adminCookie,
			"p2",
			nil,
			&p2IssueList,
			http.StatusOK,
		)
		return len(p2IssueList.Issues) == 1
	})
	jsonProjectAPI(
		t,
		client,
		http.MethodGet,
		base+"/api/issues",
		adminCookie,
		"default",
		nil,
		&issueList,
		http.StatusOK,
	)
	if len(issueList.Issues) != 2 {
		t.Fatalf("cross-project error leak: %#v", issueList.Issues)
	}
	var rawEvent map[string]any
	waitFor(t, 15*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/issues/%d/events/error-1", base, grouped.ID), nil)
		addSessionCookies(req, adminCookie)
		req.Header.Set("X-LO-Project", "default")
		response, requestErr := client.Do(req)
		if requestErr != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&rawEvent) == nil
	})
	contexts, _ := rawEvent["contexts"].(map[string]any)
	trace, _ := contexts["trace"].(map[string]any)
	if trace["trace_id"] != "e2e-trace-id" {
		t.Fatalf("raw error trace context: %#v", rawEvent)
	}
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/vitals",
		token1,
		map[string]any{
			"vitals": []any{
				map[string]any{
					"name":      "LCP",
					"value":     321.5,
					"page":      "/checkout",
					"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
				},
			},
		},
	)
	waitFor(t, 15*time.Second, func() bool {
		return metricValuesAre(
			authorizedGet(
				t,
				client,
				base+"/api/v1/query?"+url.Values{"query": []string{`web_vitals_lcp{page="/checkout"}`}}.Encode(),
				read1,
			),
			"321.5",
		)
	})
	t.Log("error AC2-AC5: grouping/type split, lifecycle, raw trace context, isolation, and Web Vitals passed")

	metricName := fmt.Sprintf("lazy_observe_e2e_%d", time.Now().UnixNano())
	postProto(t, client, base+"/v1/metrics", token1, metricPayload(metricName, 111))
	postProto(t, client, base+"/v1/metrics", token2, metricPayload(metricName, 222))
	var metricBody []byte
	waitFor(t, 15*time.Second, func() bool {
		metricBody = authorizedGet(
			t,
			client,
			base+"/api/v1/query?"+url.Values{"query": []string{metricName}}.Encode(),
			read2,
		)
		return metricValuesAre(metricBody, "222")
	})
	metricBody = sessionGet(
		t,
		client,
		base+"/api/v1/query?"+url.Values{"query": []string{metricName}}.Encode(),
		adminCookie,
		"p2",
	)
	if !metricValuesAre(metricBody, "222") {
		t.Fatalf("session metric query did not return p2 value: %s", metricBody)
	}
	t.Log("AC3: token and session channels queried the p2 OTLP metric; management auth/RBAC passed")

	notifications := make(chan alertpkg.Notification, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n alertpkg.Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		notifications <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	var channel notificationChannelResponse
	jsonProjectAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/channels",
		adminCookie,
		"p2",
		map[string]any{"type": "webhook", "name": "e2e-webhook", "config": map[string]any{"url": webhook.URL}},
		&channel,
		http.StatusCreated,
	)
	channelID, err := strconv.Atoi(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	var platformConfig projectAlertConfigResponse
	jsonAPI(
		t,
		client,
		http.MethodPut,
		fmt.Sprintf("%s/api/projects/%s/alert-config", base, p2.ID),
		adminCookie,
		map[string]any{
			"new_issue":                    false,
			"regression":                   false,
			"error_rate_threshold_per_min": 0,
			"channel_ids":                  []int{channelID},
		},
		&platformConfig,
		http.StatusOK,
	)
	jsonAPI(
		t,
		client,
		http.MethodGet,
		fmt.Sprintf("%s/api/projects/%s/alert-config", base, p2.ID),
		adminCookie,
		nil,
		&platformConfig,
		http.StatusOK,
	)
	if platformConfig.NewIssue || platformConfig.Regression || platformConfig.ErrorRateThresholdPerMinute != 0 ||
		len(platformConfig.ChannelIDs) != 1 ||
		platformConfig.ChannelIDs[0] != channel.ID {
		t.Fatalf("project alert config round trip: %#v", platformConfig)
	}
	ruleName := fmt.Sprintf("AlwaysFiring%d", time.Now().UnixNano())
	var rule alertRuleResponse
	jsonProjectAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/alert-rules",
		adminCookie,
		"p2",
		map[string]any{
			"name":        ruleName,
			"expr":        metricName + " > 0",
			"for":         "0s",
			"severity":    "critical",
			"channel_ids": []string{channel.ID},
		},
		&rule,
		http.StatusCreated,
	)
	firing := waitNotification(t, notifications, 75*time.Second)
	if firing.Status != "firing" || firing.Project != "p2" || firing.Rule != ruleName || len(firing.Alerts) == 0 ||
		firing.Alerts[0].Labels["project_id"] != "p2" {
		t.Fatalf("unexpected firing notification: %#v", firing)
	}
	t.Log("alert AC3: the native evaluator queried a real metric and the webhook received its grouped notification")

	jsonProjectAPI(
		t,
		client,
		http.MethodPatch,
		fmt.Sprintf("%s/api/alert-rules/%s", base, rule.ID),
		adminCookie,
		"p2",
		map[string]any{"expr": "vector(0) > 0"},
		&rule,
		http.StatusOK,
	)
	resolved := waitNotification(t, notifications, 45*time.Second)
	if resolved.Status != "resolved" || resolved.Rule != ruleName {
		t.Fatalf("unexpected resolved notification: %#v", resolved)
	}
	t.Log("alert AC5: updating the persisted rule changed the next evaluation and delivered resolved")

	var silence silenceResponse
	jsonProjectAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/silences",
		adminCookie,
		"p2",
		map[string]any{
			"matchers":  []map[string]string{{"name": "alertname", "op": "=", "value": ruleName}},
			"starts_at": time.Now().Add(-time.Minute),
			"ends_at":   time.Now().Add(10 * time.Minute),
			"comment":   "e2e",
		},
		&silence,
		http.StatusCreated,
	)
	jsonProjectAPI(
		t,
		client,
		http.MethodPatch,
		fmt.Sprintf("%s/api/alert-rules/%s", base, rule.ID),
		adminCookie,
		"p2",
		map[string]any{"expr": metricName + " > 0"},
		&rule,
		http.StatusOK,
	)
	select {
	case n := <-notifications:
		t.Fatalf("silenced alert produced notification: %#v", n)
	case <-time.After(50 * time.Second):
	}
	jsonProjectAPI(
		t,
		client,
		http.MethodDelete,
		fmt.Sprintf("%s/api/silences/%s", base, silence.ID),
		adminCookie,
		"p2",
		nil,
		nil,
		http.StatusNoContent,
	)
	jsonProjectAPI(
		t,
		client,
		http.MethodPatch,
		fmt.Sprintf("%s/api/alert-rules/%s", base, rule.ID),
		adminCookie,
		"p2",
		map[string]any{"expr": "vector(0) > 0"},
		&rule,
		http.StatusOK,
	)
	_ = waitNotification(t, notifications, 45*time.Second)
	t.Log("alert AC4: active silence suppressed a transition; deleting it restored notifications")

	maliciousURL := base + "/api/v1/query?" + url.Values{
		"query":           []string{metricName},
		"extra_filters[]": []string{`{project_id="default"}`},
	}.Encode()
	metricBody = authorizedGet(t, client, maliciousURL, read2)
	if !metricValuesAre(metricBody, "222") {
		t.Fatalf("malicious-filter response escaped p2 project: %s", metricBody)
	}
	t.Log("AC4: malicious extra_filters[] still returned only p2 value 222")

	defaultMarker := fmt.Sprintf("lazy-observe-log-default-%d", time.Now().UnixNano())
	marker := fmt.Sprintf("lazy-observe-log-p2-%d", time.Now().UnixNano())
	postProto(t, client, base+"/v1/logs", token1, logsPayload(defaultMarker))
	postProto(t, client, base+"/v1/logs", token2, logsPayload(marker))
	logsQuery := base + "/api/v1/logs/query?" + url.Values{"query": []string{`_msg:="` + marker + `"`}}.Encode()
	var logBody []byte
	waitFor(t, 15*time.Second, func() bool {
		logBody = authorizedGet(t, client, logsQuery, read2)
		return bytes.Contains(logBody, []byte(marker))
	})
	logBody = sessionGet(t, client, logsQuery, adminCookie, "p2")
	if !bytes.Contains(logBody, []byte(marker)) {
		t.Fatalf("session p2 log query missed marker: %s", logBody)
	}
	defaultLogsQuery := base + "/api/v1/logs/query?" + url.Values{
		"query": []string{`_msg:="` + defaultMarker + `"`},
	}.Encode()
	otherBody := authorizedGet(t, client, defaultLogsQuery, read2)
	if bytes.Contains(otherBody, []byte(defaultMarker)) {
		t.Fatalf("p2 project saw default log: %s", otherBody)
	}

	connectMCP := func(reader e2eReader) *sdkmcp.ClientSession {
		t.Helper()
		mcpHTTPClient := *client
		mcpHTTPClient.Transport = bearerTokenTransport{
			base: http.DefaultTransport, token: reader.token, project: reader.project,
		}
		mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "lazy-observe-e2e", Version: "1.0.0"}, nil)
		session, err := mcpClient.Connect(
			ctx,
			&sdkmcp.StreamableClientTransport{
				Endpoint:             base + "/mcp",
				HTTPClient:           &mcpHTTPClient,
				DisableStandaloneSSE: true,
			},
			nil,
		)
		if err != nil {
			t.Fatalf("connect MCP: %v", err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	defaultMCP := connectMCP(read1)
	tools, err := defaultMCP.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) < 12 {
		t.Fatalf("MCP list tools: count=%d err=%v", len(tools.Tools), err)
	}
	metricResult := callMCPToolE2E(t, ctx, defaultMCP, "query_metrics", map[string]any{"query": metricName})
	if !bytes.Contains(metricResult, []byte("111")) {
		t.Fatalf("MCP metric missed seed value: %s", metricResult)
	}
	logResult := callMCPToolE2E(
		t,
		ctx,
		defaultMCP,
		"query_logs",
		map[string]any{"query": `_msg:="` + defaultMarker + `"`, "limit": 10},
	)
	if !bytes.Contains(logResult, []byte(defaultMarker)) {
		t.Fatalf("MCP logs missed seed row: %s", logResult)
	}
	issuesResult := callMCPToolE2E(t, ctx, defaultMCP, "list_issues", map[string]any{"q": "panic"})
	if !bytes.Contains(issuesResult, []byte("panic")) {
		t.Fatalf("MCP issues missed ingested error: %s", issuesResult)
	}

	p2MCP := connectMCP(read2)
	isolationMetric := callMCPToolE2E(t, ctx, p2MCP, "query_metrics", map[string]any{"query": metricName + " == 111"})
	if bytes.Contains(isolationMetric, []byte("111")) {
		t.Fatalf("p2 MCP saw default metric: %s", isolationMetric)
	}
	isolationIssues := callMCPToolE2E(t, ctx, p2MCP, "list_issues", map[string]any{"q": "panic"})
	if bytes.Contains(isolationIssues, []byte("panic")) {
		t.Fatalf("p2 MCP saw default issue: %s", isolationIssues)
	}

	stdioClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "lazy-observe-stdio-e2e", Version: "1.0.0"}, nil)
	stdioSession, err := stdioClient.Connect(
		ctx,
		&sdkmcp.CommandTransport{Command: exec.Command(binary, "mcp", "--dsn", base+"/p/default@"+readerToken)},
		nil,
	)
	if err != nil {
		t.Fatalf("connect stdio MCP: %v", err)
	}
	t.Cleanup(func() { _ = stdioSession.Close() })
	stdioTools, err := stdioSession.ListTools(ctx, nil)
	if err != nil || len(stdioTools.Tools) < 12 {
		t.Fatalf("stdio MCP list tools: count=%d err=%v", len(stdioTools.Tools), err)
	}
	stdioMetric := callMCPToolE2E(t, ctx, stdioSession, "query_metrics", map[string]any{"query": metricName})
	if !bytes.Contains(stdioMetric, []byte("111")) {
		t.Fatalf("stdio MCP metric missed seed value: %s", stdioMetric)
	}
	t.Log(
		"MCP AC2-AC4: official SDK streamable HTTP and stdio clients passed tool discovery, real queries, and project isolation",
	)

	defaultTraceID := "11111111111111111111111111111111"
	p2TraceID := "22222222222222222222222222222222"
	postData(
		t,
		client,
		base+"/v1/traces",
		token1,
		"application/json",
		tracePayload(defaultTraceID, "default-service", "default-operation"),
	)
	postData(
		t,
		client,
		base+"/v1/traces",
		token2,
		"application/json",
		tracePayload(p2TraceID, "p2-service", "p2-operation"),
	)
	var traceBody []byte
	waitFor(t, 60*time.Second, func() bool {
		traceBody = authorizedGet(t, client, base+"/api/traces/"+p2TraceID, read2)
		return bytes.Contains(traceBody, []byte(p2TraceID))
	})
	traceBody = sessionGet(t, client, base+"/api/traces/"+p2TraceID, adminCookie, "p2")
	if !bytes.Contains(traceBody, []byte(p2TraceID)) {
		t.Fatalf("session p2 trace query missed trace: %s", traceBody)
	}
	crossTrace := authorizedGet(t, client, base+"/api/traces/"+defaultTraceID, read2)
	if bytes.Contains(crossTrace, []byte(defaultTraceID)) {
		t.Fatalf("p2 project saw default trace: %s", crossTrace)
	}
	t.Log("AC4: p2 could not query default logs or trace; AC3: both channels queried p2 logs/traces")

	llmAt := time.Now().UTC().Add(-time.Second)
	postData(
		t,
		client,
		base+"/v1/traces",
		token1,
		"application/json",
		llmTracePayload("33333333333333333333333333333333", "checkout-ai", "gpt-4o-mini-2024-07-18", 2000, 500, llmAt),
	)
	postData(
		t,
		client,
		base+"/v1/traces",
		token2,
		"application/json",
		llmTracePayload(
			"44444444444444444444444444444444",
			"p2-agent",
			"unknown-e2e-model",
			90,
			10,
			llmAt.Add(time.Millisecond),
		),
	)
	type llmE2ESummary struct {
		TotalCalls        int64   `json:"total_calls"`
		TotalInputTokens  int64   `json:"total_input_tokens"`
		TotalOutputTokens int64   `json:"total_output_tokens"`
		TotalCost         float64 `json:"total_cost"`
		ByModel           []struct {
			Model    string  `json:"model"`
			Cost     float64 `json:"cost"`
			Unpriced bool    `json:"unpriced"`
		} `json:"by_model"`
	}
	var defaultLLMSummary llmE2ESummary
	waitFor(t, 30*time.Second, func() bool {
		body := authorizedGet(t, client, base+"/api/llm/summary", read1)
		return json.Unmarshal(body, &defaultLLMSummary) == nil && defaultLLMSummary.TotalCalls == 1
	})
	if defaultLLMSummary.TotalInputTokens != 2000 || defaultLLMSummary.TotalOutputTokens != 500 ||
		len(defaultLLMSummary.ByModel) != 1 ||
		defaultLLMSummary.ByModel[0].Model != "gpt-4o-mini-2024-07-18" ||
		defaultLLMSummary.ByModel[0].Unpriced ||
		math.Abs(defaultLLMSummary.TotalCost-0.0006) > 1e-12 {
		t.Fatalf("default LLM summary: %#v", defaultLLMSummary)
	}
	var calls struct {
		Calls []llmCall `json:"calls"`
	}
	if body := authorizedGet(
		t,
		client,
		base+"/api/llm/calls?limit=10",
		read1,
	); json.Unmarshal(body, &calls) != nil || len(calls.Calls) != 1 || calls.Calls[0].TraceID != "33333333333333333333333333333333" ||
		calls.Calls[0].Service != "checkout-ai" {
		t.Fatalf("default LLM calls: body=%s calls=%#v", body, calls)
	}
	seriesURL := base + "/api/llm/timeseries?" + url.Values{
		"start": {
			llmAt.Add(-time.Minute).Format(time.RFC3339Nano),
		},
		"end":    {llmAt.Add(time.Minute).Format(time.RFC3339Nano)},
		"step":   {"1m"},
		"metric": {"calls"},
	}.Encode()
	var series struct {
		Points []llmTimeseriesPoint `json:"points"`
	}
	if body := authorizedGet(t, client, seriesURL, read1); json.Unmarshal(body, &series) != nil {
		t.Fatalf("default LLM timeseries: %s", body)
	}
	var seriesCalls float64
	for _, point := range series.Points {
		seriesCalls += point.Value
	}
	if seriesCalls != 1 {
		t.Fatalf("default LLM timeseries calls=%v points=%#v", seriesCalls, series.Points)
	}
	var p2LLMSummary llmE2ESummary
	if body := sessionGet(
		t,
		client,
		base+"/api/llm/summary",
		adminCookie,
		"p2",
	); json.Unmarshal(body, &p2LLMSummary) != nil || p2LLMSummary.TotalCalls != 1 || p2LLMSummary.TotalInputTokens != 90 || p2LLMSummary.TotalCost != 0 || len(p2LLMSummary.ByModel) != 1 ||
		!p2LLMSummary.ByModel[0].Unpriced {
		t.Fatalf("p2 LLM summary/isolation: body=%s summary=%#v", body, p2LLMSummary)
	}
	t.Log(
		"LLM AC3-AC4: real VictoriaTraces summary/calls/timeseries, token+session auth, pricing, unknown-model, and project isolation passed",
	)

	var aiSettings map[string]any
	jsonAPI(
		t,
		client,
		http.MethodPut,
		base+"/api/ai/settings",
		adminCookie,
		map[string]any{
			"enabled": true, "provider": "openai", "base_url": fakeLLM.URL, "model": "fake-e2e",
			"api_key": "e2e-not-secret",
		},
		&aiSettings,
		http.StatusOK,
	)
	if aiSettings["enabled"] != true || aiSettings["has_api_key"] != true {
		t.Fatalf("dynamic AI settings: %#v", aiSettings)
	}

	traceLogMarker := "issue-ai-database-warning-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	postProto(t, client, base+"/v1/logs", token1, traceLogsPayload(defaultTraceID, traceLogMarker))
	traceLogQuery := base + "/api/v1/logs/query?" + url.Values{
		"query": []string{`trace_id:="` + defaultTraceID + `"`},
	}.Encode()
	waitFor(t, 15*time.Second, func() bool {
		return bytes.Contains(authorizedGet(t, client, traceLogQuery, read1), []byte(traceLogMarker))
	})
	aiEvent := func(id string, timestamp time.Time) map[string]any {
		return map[string]any{
			"event_id":  id,
			"timestamp": timestamp.UTC().Format(time.RFC3339Nano),
			"platform":  "go",
			"release":   "v-ai-e2e",
			"level":     "error",
			"message":   "checkout investigation failed",
			"exception": map[string]any{
				"type":  "AIInvestigationError",
				"value": "database warning",
				"stacktrace": map[string]any{
					"frames": []any{
						map[string]any{
							"function": "checkout.Investigate",
							"filename": "/srv/checkout/investigate.go",
							"lineno":   42,
							"in_app":   true,
						},
					},
				},
			},
			"tags": map[string]string{
				"browser": "Chrome-e2e",
				"os":      "Linux-e2e",
			},
			"contexts": map[string]any{
				"trace": map[string]string{"trace_id": defaultTraceID, "span_id": "0102030405060708"},
			},
		}
	}
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token1,
		map[string]any{"events": []any{aiEvent("issue-ai-1", time.Now())}},
	)
	var aiIssues struct {
		Issues []issueWire `json:"issues"`
	}
	waitFor(t, 15*time.Second, func() bool {
		aiIssues.Issues = nil
		jsonProjectAPI(
			t,
			client,
			http.MethodGet,
			base+"/api/issues?q=AIInvestigationError",
			adminCookie,
			"default",
			nil,
			&aiIssues,
			http.StatusOK,
		)
		return len(aiIssues.Issues) == 1
	})
	if len(aiIssues.Issues) != 1 {
		t.Fatalf("AI issue not found: %#v", aiIssues.Issues)
	}
	waitFor(t, 15*time.Second, func() bool {
		req, _ := http.NewRequest(
			http.MethodGet,
			fmt.Sprintf("%s/api/issues/%d/events/issue-ai-1", base, aiIssues.Issues[0].ID),
			nil,
		)
		addSessionCookies(req, adminCookie)
		req.Header.Set("X-LO-Project", "default")
		response, requestErr := client.Do(req)
		if requestErr != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	aiEndpoint := fmt.Sprintf("%s/api/issues/%d/ai-analysis", base, aiIssues.Issues[0].ID)
	var analysis map[string]any
	waitFor(t, 30*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodGet, aiEndpoint, nil)
		addSessionCookies(req, adminCookie)
		req.Header.Set("X-LO-Project", "default")
		response, requestErr := client.Do(req)
		if requestErr != nil {
			return false
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return false
		}
		analysis = nil
		return json.NewDecoder(response.Body).Decode(&analysis) == nil && analysis["summary"] != ""
	})
	if analysis["cached"] != true {
		t.Fatalf("automatic AI analysis: %#v", analysis)
	}
	expectedPromptEvidence := []string{"checkout.Investigate", traceLogMarker, "default-operation"}
	matchingPrompts := func() (string, int) {
		llmMu.Lock()
		defer llmMu.Unlock()
		var prompt string
		var calls int
		for _, candidate := range llmPrompts {
			matches := true
			for _, expected := range expectedPromptEvidence {
				matches = matches && strings.Contains(candidate, expected)
			}
			if matches {
				prompt = candidate
				calls++
			}
		}
		return prompt, calls
	}
	prompt, llmCalls := matchingPrompts()
	for _, expected := range expectedPromptEvidence {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("real-storage AI prompt missing %q:\n%s", expected, prompt)
		}
	}
	if llmCalls != 1 {
		t.Fatalf("automatic AI analysis matching calls=%d", llmCalls)
	}
	jsonProjectAPI(t, client, http.MethodPost, aiEndpoint, adminCookie, "default", nil, &analysis, http.StatusOK)
	_, cachedCalls := matchingPrompts()
	if analysis["cached"] != true || cachedCalls != llmCalls {
		t.Fatalf("POST automatic AI cache: %#v matching_calls=%d->%d", analysis, llmCalls, cachedCalls)
	}
	postJSONTokenE2E(
		t,
		client,
		base+"/api/events/errors",
		token1,
		map[string]any{"events": []any{aiEvent("issue-ai-2", time.Now())}},
	)
	waitFor(t, 15*time.Second, func() bool {
		analysis = nil
		jsonProjectAPI(t, client, http.MethodGet, aiEndpoint, adminCookie, "default", nil, &analysis, http.StatusOK)
		return analysis["stale"] == true
	})
	if analysis["stale"] != true {
		t.Fatalf("stale AI cache: %#v", analysis)
	}
	// The test client and server are separate processes. Leave enough margin for
	// the refreshed server timestamp to be later than the client event timestamp.
	time.Sleep(50 * time.Millisecond)
	analysis = nil
	jsonProjectAPI(
		t,
		client,
		http.MethodPost,
		aiEndpoint+"?refresh=1",
		adminCookie,
		"default",
		nil,
		&analysis,
		http.StatusOK,
	)
	_, refreshedCalls := matchingPrompts()
	if analysis["cached"] != false || analysis["stale"] == true || cachedCalls != 1 || refreshedCalls != 2 {
		t.Fatalf("refreshed AI analysis=%#v matching_calls=%d->%d", analysis, cachedCalls, refreshedCalls)
	}
	t.Log(
		"issue AI AC3: a new Issue was analyzed asynchronously with real VictoriaLogs/VictoriaTraces context; notification-safe cache, stale, and refresh behavior passed",
	)
}

// TestSelfMonitoringDisabledE2E retains the task runner's existing selector while
// covering both explicit modes in isolated server processes.
func TestSelfMonitoringDisabledE2E(t *testing.T) {
	t.Run("enabled", func(t *testing.T) { runSelfMonitoringModeE2E(t, true) })
	t.Run("disabled", func(t *testing.T) { runSelfMonitoringModeE2E(t, false) })
}

func runSelfMonitoringModeE2E(t *testing.T, enabled bool) {
	t.Helper()
	binary := os.Getenv("LO_E2E_BINARY")
	binDir := os.Getenv("LO_E2E_STORAGE_BIN_DIR")
	if binary == "" || binDir == "" {
		t.Skip("LO_E2E_BINARY and LO_E2E_STORAGE_BIN_DIR are required")
	}
	dataDir := t.TempDir()
	metaPath := filepath.Join(dataDir, "meta", "lazy-observe.db")
	store, err := meta.OpenStore(context.Background(), metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(context.Background(), "admin", "self-monitoring-e2e-password", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	base := "http://" + addr
	var output lockedBuffer
	cmd := exec.Command(binary, "server", "--listen", addr, "--storage-bin-dir", binDir, "--data-dir", dataDir)
	cmd.Env = append(
		os.Environ(),
		"LO_META_DB_URL="+metaPath,
		"LO_SELF_MONITORING_ENABLED="+strconv.FormatBool(enabled),
	)
	cmd.Stdout, cmd.Stderr = &output, &output
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("self-monitoring=%t server exit: %v", enabled, err)
			}
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Errorf("self-monitoring=%t server did not stop", enabled)
		}
		if t.Failed() {
			t.Logf("self-monitoring=%t server output:\n%s", enabled, output.String())
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	waitFor(t, 30*time.Second, func() bool {
		resp, requestErr := client.Get(base + "/healthz")
		if requestErr != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	cookie := loginE2E(t, client, base, "admin", "self-monitoring-e2e-password")
	metricURL := base + "/api/v1/query?" + url.Values{
		"query": []string{`sum({__name__="http.server.request.duration_count"})`},
	}.Encode()
	var before float64
	if enabled {
		waitFor(t, 20*time.Second, func() bool {
			value, ok := metricScalar(sessionGet(t, client, metricURL, cookie, meta.SelfMonitoringProjectSlug))
			before = value
			return ok
		})
	}
	if remaining := 70*time.Second - time.Since(startedAt); remaining > 0 {
		// Keep normal gateway traffic flowing during the observation window.
		deadline := time.Now().Add(remaining)
		for time.Now().Before(deadline) {
			resp, requestErr := client.Get(base + "/healthz")
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			resp.Body.Close()
			time.Sleep(time.Second)
		}
	}
	logsURL := base + "/api/v1/logs/query?" + url.Values{"query": []string{`_msg:="self-monitoring enabled"`}}.Encode()
	tracesURL := base + "/api/traces/search?" + url.Values{
		"service": []string{"lazy-observe"},
		"limit":   []string{"10"},
	}.Encode()
	if enabled {
		var after float64
		waitFor(t, 20*time.Second, func() bool {
			value, ok := metricScalar(sessionGet(t, client, metricURL, cookie, meta.SelfMonitoringProjectSlug))
			after = value
			return ok && after > before
		})
		if delta := after - before; delta > 180 {
			t.Fatalf(
				"self-monitoring request count grew non-linearly: before=%.0f after=%.0f delta=%.0f",
				before,
				after,
				delta,
			)
		} else {
			t.Logf(
				"self-monitoring recursion evidence: source traffic produced %.0f additional observed requests",
				delta,
			)
		}
		waitFor(t, 20*time.Second, func() bool {
			return bytes.Contains(
				sessionGet(t, client, logsURL, cookie, meta.SelfMonitoringProjectSlug),
				[]byte("self-monitoring enabled"),
			)
		})
		waitFor(t, 20*time.Second, func() bool {
			return bytes.Contains(
				sessionGet(t, client, tracesURL, cookie, meta.SelfMonitoringProjectSlug),
				[]byte("lazy-observe"),
			)
		})
		t.Log("self-monitoring enabled AC2: metrics, startup log, and gateway traces passed after >=70s")
		return
	}
	if body := sessionGet(
		t,
		client,
		metricURL,
		cookie,
		meta.SelfMonitoringProjectSlug,
	); bytes.Contains(
		body,
		[]byte(`"result":[{`),
	) {
		t.Fatalf("self metrics exist while disabled: %s", body)
	}
	if body := sessionGet(
		t,
		client,
		logsURL,
		cookie,
		meta.SelfMonitoringProjectSlug,
	); bytes.Contains(
		body,
		[]byte("self-monitoring enabled"),
	) {
		t.Fatalf("self logs exist while disabled: %s", body)
	}
	if body := sessionGet(
		t,
		client,
		tracesURL,
		cookie,
		meta.SelfMonitoringProjectSlug,
	); bytes.Contains(
		body,
		[]byte(`"traceID"`),
	) {
		t.Fatalf("self traces exist while disabled: %s", body)
	}
	t.Log("self-monitoring disabled AC2: no self metrics, logs, or traces after >=70s")
}

func startEmbeddedPostgresE2E(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// The downloaded distribution goes to LO_TEST_PG_CACHE when it is set so that repeated runs and
	// CI can reuse it; the supervisor, runtime and data stay in the per-run temporary directory.
	config, err := embeddedpg.NewConfig(embeddedpg.CachePath(filepath.Join(root, "cache")), root)
	if err != nil {
		t.Fatalf("prepare embedded postgres: %v", err)
	}
	postgres := embeddedpostgres.NewDatabase(config.Database("lazy_observe_e2e"))
	if err := postgres.Start(); err != nil {
		t.Fatalf("start embedded postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := postgres.Stop(); err != nil {
			t.Errorf("stop embedded postgres: %v", err)
		}
	})
	dbURL := postgres.ConnectionURL()
	if dbURL == "" || postgres.GetPort() == 0 {
		t.Fatalf("embedded postgres returned empty endpoint: url=%q port=%d", dbURL, postgres.GetPort())
	}
	return dbURL
}

type bearerTokenTransport struct {
	base    http.RoundTripper
	token   string
	project string
}

func (t bearerTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	if t.project != "" {
		clone.Header.Set("X-LO-Project", t.project)
	}
	return t.base.RoundTrip(clone)
}

func callMCPToolE2E(
	t *testing.T,
	ctx context.Context,
	session *sdkmcp.ClientSession,
	name string,
	arguments map[string]any,
) []byte {
	t.Helper()
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("MCP call %s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("MCP call %s returned tool error: %v", name, result.GetError())
	}
	if len(result.Content) == 0 {
		t.Fatalf("MCP call %s returned no content", name)
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("MCP call %s returned %T, want text", name, result.Content[0])
	}
	return []byte(text.Text)
}

// e2eSession carries the session and (when present) CSRF cookies from a login
// so helpers can attach both to subsequent session-authenticated requests.
type e2eSession struct {
	session *http.Cookie
	csrf    *http.Cookie
}

// cookies returns all cookies carried by the session.
func (s *e2eSession) cookies() []*http.Cookie {
	if s == nil {
		return nil
	}
	cookies := make([]*http.Cookie, 0, 2)
	if s.session != nil {
		cookies = append(cookies, s.session)
	}
	if s.csrf != nil {
		cookies = append(cookies, s.csrf)
	}
	return cookies
}

// csrfValue returns the CSRF token value, or "" when none is set.
func (s *e2eSession) csrfValue() string {
	if s == nil || s.csrf == nil {
		return ""
	}
	return s.csrf.Value
}

func addSessionCookies(req *http.Request, session *e2eSession) {
	for _, cookie := range session.cookies() {
		req.AddCookie(cookie)
	}
}

func loginE2E(t *testing.T, client *http.Client, base, username, password string) *e2eSession {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) == 0 {
		t.Fatalf("login %s: status=%d body=%s", username, resp.StatusCode, body)
	}
	session := &e2eSession{}
	for _, cookie := range resp.Cookies() {
		switch cookie.Name {
		case "lo_session":
			session.session = cookie
		case csrfCookieName:
			session.csrf = cookie
		}
	}
	if session.session == nil {
		t.Fatalf("login %s: no session cookie in response", username)
	}
	return session
}

func jsonAPI(
	t *testing.T,
	client *http.Client,
	method, target string,
	session *e2eSession,
	payload any,
	output any,
	want int,
) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, cookie := range session.cookies() {
		req.AddCookie(cookie)
	}
	if session != nil && session.csrfValue() != "" && method != http.MethodGet && method != http.MethodHead &&
		method != http.MethodOptions {
		req.Header.Set(csrfHeaderName, session.csrfValue())
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, target, resp.StatusCode, want, responseBody)
	}
	if output != nil && len(responseBody) != 0 {
		if err := json.Unmarshal(responseBody, output); err != nil {
			t.Fatalf("decode %s: %v body=%s", target, err, responseBody)
		}
	}
}

func jsonProjectAPI(
	t *testing.T,
	client *http.Client,
	method, target string,
	session *e2eSession,
	project string,
	payload, output any,
	want int,
) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LO-Project", project)
	for _, cookie := range session.cookies() {
		req.AddCookie(cookie)
	}
	if session != nil && session.csrfValue() != "" && method != http.MethodGet && method != http.MethodHead &&
		method != http.MethodOptions {
		req.Header.Set(csrfHeaderName, session.csrfValue())
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, target, resp.StatusCode, want, responseBody)
	}
	if output != nil && len(responseBody) != 0 {
		if err := json.Unmarshal(responseBody, output); err != nil {
			t.Fatalf("decode %s: %v body=%s", target, err, responseBody)
		}
	}
}

func postJSONTokenE2E(t *testing.T, client *http.Client, target, token string, payload any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s", target, resp.StatusCode, response)
	}
}

func waitNotification(t *testing.T, ch <-chan alertpkg.Notification, timeout time.Duration) alertpkg.Notification {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(timeout):
		t.Fatalf("notification timeout after %s", timeout)
		return alertpkg.Notification{}
	}
}

func metricPayload(name string, value float64) []byte {
	now := uint64(time.Now().UnixNano())
	req := &metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricsdata.ResourceMetrics{{
		Resource: &resourcepb.Resource{},
		ScopeMetrics: []*metricsdata.ScopeMetrics{{Metrics: []*metricsdata.Metric{{
			Name: name,
			Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{DataPoints: []*metricsdata.NumberDataPoint{{
				TimeUnixNano: now, Value: &metricsdata.NumberDataPoint_AsDouble{AsDouble: value},
			}}}},
		}}}},
	}}}
	b, _ := proto.Marshal(req)
	return b
}

func logsPayload(marker string) []byte {
	now := uint64(time.Now().UnixNano())
	req := &logspb.ExportLogsServiceRequest{ResourceLogs: []*logdata.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{
						Key: "service.name",
						Value: &commonpb.AnyValue{
							Value: &commonpb.AnyValue_StringValue{StringValue: "lazy-observe-e2e"},
						},
					},
				},
			},
			ScopeLogs: []*logdata.ScopeLogs{{LogRecords: []*logdata.LogRecord{{
				TimeUnixNano: now, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}},
			}}}},
		},
	}}
	b, _ := proto.Marshal(req)
	return b
}

func traceLogsPayload(traceID, marker string) []byte {
	decoded := make([]byte, 16)
	for i := 0; i < len(decoded); i++ {
		value, _ := strconv.ParseUint(traceID[i*2:i*2+2], 16, 8)
		decoded[i] = byte(value)
	}
	now := uint64(time.Now().UnixNano())
	req := &logspb.ExportLogsServiceRequest{ResourceLogs: []*logdata.ResourceLogs{
		{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{
						Key:   "service.name",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
					},
				},
			},
			ScopeLogs: []*logdata.ScopeLogs{{LogRecords: []*logdata.LogRecord{
				{
					TimeUnixNano: now,
					SeverityText: "WARN",
					TraceId:      decoded,
					Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}},
				},
			}}},
		},
	}}
	b, _ := proto.Marshal(req)
	return b
}

func tracePayload(traceID, service, operation string) []byte {
	start := time.Now().UnixNano()
	return fmt.Appendf(
		nil,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}}]},"scopeSpans":[{"scope":{"name":"lazy-observe-e2e"},"spans":[{"traceId":%q,"spanId":"0102030405060708","name":%q,"kind":1,"startTimeUnixNano":%q,"endTimeUnixNano":%q,"status":{}}]}]}]}`,
		service,
		traceID,
		operation,
		strconv.FormatInt(start, 10),
		strconv.FormatInt(start+int64(time.Millisecond), 10),
	)
}

func llmTracePayload(traceID, service, model string, inputTokens, outputTokens int64, at time.Time) []byte {
	start := at.UnixNano()
	return fmt.Appendf(
		nil,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}}]},"scopeSpans":[{"scope":{"name":"lazy-observe-e2e"},"spans":[{"traceId":%q,"spanId":"090a0b0c0d0e0f10","name":"chat", "kind":3,"startTimeUnixNano":%q,"endTimeUnixNano":%q,"attributes":[{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.system","value":{"stringValue":"openai"}},{"key":"gen_ai.request.model","value":{"stringValue":%q}},{"key":"gen_ai.response.model","value":{"stringValue":%q}},{"key":"gen_ai.usage.input_tokens","value":{"intValue":%q}},{"key":"gen_ai.usage.output_tokens","value":{"intValue":%q}}],"status":{"code":1}}]}]}]}`,
		service,
		traceID,
		strconv.FormatInt(start, 10),
		strconv.FormatInt(start+int64(250*time.Millisecond), 10),
		model,
		model,
		strconv.FormatInt(inputTokens, 10),
		strconv.FormatInt(outputTokens, 10),
	)
}

func postProto(t *testing.T, client *http.Client, target, token string, payload []byte) {
	postData(t, client, target, token, "application/x-protobuf", payload)
}

func postData(t *testing.T, client *http.Client, target, token, contentType string, payload []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST %s: status=%d body=%s", target, resp.StatusCode, body)
	}
}

// e2eReader is a query credential: a user API token plus the selected project.
type e2eReader struct{ token, project string }

func authorizedGet(t *testing.T, client *http.Client, target string, reader e2eReader) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+reader.token)
	req.Header.Set("X-LO-Project", reader.project)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil
	}
	return body
}

func sessionGet(t *testing.T, client *http.Client, target string, session *e2eSession, project string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	for _, cookie := range session.cookies() {
		req.AddCookie(cookie)
	}
	req.Header.Set("X-LO-Project", project)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Logf("session GET %s: status=%d body=%s", target, resp.StatusCode, body)
		return nil
	}
	return body
}

func metricValuesAre(body []byte, expected string) bool {
	var response struct {
		Data struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Data.Result) == 0 {
		return false
	}
	for _, result := range response.Data.Result {
		if len(result.Value) != 2 || strings.Trim(string(result.Value[1]), `"`) != expected {
			return false
		}
	}
	return true
}

func metricScalar(body []byte) (float64, bool) {
	var response struct {
		Data struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Data.Result) != 1 ||
		len(response.Data.Result[0].Value) != 2 {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.Trim(string(response.Data.Result[0].Value[1]), `"`), 64)
	return value, err == nil
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
