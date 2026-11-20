package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
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

// TestMCPRealGatewayAll14ToolsContract verifies every one of the 14 MCP tools
// end-to-end against real Gateway HTTP handlers, real metadata/blob/alert/replay/profile
// stores, tenant isolation headers/PromQL injection, redaction, and error contracts.
func TestMCPRealGatewayAll14ToolsContract(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project, err := store.CreateProject(ctx, "Checkout", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := store.CreateProject(ctx, "Billing", "billing")
	if err != nil {
		t.Fatal(err)
	}
	ingestToken, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherIngestToken, err := store.CreateProjectAPIToken(ctx, otherProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(ctx, "mcp-full-reader", "password-1234", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	_, userToken, err := store.CreateUserAPIToken(ctx, user.ID, "mcp-all-14")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alertSvc := alert.NewService(alert.Config{Store: store, GroupWait: time.Millisecond})
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// Seed a real firing alert event on project "checkout" and another on "billing".
	enabled := true
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug,
		Name:        "HighLatency",
		Signal:      "metrics",
		Expression:  "up == 0",
		Severity:    "critical",
		Enabled:     &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAlertEvent(ctx, meta.AlertEventCreate{
		RuleID:      rule.ID,
		Fingerprint: "alert-fp-checkout",
		Status:      "firing",
		Labels:      map[string]string{"project_id": project.Slug, "alertname": "HighLatency"},
		Annotations: map[string]string{"summary": "checkout latency spike"},
		StartsAt:    now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	otherRule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: otherProject.Slug,
		Name:        "BillingDown",
		Signal:      "metrics",
		Expression:  "up == 0",
		Severity:    "warning",
		Enabled:     &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAlertEvent(ctx, meta.AlertEventCreate{
		RuleID:      otherRule.ID,
		Fingerprint: "alert-fp-billing",
		Status:      "firing",
		Labels:      map[string]string{"project_id": otherProject.Slug, "alertname": "BillingDown"},
		Annotations: map[string]string{"summary": "billing secret alert"},
		StartsAt:    now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	const validTraceID = "0123456789abcdef0123456789abcdef"
	var errBackend errorStorage
	var mu sync.Mutex
	var upstreamPaths []string
	storageBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/query":
			if got := r.URL.Query().Get("extra_filters[]"); got != `{project_id="checkout"}` {
				http.Error(w, "missing project isolation filter", http.StatusBadRequest)
				return
			}
			expr := r.URL.Query().Get("query")
			switch {
			case strings.Contains(expr, "node_uname_info"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "success",
					"data": map[string]any{
						"resultType": "vector",
						"result": []any{
							map[string]any{
								"metric": map[string]string{
									"instance": "node-1:9100",
									"nodename": "checkout-worker-1",
								},
								"value": []any{float64(now.Unix()), "1"},
							},
						},
					},
				})
			case strings.Contains(expr, "up{"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "success",
					"data": map[string]any{
						"resultType": "vector",
						"result": []any{
							map[string]any{
								"metric": map[string]string{"instance": "node-1:9100"},
								"value":  []any{float64(now.Unix()), "1"},
							},
						},
					},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "success",
					"data": map[string]any{
						"resultType": "vector",
						"result": []any{
							map[string]any{
								"metric": map[string]string{"__name__": "up", "project_id": "checkout"},
								"value":  []any{float64(now.Unix()), "1"},
							},
						},
					},
				})
			}
		case "/api/v1/query_range":
			if got := r.URL.Query().Get("extra_filters[]"); got != `{project_id="checkout"}` {
				http.Error(w, "missing project isolation filter", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{
					"resultType": "matrix",
					"result": []any{
						map[string]any{
							"metric": map[string]string{"__name__": "up", "project_id": "checkout"},
							"values": [][]any{
								{float64(now.Add(-time.Minute).Unix()), "1"},
								{float64(now.Unix()), "1"},
							},
						},
					},
				},
			})
		case "/select/logsql/query":
			if strings.Contains(r.URL.Query().Get("query"), `event_type="error"`) {
				errBackend.ServeHTTP(w, r)
				return
			}
			if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != strconv.Itoa(project.ID) {
				http.Error(w, "missing logs tenant isolation", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(
				w,
				`{"_time":"2026-07-17T12:00:00Z","level":"error","service_name":"checkout","message":"card declined"}`+"\n",
			)
		case "/select/logsql/hits":
			if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != strconv.Itoa(project.ID) {
				http.Error(w, "missing logs hits tenant isolation", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hits": []any{
					map[string]any{
						"timestamps": []any{"2026-07-17T11:55:00Z", "2026-07-17T12:00:00Z"},
						"values":     []any{3, 7},
					},
				},
			})
		case "/select/jaeger/api/services":
			if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != strconv.Itoa(project.ID) {
				http.Error(w, "missing traces tenant isolation", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{"checkout-api", "payment-svc"}})
		case "/select/jaeger/api/traces":
			if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != strconv.Itoa(project.ID) {
				http.Error(w, "missing trace search tenant isolation", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{
						"traceID": validTraceID,
						"processes": map[string]any{
							"p1": map[string]any{"serviceName": "checkout-api"},
						},
						"spans": []any{
							map[string]any{
								"spanID":        "root-1",
								"operationName": "POST /checkout",
								"processID":     "p1",
								"startTime":     now.Add(-time.Second).UnixMicro(),
								"duration":      45000,
								"tags":          []any{map[string]any{"key": "http.status_code", "value": 500}},
							},
						},
					},
				},
			})
		case "/select/jaeger/api/traces/" + validTraceID:
			if r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != strconv.Itoa(project.ID) {
				http.Error(w, "missing trace detail tenant isolation", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{
					map[string]any{
						"traceID": validTraceID,
						"processes": map[string]any{
							"p1": map[string]any{"serviceName": "checkout-api"},
						},
						"spans": []any{
							map[string]any{
								"spanID":        "root-1",
								"operationName": "POST /checkout",
								"processID":     "p1",
								"startTime":     now.Add(-time.Second).UnixMicro(),
								"duration":      45000,
							},
							map[string]any{
								"spanID":        "child-1",
								"operationName": "SELECT orders",
								"processID":     "p1",
								"startTime":     now.Add(-900 * time.Millisecond).UnixMicro(),
								"duration":      20000,
								"references":    []any{map[string]any{"refType": "CHILD_OF", "spanID": "root-1"}},
								"tags":          []any{map[string]any{"key": "error", "value": true}},
							},
						},
					},
				},
			})
		default:
			errBackend.ServeHTTP(w, r)
		}
	}))
	defer storageBackend.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + ln.Addr().String()

	gw := newMetadataTestGateway(store, Config{
		Tokens:     store,
		Store:      store,
		Alerts:     alertSvc,
		Replays:    store,
		Blobs:      blobs,
		Profiles:   store,
		Logs:       storageBackend.URL,
		Metrics:    storageBackend.URL,
		Traces:     storageBackend.URL,
		MCPBaseURL: baseURL,
		Now:        func() time.Time { return now },
	})
	go func() { _ = gw.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = gw.Drain(shutdownCtx)
	})

	// Seed real Issue on checkout (and one on billing to verify project isolation).
	postErrorsTest(t, gw.App(), ingestToken, []any{
		map[string]any{
			"event_id":  "ev-all-1",
			"timestamp": now.Format(time.RFC3339Nano),
			"platform":  "javascript",
			"release":   "v3.0.0",
			"level":     "error",
			"message":   "checkout failed",
			"exception": map[string]any{
				"type":  "CheckoutError",
				"value": "declined",
				"stacktrace": map[string]any{
					"frames": []any{
						map[string]any{
							"function": "chargeCard", "filename": "pay.ts", "lineno": 42,
							"in_app": true, "symbolicated": true,
						},
					},
				},
			},
		},
	}, http.StatusOK)
	postErrorsTest(t, gw.App(), otherIngestToken, []any{
		map[string]any{
			"event_id":  "ev-billing-1",
			"timestamp": now.Format(time.RFC3339Nano),
			"platform":  "go",
			"level":     "error",
			"message":   "billing isolated issue",
		},
	}, http.StatusOK)

	// Seed real Replay session via POST /api/replay/segments with sensitive URL query & marker.
	replayEvents := []byte(fmt.Sprintf(`[
		{"type":4,"data":{"href":"https://alice:secretpass@shop.example/checkout?token=topsecret#step2"},"timestamp":%d},
		{"type":5,"data":{"tag":"lazy-observe:http","payload":{"timestamp":%d,"method":"POST","path":"https://shop.example/api/pay?secret=leak","status":502,"duration_ms":120.5}},"timestamp":%d}
	]`,
		now.Add(-30*time.Second).UnixMilli(),
		now.Add(-5*time.Second).UnixMilli(),
		now.Add(-5*time.Second).UnixMilli(),
	))
	uploadReplay(t, gw.App(), ingestToken, "sess-checkout-1", 0, replayEvents, true, http.StatusAccepted, false)

	// Seed real Profile via POST /api/profiles for service "checkout-profiler".
	profBytes := writeProfile(t, syntheticProfile(15, "checkout.hotPath", "main.main"))
	profResp := uploadProfileFixture(
		t,
		gw.App(),
		ingestToken,
		"checkout-profiler",
		"cpu",
		profBytes,
		http.StatusCreated,
	)
	_ = profResp.Body.Close()

	// Auth contract: project ingest token MUST be rejected on /mcp with 403 ingest_only_token.
	ingestReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/mcp", strings.NewReader(`{}`))
	ingestReq.Header.Set("Authorization", "Bearer "+ingestToken)
	ingestReq.Header.Set("X-LO-Project", "checkout")
	ingestResp, err := http.DefaultClient.Do(ingestReq)
	if err != nil {
		t.Fatal(err)
	}
	ingestBody, _ := io.ReadAll(ingestResp.Body)
	_ = ingestResp.Body.Close()
	if ingestResp.StatusCode != http.StatusForbidden || !strings.Contains(string(ingestBody), "ingest_only_token") {
		t.Fatalf("ingest token on /mcp status=%d body=%s", ingestResp.StatusCode, ingestBody)
	}

	// Connect real MCP client with user API token scoped to project "checkout".
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: mcpBearerTransport{
			base:    http.DefaultTransport,
			token:   userToken,
			project: "checkout",
		},
	}
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "all-14-contract", Version: "1"}, nil).Connect(
		ctx,
		&sdkmcp.StreamableClientTransport{
			Endpoint:             baseURL + "/mcp",
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("connect /mcp: %v", err)
	}
	defer session.Close()

	toolsResp, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolsResp.Tools) != 14 {
		t.Fatalf("discovered %d MCP tools, want 14", len(toolsResp.Tools))
	}

	called := map[string]bool{}
	callOK := func(name string, args map[string]any) (map[string]any, string) {
		t.Helper()
		called[name] = true
		res, callErr := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		if callErr != nil || res == nil || res.IsError || len(res.Content) != 1 {
			t.Fatalf("tool %s failed: res=%#v err=%v", name, res, callErr)
		}
		text := res.Content[0].(*sdkmcp.TextContent).Text
		var decoded map[string]any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatalf("tool %s returned invalid JSON: %v\n%s", name, err, text)
		}
		return decoded, text
	}
	callErr := func(name string, args map[string]any) {
		t.Helper()
		res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("tool %s(%v) unexpectedly succeeded: %#v", name, args, res)
		}
	}

	// 1. query_metrics
	mInstant, _ := callOK("query_metrics", map[string]any{"query": "up"})
	if mInstant["truncated"] != false || mInstant["status"] != "success" {
		t.Fatalf("query_metrics=%#v", mInstant)
	}

	// 2. query_metrics_range
	mRange, _ := callOK("query_metrics_range", map[string]any{
		"query": "up",
		"start": now.Add(-5 * time.Minute).Format(time.RFC3339),
		"end":   now.Format(time.RFC3339),
		"step":  "60s",
	})
	if mRange["truncated"] != false || mRange["status"] != "success" {
		t.Fatalf("query_metrics_range=%#v", mRange)
	}

	// 3. query_logs
	logsOut, _ := callOK("query_logs", map[string]any{"query": `level:="error"`, "limit": 10})
	if logsOut["count"] != float64(1) || logsOut["truncated"] != false {
		t.Fatalf("query_logs=%#v", logsOut)
	}

	// 4. logs_histogram
	histOut, _ := callOK("logs_histogram", map[string]any{
		"query": "*",
		"start": now.Add(-10 * time.Minute).Format(time.RFC3339),
		"end":   now.Format(time.RFC3339),
		"step":  "5m",
	})
	if histOut["bucket_count"] != float64(2) || histOut["truncated"] != false {
		t.Fatalf("logs_histogram=%#v", histOut)
	}

	// 5. search_traces
	traceSearch, traceSearchText := callOK("search_traces", map[string]any{
		"service": "checkout-api",
		"start":   now.Add(-time.Minute).UnixMicro(),
		"end":     now.UnixMicro(),
		"limit":   10,
	})
	if traceSearch["count"] != float64(1) || strings.Contains(traceSearchText, `"spans"`) {
		t.Fatalf("search_traces=%s", traceSearchText)
	}

	// 6. get_trace (+ error contract for invalid trace_id)
	callErr("get_trace", map[string]any{"trace_id": "../v1/issues"})
	traceDetail, _ := callOK("get_trace", map[string]any{"trace_id": validTraceID})
	if traceDetail["trace_id"] != validTraceID || traceDetail["span_count"] != float64(2) {
		t.Fatalf("get_trace=%#v", traceDetail)
	}

	// 7. list_issues (redaction + cross-project isolation)
	issuesOut, issuesText := callOK("list_issues", map[string]any{})
	if strings.Contains(issuesText, `"fingerprint"`) || strings.Contains(issuesText, `"project_id"`) ||
		strings.Contains(issuesText, "billing isolated issue") {
		t.Fatalf("list_issues leaked internal/cross-project fields: %s", issuesText)
	}
	issuesList := issuesOut["issues"].([]any)
	if len(issuesList) != 1 {
		t.Fatalf("list_issues count=%d body=%s", len(issuesList), issuesText)
	}
	issueID := int(issuesList[0].(map[string]any)["id"].(float64))

	// 8. get_issue (+ error contract for non-existent & non-positive IDs)
	callErr("get_issue", map[string]any{"issue_id": 0})
	callErr("get_issue", map[string]any{"issue_id": 999999})
	issueDetail, issueDetailText := callOK("get_issue", map[string]any{"issue_id": issueID})
	if strings.Contains(issueDetailText, `"fingerprint"`) || strings.Contains(issueDetailText, `"project_id"`) {
		t.Fatalf("get_issue leaked internal fields: %s", issueDetailText)
	}
	if len(issueDetail["events"].([]any)) != 1 {
		t.Fatalf("get_issue=%s", issueDetailText)
	}

	// 9. list_replays (redaction of URL credentials & query params)
	replaysOut, replaysText := callOK("list_replays", map[string]any{"has_error": true})
	if replaysOut["count"] != float64(1) || !strings.Contains(replaysText, `"/checkout"`) ||
		strings.Contains(replaysText, "topsecret") || strings.Contains(replaysText, "secretpass") ||
		strings.Contains(replaysText, `"project_id"`) {
		t.Fatalf("list_replays unsafe or wrong: %s", replaysText)
	}

	// 10. get_replay_metadata (+ error contract for invalid & missing session_id)
	callErr("get_replay_metadata", map[string]any{"session_id": "../other"})
	callErr("get_replay_metadata", map[string]any{"session_id": "missing-session"})
	replayMeta, replayMetaText := callOK("get_replay_metadata", map[string]any{"session_id": "sess-checkout-1"})
	if replayMeta["returned_segment_count"] != float64(1) || replayMeta["marker_count"] != float64(1) ||
		strings.Contains(replayMetaText, "blob_key") || strings.Contains(replayMetaText, "secret=leak") ||
		strings.Contains(replayMetaText, "topsecret") {
		t.Fatalf("get_replay_metadata unsafe or wrong: %s", replayMetaText)
	}

	// 11. list_services (sorted union of traces + profiles)
	servicesOut, servicesText := callOK("list_services", map[string]any{})
	services := servicesOut["services"].([]any)
	if len(services) != 3 || services[0] != "checkout-api" || services[1] != "checkout-profiler" ||
		services[2] != "payment-svc" {
		t.Fatalf("list_services=%s", servicesText)
	}

	// 12. list_hosts
	hostsOut, hostsText := callOK("list_hosts", map[string]any{})
	if hostsOut["count"] != float64(1) || !strings.Contains(hostsText, "checkout-worker-1") {
		t.Fatalf("list_hosts=%s", hostsText)
	}

	// 13. list_alerts (cross-project isolated)
	alertsOut, alertsText := callOK("list_alerts", map[string]any{"status": "firing"})
	if alertsOut["count"] != float64(1) || !strings.Contains(alertsText, "HighLatency") ||
		strings.Contains(alertsText, "billing secret alert") {
		t.Fatalf("list_alerts=%s", alertsText)
	}

	// 14. get_flamegraph_summary
	flameOut, flameText := callOK("get_flamegraph_summary", map[string]any{
		"service": "checkout-profiler",
		"type":    "cpu",
		"start":   "2026-07-17T11:55:00Z",
		"end":     "2026-07-17T12:05:00Z",
		"top_n":   5,
	})
	if flameOut["total"] != float64(15) || !strings.Contains(flameText, "checkout.hotPath") {
		t.Fatalf("get_flamegraph_summary=%s", flameText)
	}

	// Verify all 14 discovered tools were actually exercised in this real-gateway contract suite.
	for _, tool := range toolsResp.Tools {
		if !called[tool.Name] {
			t.Fatalf("MCP tool %q was not exercised by real gateway contract test", tool.Name)
		}
	}
}
