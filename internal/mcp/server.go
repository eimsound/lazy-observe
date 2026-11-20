package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverVersion = "1.1.0"

type instantMetricsArgs struct {
	Query string `json:"query"          jsonschema:"Required PromQL expression. Project isolation is injected by lazy-observe; do not add or override project_id."`
	Time  string `json:"time,omitempty" jsonschema:"Optional evaluation time as RFC3339 or Unix timestamp; defaults to now."`
}

type rangeMetricsArgs struct {
	Query string `json:"query" jsonschema:"Required PromQL expression. Project isolation is injected by lazy-observe."`
	Start string `json:"start" jsonschema:"Required range start as RFC3339 or Unix timestamp."`
	End   string `json:"end"   jsonschema:"Required range end as RFC3339 or Unix timestamp."`
	Step  string `json:"step"  jsonschema:"Required Prometheus step such as 30s, 5m, or a number of seconds."`
}

type logsArgs struct {
	Query string `json:"query"           jsonschema:"Required LogsQL expression used to filter structured log records."`
	Start string `json:"start,omitempty" jsonschema:"Optional RFC3339 range start; omit to use the storage default."`
	End   string `json:"end,omitempty"   jsonschema:"Optional RFC3339 range end; omit to use now."`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum rows requested; hard-capped at 100."`
}

type histogramArgs struct {
	Query string `json:"query" jsonschema:"Required LogsQL expression whose matching volume should be bucketed."`
	Start string `json:"start" jsonschema:"Required RFC3339 range start."`
	End   string `json:"end"   jsonschema:"Required RFC3339 range end."`
	Step  string `json:"step"  jsonschema:"Required bucket width such as 30s or 5m."`
}

type searchTracesArgs struct {
	Service   string `json:"service"             jsonschema:"Required service name to search."`
	Operation string `json:"operation,omitempty" jsonschema:"Optional exact operation name within the service."`
	Start     int64  `json:"start"               jsonschema:"Required range start in epoch microseconds."`
	End       int64  `json:"end"                 jsonschema:"Required range end in epoch microseconds."`
	Limit     int    `json:"limit,omitempty"     jsonschema:"Maximum trace summaries requested; hard-capped at 100."`
}

type traceArgs struct {
	TraceID string `json:"trace_id" jsonschema:"Required Jaeger/OpenTelemetry trace ID."`
}

type listIssuesArgs struct {
	Status string `json:"status,omitempty" jsonschema:"Optional status: open, resolved, or ignored."`
	Level  string `json:"level,omitempty"  jsonschema:"Optional exact severity level, for example error or warning."`
	Query  string `json:"q,omitempty"      jsonschema:"Optional case-insensitive text search over issue titles."`
}

type issueArgs struct {
	IssueID int `json:"issue_id" jsonschema:"Required positive lazy-observe issue ID."`
}

type listReplaysArgs struct {
	HasError *bool  `json:"has_error,omitempty" jsonschema:"Optional filter for sessions with or without captured errors."`
	Start    string `json:"start,omitempty"     jsonschema:"Optional RFC3339 inclusive start time."`
	End      string `json:"end,omitempty"       jsonschema:"Optional RFC3339 inclusive end time."`
	Page     int    `json:"page,omitempty"      jsonschema:"One-based result page; defaults to 1. Each page contains at most 50 sessions."`
}

type replayArgs struct {
	SessionID string `json:"session_id" jsonschema:"Required Replay session ID returned by list_replays."`
}

type emptyArgs struct{}

type alertsArgs struct {
	Status string `json:"status,omitempty" jsonschema:"Optional status: firing or resolved. Omit to include both current firing and recent resolved alerts."`
}

type flamegraphArgs struct {
	Service string `json:"service"         jsonschema:"Required profiled service name."`
	Type    string `json:"type,omitempty"  jsonschema:"Profile type: cpu (default), heap, or goroutine."`
	Start   string `json:"start,omitempty" jsonschema:"Optional RFC3339 range start."`
	End     string `json:"end,omitempty"   jsonschema:"Optional RFC3339 range end."`
	TopN    int    `json:"top_n,omitempty" jsonschema:"Number of hottest functions to return; defaults to 20 and is capped at 50."`
}

func NewServer(api *APIClient) *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "lazy-observe", Version: serverVersion}, nil)
	registerMetricsTools(server, api)
	registerTraceTools(server, api)
	registerIssueTools(server, api)
	registerReplayTools(server, api)
	registerInventoryTools(server, api)
	registerProfileTools(server, api)
	return server
}

// registerMetricsTools adds the metrics and logs queries tools.
func registerMetricsTools(server *sdkmcp.Server, api *APIClient) {
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "query_metrics",
			Description: "Use for a PromQL instant query at one point in time, such as current error rate, saturation, or a single metric value. Returns the Prometheus result envelope plus truncation metadata; at most 500 series.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in instantMetricsArgs) (*sdkmcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Query) == "" {
				return nil, nil, fmt.Errorf("query is required")
			}
			q := url.Values{"query": {in.Query}}
			if in.Time != "" {
				q.Set("time", in.Time)
			}
			out, err := api.metrics(ctx, "/api/v1/query", q)
			return nil, out, err
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "query_metrics_range",
			Description: "Use for trends and changes over a time window with a PromQL range query. start/end select the window and step controls sample resolution. Returns a Prometheus matrix capped at 500 series and 5,000 total samples, with truncated=true when clipped.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in rangeMetricsArgs) (*sdkmcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Query) == "" || in.Start == "" || in.End == "" || in.Step == "" {
				return nil, nil, fmt.Errorf("query, start, end, and step are required")
			}
			out, err := api.metrics(
				ctx,
				"/api/v1/query_range",
				url.Values{"query": {in.Query}, "start": {in.Start}, "end": {in.End}, "step": {in.Step}},
			)
			return nil, out, err
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "query_logs",
			Description: "Use to inspect matching structured log records with LogsQL during an incident window. Returns parsed JSON rows, count, limit, and an explicit truncated flag. The server never returns more than 100 rows.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in logsArgs) (*sdkmcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Query) == "" {
				return nil, nil, fmt.Errorf("query is required")
			}
			q := url.Values{"query": {in.Query}}
			if in.Start != "" {
				q.Set("start", in.Start)
			}
			if in.End != "" {
				q.Set("end", in.End)
			}
			out, err := api.logs(ctx, q, in.Limit)
			return nil, out, err
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "logs_histogram",
			Description: "Use before fetching individual logs to see when matching LogsQL events spike. Returns aligned timestamps and counts, capped at 500 buckets with truncation metadata.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in histogramArgs) (*sdkmcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Query) == "" || in.Start == "" || in.End == "" || in.Step == "" {
				return nil, nil, fmt.Errorf("query, start, end, and step are required")
			}
			v, err := api.json(
				ctx,
				"/api/v1/logs/hits",
				url.Values{"query": {in.Query}, "start": {in.Start}, "end": {in.End}, "step": {in.Step}},
			)
			if err != nil {
				return nil, nil, err
			}
			return nil, compactHistogram(v, 500), nil
		},
	)
}

// registerTraceTools adds the trace search and detail tools.
func registerTraceTools(server *sdkmcp.Server, api *APIClient) {
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "search_traces",
			Description: "Use to find trace summaries for one service and optional operation in an epoch-microsecond time window. Returns compact trace-level summaries, not full spans, capped at 100 traces; call get_trace for details.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in searchTracesArgs) (*sdkmcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Service) == "" || in.Start <= 0 || in.End <= in.Start {
				return nil, nil, fmt.Errorf("service and a valid start/end window are required")
			}
			limit := in.Limit
			if limit <= 0 || limit > maxTraceSearch {
				limit = maxTraceSearch
			}
			q := url.Values{
				"service": {in.Service},
				"start":   {strconv.FormatInt(in.Start, 10)},
				"end":     {strconv.FormatInt(in.End, 10)},
				"limit":   {strconv.Itoa(limit + 1)},
			}
			if in.Operation != "" {
				q.Set("operation", in.Operation)
			}
			v, err := api.json(ctx, "/api/traces/search", q)
			if err != nil {
				return nil, nil, err
			}
			return nil, summarizeTraceSearch(v, limit), nil
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "get_trace",
			Description: "Use after search_traces when one trace needs inspection. Returns a token-efficient chronological span tree with name, service, duration, parent/depth, and error status. At most 200 spans are returned and truncation is explicit.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in traceArgs) (*sdkmcp.CallToolResult, any, error) {
			traceID := strings.TrimSpace(in.TraceID)
			if traceID == "" {
				return nil, nil, fmt.Errorf("trace_id is required")
			}
			if !validTraceID(traceID) {
				return nil, nil, fmt.Errorf("trace_id must be 1-32 hexadecimal characters")
			}
			v, err := api.json(ctx, "/api/traces/"+url.PathEscape(traceID), nil)
			if err != nil {
				return nil, nil, err
			}
			out, err := compactTrace(v)
			return nil, out, err
		},
	)
}

func validTraceID(traceID string) bool {
	if len(traceID) == 0 || len(traceID) > 32 {
		return false
	}
	for i := 0; i < len(traceID); i++ {
		ch := traceID[i]
		isDigit := ch >= '0' && ch <= '9'
		isLowerHex := ch >= 'a' && ch <= 'f'
		isUpperHex := ch >= 'A' && ch <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}
	return true
}

// registerIssueTools adds the grouped errors tools.
func registerIssueTools(server *sdkmcp.Server, api *APIClient) {
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "list_issues",
			Description: "Use to find grouped application errors by status, severity, or title text. Returns up to 50 issue summaries sorted by most recent occurrence, with project-internal fields removed.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in listIssuesArgs) (*sdkmcp.CallToolResult, any, error) {
			q := url.Values{}
			if in.Status != "" {
				q.Set("status", in.Status)
			}
			if in.Level != "" {
				q.Set("level", in.Level)
			}
			if in.Query != "" {
				q.Set("q", in.Query)
			}
			v, err := api.json(ctx, "/api/v1/issues", q)
			if err != nil {
				return nil, nil, err
			}
			truncated := false
			if root, _ := v.(map[string]any); root != nil && len(anySlice(root["issues"])) == 50 {
				nextQuery := cloneValues(q)
				nextQuery.Set("page", "2")
				next, nextErr := api.json(ctx, "/api/v1/issues", nextQuery)
				if nextErr != nil {
					return nil, nil, nextErr
				}
				if nextRoot, _ := next.(map[string]any); nextRoot != nil && len(anySlice(nextRoot["issues"])) > 0 {
					truncated = true
				}
			}
			return nil, sanitizeIssues(v, truncated), nil
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "get_issue",
			Description: "Use after list_issues for one grouped error. Returns issue metadata plus compact summaries of recent symbolicated events and stack frames, capped at 5 events and 30 frames each.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in issueArgs) (*sdkmcp.CallToolResult, any, error) {
			if in.IssueID <= 0 {
				return nil, nil, fmt.Errorf("issue_id must be positive")
			}
			v, err := api.json(ctx, "/api/v1/issues/"+strconv.Itoa(in.IssueID), nil)
			if err != nil {
				return nil, nil, err
			}
			return nil, enrichAndSummarizeIssue(ctx, api, in.IssueID, v), nil
		},
	)
}

// registerReplayTools adds the Session Replay metadata tools.
func registerReplayTools(server *sdkmcp.Server, api *APIClient) {
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "list_replays",
			Description: "Use to find project-scoped Session Replay metadata around an incident. Returns at most 50 sessions per page with timing, size, release, error state, and a query-free page path; raw rrweb events are never exposed.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in listReplaysArgs) (*sdkmcp.CallToolResult, any, error) {
			q := url.Values{}
			if in.HasError != nil {
				q.Set("has_error", strconv.FormatBool(*in.HasError))
			}
			if in.Start != "" {
				q.Set("start", in.Start)
			}
			if in.End != "" {
				q.Set("end", in.End)
			}
			if in.Page < 0 {
				return nil, nil, fmt.Errorf("page must be positive")
			}
			if in.Page > 0 {
				q.Set("page", strconv.Itoa(in.Page))
			}
			v, err := api.json(ctx, "/api/replay/sessions", q)
			if err != nil {
				return nil, nil, err
			}
			return nil, summarizeReplayList(v), nil
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "get_replay_metadata",
			Description: "Use after list_replays for one session's segment and timeline-marker metadata. Returns bounded error, HTTP, and Web Vital markers without request bodies, response bodies, URL queries, credentials, or raw rrweb events.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in replayArgs) (*sdkmcp.CallToolResult, any, error) {
			sessionID := strings.TrimSpace(in.SessionID)
			if !validReplayID(sessionID) {
				return nil, nil, fmt.Errorf("session_id must contain only letters, digits, dot, underscore, or hyphen")
			}
			v, err := api.json(ctx, "/api/replay/sessions/"+url.PathEscape(sessionID), nil)
			if err != nil {
				return nil, nil, err
			}
			return nil, summarizeReplayDetail(v), nil
		},
	)
}

// registerInventoryTools adds the services, hosts, and alerts tools.
func registerInventoryTools(server *sdkmcp.Server, api *APIClient) {
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "list_services",
			Description: "Use to discover valid service names before trace or profile queries. Returns the sorted union of services currently present in traces and profiles, capped at 200 names with truncation metadata.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ emptyArgs) (*sdkmcp.CallToolResult, any, error) {
			traces, err := api.json(ctx, "/api/traces/services", nil)
			if err != nil {
				return nil, nil, err
			}
			profiles, err := api.json(ctx, "/api/profiles/services", nil)
			if err != nil && !strings.Contains(err.Error(), "503") {
				return nil, nil, err
			}
			return nil, unionServices(traces, profiles, 200), nil
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "list_hosts",
			Description: "Use for a compact inventory and current health/resource snapshot of monitored hosts. Returns instance, hostname, up/down state, CPU cores, load, memory, and peak disk usage, capped at 200 hosts.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ emptyArgs) (*sdkmcp.CallToolResult, any, error) {
			v, err := api.json(ctx, "/api/v1/hosts", nil)
			if err != nil {
				return nil, nil, err
			}
			return nil, capArray(v, "hosts", 200), nil
		},
	)
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "list_alerts",
			Description: "Use to inspect currently firing alerts and recently resolved alert events. Optionally filter status to firing or resolved. Returns at most 100 events and states whether results were truncated.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in alertsArgs) (*sdkmcp.CallToolResult, any, error) {
			q := url.Values{}
			if in.Status != "" {
				q.Set("status", in.Status)
			}
			v, err := api.json(ctx, "/api/v1/alerts", q)
			if err != nil {
				return nil, nil, err
			}
			return nil, capArray(v, "alerts", 100), nil
		},
	)
}

// registerProfileTools adds the continuous profiling tools.
func registerProfileTools(server *sdkmcp.Server, api *APIClient) {
	sdkmcp.AddTool(
		server,
		&sdkmcp.Tool{
			Name:        "get_flamegraph_summary",
			Description: "Use to identify CPU, heap, or goroutine hotspots for a service in a time window without returning a huge flame tree. Returns a textual top-N inclusive-hotspot summary and structured values; top_n is capped at 50.",
		},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in flamegraphArgs) (*sdkmcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Service) == "" {
				return nil, nil, fmt.Errorf("service is required")
			}
			q := url.Values{"service": {in.Service}}
			if in.Type != "" {
				q.Set("type", in.Type)
			}
			if in.Start != "" {
				q.Set("start", in.Start)
			}
			if in.End != "" {
				q.Set("end", in.End)
			}
			v, err := api.json(ctx, "/api/profiles/flamegraph", q)
			if err != nil {
				return nil, nil, err
			}
			top := in.TopN
			if top <= 0 {
				top = 20
			}
			if top > 50 {
				top = 50
			}
			return nil, flamegraphSummary(v, top), nil
		},
	)
}

func NewHTTPHandler(baseURL string, client *http.Client) http.Handler {
	if client == nil {
		client = defaultHTTPClient()
	}
	return sdkmcp.NewStreamableHTTPHandler(func(req *http.Request) *sdkmcp.Server {
		// The gateway mounts this handler through fasthttpadaptor, whose
		// net/http header values borrow fasthttp's reusable request buffer even
		// when Fiber runs Immutable. The API client keeps the credential and
		// project for every tool call, so it must own copies of them.
		authorization := strings.Clone(strings.TrimSpace(req.Header.Get("Authorization")))
		scheme, token, _ := strings.Cut(authorization, " ")
		if !strings.EqualFold(scheme, "Bearer") {
			token = ""
		}
		api, err := NewAPIClient(baseURL, strings.TrimSpace(token), client)
		if err != nil {
			// Authentication middleware rejects missing credentials before this.
			api = &APIClient{BaseURL: baseURL, Token: "invalid", HTTP: client}
		}
		api.Project = strings.Clone(strings.TrimSpace(req.Header.Get("X-LO-Project")))
		return NewServer(api)
	}, &sdkmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

func RunStdio(ctx context.Context, api *APIClient) error {
	return NewServer(api).Run(ctx, &sdkmcp.StdioTransport{})
}

func compactHistogram(v any, limit int) map[string]any {
	root, _ := v.(map[string]any)
	var timestamps, values []any
	if root != nil {
		timestamps, _ = root["timestamps"].([]any)
		values, _ = root["values"].([]any)
	}
	if len(timestamps) == 0 && root != nil {
		if hits, _ := root["hits"].([]any); len(hits) > 0 {
			first, _ := hits[0].(map[string]any)
			timestamps, _ = first["timestamps"].([]any)
			values = make([]any, len(timestamps))
			for i := range values {
				var sum float64
				for _, raw := range hits {
					h, _ := raw.(map[string]any)
					vs, _ := h["values"].([]any)
					if i < len(vs) {
						sum += numberValue(vs[i])
					}
				}
				values[i] = sum
			}
		}
	}
	original := min(len(timestamps), len(values))
	truncated := original > limit
	if truncated {
		timestamps, values = timestamps[:limit], values[:limit]
	}
	return map[string]any{
		"timestamps":   timestamps,
		"values":       values,
		"bucket_count": min(original, limit),
		"truncated":    truncated,
		"limit":        limit,
	}
}

func summarizeTraceSearch(v any, limit int) map[string]any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{"traces": []any{}, "count": 0, "truncated": false, "limit": limit}
	}
	data, _ := root["data"].([]any)
	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}
	results := make([]any, 0, len(data))
	for _, raw := range data {
		trace, _ := raw.(map[string]any)
		if trace == nil {
			continue
		}
		spans, _ := trace["spans"].([]any)
		processes, _ := trace["processes"].(map[string]any)
		var start, end int64
		errorsCount := 0
		services := map[string]bool{}
		rootName, rootService := "", ""
		ids, children := map[string]bool{}, map[string]bool{}
		for _, rs := range spans {
			s, _ := rs.(map[string]any)
			ids[stringValue(s["spanID"])] = true
		}
		for _, rs := range spans {
			s, _ := rs.(map[string]any)
			st, dur := int64(numberValue(s["startTime"])), int64(numberValue(s["duration"]))
			if start == 0 || st < start {
				start = st
			}
			if st+dur > end {
				end = st + dur
			}
			if spanHasError(s) {
				errorsCount++
			}
			pid := stringValue(s["processID"])
			if p, _ := processes[pid].(map[string]any); p != nil {
				services[stringValue(p["serviceName"])] = true
			}
			for _, rr := range anySlice(s["references"]) {
				r, _ := rr.(map[string]any)
				if r["refType"] == "CHILD_OF" && ids[stringValue(r["spanID"])] {
					children[stringValue(s["spanID"])] = true
				}
			}
		}
		for _, rs := range spans {
			s, _ := rs.(map[string]any)
			if !children[stringValue(s["spanID"])] {
				rootName = stringValue(s["operationName"])
				if p, _ := processes[stringValue(s["processID"])].(map[string]any); p != nil {
					rootService = stringValue(p["serviceName"])
				}
				break
			}
		}
		results = append(
			results,
			map[string]any{
				"trace_id":      trace["traceID"],
				"root_name":     rootName,
				"root_service":  rootService,
				"span_count":    len(spans),
				"service_count": len(services),
				"error_count":   errorsCount,
				"start_time_us": start,
				"duration_us":   end - start,
			},
		)
	}
	return map[string]any{"traces": results, "count": len(results), "truncated": truncated, "limit": limit}
}

func sanitizeIssues(v any, truncated bool) any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{
			"issues":    []any{},
			"count":     0,
			"truncated": truncated,
			"limit":     50,
		}
	}
	issues, _ := root["issues"].([]any)
	if issues == nil {
		issues = []any{}
	}
	for _, raw := range issues {
		if issue, _ := raw.(map[string]any); issue != nil {
			delete(issue, "project_id")
			delete(issue, "fingerprint")
		}
	}
	root["issues"] = issues
	root["count"] = len(issues)
	root["truncated"] = truncated
	root["limit"] = 50
	return root
}

func enrichAndSummarizeIssue(ctx context.Context, api *APIClient, issueID int, v any) any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{
			"events":           []any{},
			"events_truncated": false,
			"event_limit":      5,
		}
	}
	var fetcher func(eventID string) map[string]any
	if api != nil && issueID > 0 {
		fetcher = func(eventID string) map[string]any {
			path := "/api/v1/issues/" + strconv.Itoa(issueID) + "/events/" + url.PathEscape(eventID)
			raw, err := api.json(ctx, path, nil)
			if err != nil {
				return nil
			}
			detail, _ := raw.(map[string]any)
			return detail
		}
	}
	return summarizeIssueWithFetcher(root, fetcher)
}

func summarizeIssue(v any) any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{
			"events":           []any{},
			"events_truncated": false,
			"event_limit":      5,
		}
	}
	return summarizeIssueWithFetcher(root, nil)
}

func summarizeIssueWithFetcher(root map[string]any, fetchEvent func(eventID string) map[string]any) map[string]any {
	delete(root, "project_id")
	delete(root, "fingerprint")
	issueLevel := stringValue(root["level"])
	events, _ := root["events"].([]any)
	truncated := len(events) > 5
	if truncated {
		events = events[:5]
	}
	summaries := make([]any, 0, len(events))
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		if event == nil {
			continue
		}
		detail := extractIssueEventDetail(event)
		if detail == nil && fetchEvent != nil {
			if eventID := stringValue(event["event_id"]); eventID != "" {
				detail = fetchEvent(eventID)
			}
		}
		summaries = append(summaries, buildIssueEventSummary(event, detail, issueLevel))
	}
	root["events"] = summaries
	root["events_truncated"] = truncated
	root["event_limit"] = 5
	return root
}

func extractIssueEventDetail(event map[string]any) map[string]any {
	if encoded := stringValue(event["symbolicated_event"]); encoded != "" {
		var detail map[string]any
		if json.Unmarshal([]byte(encoded), &detail) == nil && detail != nil {
			return detail
		}
	}
	if _, hasException := event["exception"]; hasException {
		return event
	}
	if _, hasMessage := event["message"]; hasMessage {
		return event
	}
	return nil
}

func buildIssueEventSummary(event, detail map[string]any, issueLevel string) map[string]any {
	summary := map[string]any{
		"event_id":  event["event_id"],
		"timestamp": event["timestamp"],
	}
	if traceID := stringValue(event["trace_id"]); traceID != "" {
		summary["trace_id"] = traceID
	}
	if sessionID := stringValue(event["session_id"]); sessionID != "" {
		summary["session_id"] = sessionID
	}
	level := stringValue(event["level"])
	if detail != nil {
		if dl := stringValue(detail["level"]); dl != "" {
			level = dl
		}
		if msg := stringValue(detail["message"]); msg != "" {
			summary["message"] = msg
		}
		if rel := stringValue(detail["release"]); rel != "" {
			summary["release"] = rel
		}
		if _, hasTrace := summary["trace_id"]; !hasTrace {
			if contexts, _ := detail["contexts"].(map[string]any); contexts != nil {
				if traceCtx, _ := contexts["trace"].(map[string]any); traceCtx != nil {
					if tid := stringValue(traceCtx["trace_id"]); tid != "" {
						summary["trace_id"] = tid
					}
				}
			}
		}
		if ex, _ := detail["exception"].(map[string]any); ex != nil {
			summary["exception_type"] = ex["type"]
			summary["exception_value"] = ex["value"]
			st, _ := ex["stacktrace"].(map[string]any)
			frames := anySlice(st["frames"])
			frameTruncated := len(frames) > 30
			if frameTruncated {
				frames = frames[len(frames)-30:]
			}
			compact := make([]any, 0, len(frames))
			for _, rf := range frames {
				f, _ := rf.(map[string]any)
				if f == nil {
					continue
				}
				frame := map[string]any{
					"function": f["function"],
					"filename": f["filename"],
					"line":     f["lineno"],
					"in_app":   f["in_app"],
				}
				if sym, ok := f["symbolicated"]; ok {
					frame["symbolicated"] = sym
				}
				compact = append(compact, frame)
			}
			summary["stack_frames"] = compact
			summary["stack_truncated"] = frameTruncated
		}
	}
	if level == "" {
		level = issueLevel
	}
	if level != "" {
		summary["level"] = level
	}
	return summary
}

func summarizeReplayList(v any) map[string]any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{
			"sessions":  []any{},
			"count":     0,
			"total":     0,
			"page":      1,
			"page_size": 50,
			"has_more":  false,
		}
	}
	rows := anySlice(root["sessions"])
	if len(rows) > 50 {
		rows = rows[:50]
	}
	sessions := make([]any, 0, len(rows))
	for _, row := range rows {
		if summary := replaySessionSummary(row); summary != nil {
			sessions = append(sessions, summary)
		}
	}
	page := max(1, int(numberValue(root["page"])))
	pageSize := int(numberValue(root["page_size"]))
	if pageSize <= 0 || pageSize > 50 {
		pageSize = 50
	}
	total := max(0, int(numberValue(root["total"])))
	return map[string]any{
		"sessions": sessions, "count": len(sessions), "total": total,
		"page": page, "page_size": pageSize, "has_more": page*pageSize < total,
	}
}

func summarizeReplayDetail(v any) map[string]any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{
			"segments":      []any{},
			"markers":       []any{},
			"segment_count": 0,
			"marker_count":  0,
			"truncated":     false,
		}
	}
	result := replaySessionSummary(root)
	if result == nil {
		result = map[string]any{}
	}

	rawSegments := anySlice(root["segments"])
	segmentsTruncated := len(rawSegments) > 200
	if segmentsTruncated {
		rawSegments = rawSegments[:200]
	}
	segments := make([]any, 0, len(rawSegments))
	for _, raw := range rawSegments {
		segment, _ := raw.(map[string]any)
		if segment == nil {
			continue
		}
		segments = append(segments, map[string]any{
			"seq": segment["seq"], "started_at": segment["started_at"],
			"event_count": segment["event_count"], "size_bytes": segment["size_bytes"],
		})
	}

	rawMarkers := anySlice(root["markers"])
	markersTruncated := len(rawMarkers) > 200
	if markersTruncated {
		rawMarkers = rawMarkers[:200]
	}
	markers := make([]any, 0, len(rawMarkers))
	for _, raw := range rawMarkers {
		if marker := replayMarkerSummary(raw); marker != nil {
			markers = append(markers, marker)
		}
	}
	result["segments"] = segments
	result["markers"] = markers
	result["returned_segment_count"] = len(segments)
	result["marker_count"] = len(markers)
	result["truncated"] = segmentsTruncated || markersTruncated
	result["limits"] = map[string]any{"segments": 200, "markers": 200}
	return result
}

func replaySessionSummary(raw any) map[string]any {
	row, _ := raw.(map[string]any)
	if row == nil || stringValue(row["session_id"]) == "" {
		return nil
	}
	return map[string]any{
		"session_id": stringValue(row["session_id"]),
		"started_at": row["started_at"], "last_seen_at": row["last_seen_at"],
		"segment_count": row["segment_count"], "total_bytes": row["total_bytes"],
		"has_error": row["has_error"], "release": truncateString(stringValue(row["release"]), 255),
		"page_path": replayPagePath(stringValue(row["page_url"])),
	}
}

func replayMarkerSummary(raw any) map[string]any {
	marker, _ := raw.(map[string]any)
	if marker == nil {
		return nil
	}
	typeName := stringValue(marker["type"])
	result := map[string]any{"type": typeName, "time": marker["time"]}
	switch typeName {
	case "error":
		result["issue_id"] = truncateString(stringValue(marker["issue_id"]), 255)
		result["event_id"] = truncateString(stringValue(marker["event_id"]), 255)
		result["title"] = truncateString(stringValue(marker["title"]), 512)
		result["level"] = stringValue(marker["level"])
	case "http":
		result["method"] = truncateString(stringValue(marker["method"]), 32)
		result["path"] = replayPagePath(stringValue(marker["path"]))
		result["status"] = marker["status"]
		result["duration_ms"] = marker["duration_ms"]
	case "vital":
		result["name"] = truncateString(stringValue(marker["name"]), 16)
		result["value"] = marker["value"]
		result["rating"] = truncateString(stringValue(marker["rating"]), 32)
	default:
		return nil
	}
	return result
}

func validReplayID(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func replayPagePath(raw string) string {
	value := strings.TrimSpace(raw)
	if parsed, err := url.Parse(value); err == nil {
		value = parsed.Path
	}
	if i := strings.IndexAny(value, "?#"); i >= 0 {
		value = value[:i]
	}
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return truncateString(value, 2048)
}

func truncateString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func unionServices(traces, profiles any, limit int) map[string]any {
	set := map[string]bool{}
	if root, _ := traces.(map[string]any); root != nil {
		for _, v := range anySlice(root["data"]) {
			if s := stringValue(v); s != "" {
				set[s] = true
			}
		}
	}
	if root, _ := profiles.(map[string]any); root != nil {
		for _, v := range anySlice(root["services"]) {
			if s := stringValue(v); s != "" {
				set[s] = true
			}
		}
	}
	names := make([]string, 0, len(set))
	for s := range set {
		names = append(names, s)
	}
	sort.Strings(names)
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	return map[string]any{"services": names, "count": len(names), "truncated": truncated, "limit": limit}
}

func capArray(v any, key string, limit int) map[string]any {
	rows, _ := v.([]any)
	if rows == nil {
		rows = []any{}
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return map[string]any{key: rows, "count": len(rows), "truncated": truncated, "limit": limit}
}

type hotspot struct {
	Name    string  `json:"name"`
	Value   int64   `json:"value"`
	Percent float64 `json:"percent"`
}

func flamegraphSummary(v any, top int) map[string]any {
	root, _ := v.(map[string]any)
	if root == nil {
		return map[string]any{
			"summary":          "",
			"hotspots":         []hotspot{},
			"total":            int64(0),
			"count":            0,
			"truncated":        false,
			"source_truncated": false,
			"limit":            top,
		}
	}
	total := int64(numberValue(root["value"]))
	values := map[string]int64{}
	sourceTruncated := false
	var walk func(any)
	walk = func(raw any) {
		n, _ := raw.(map[string]any)
		if n == nil {
			return
		}
		name := stringValue(n["name"])
		if name != "" && name != "root" {
			values[name] += int64(numberValue(n["value"]))
		}
		if b, _ := n["truncated"].(bool); b {
			sourceTruncated = true
		}
		for _, child := range anySlice(n["children"]) {
			walk(child)
		}
	}
	walk(root)
	rows := make([]hotspot, 0, len(values))
	for name, value := range values {
		pct := 0.0
		if total != 0 {
			pct = float64(value) * 100 / float64(total)
		}
		rows = append(rows, hotspot{Name: name, Value: value, Percent: pct})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Value == rows[j].Value {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Value > rows[j].Value
	})
	truncated := len(rows) > top
	if truncated {
		rows = rows[:top]
	}
	var text strings.Builder
	for i, h := range rows {
		fmt.Fprintf(&text, "%d. %s — %d (%.2f%%)\n", i+1, h.Name, h.Value, h.Percent)
	}
	return map[string]any{
		"summary":          strings.TrimSpace(text.String()),
		"hotspots":         rows,
		"total":            total,
		"count":            len(rows),
		"truncated":        truncated || sourceTruncated,
		"source_truncated": sourceTruncated,
		"limit":            top,
	}
}

func anySlice(v any) []any { s, _ := v.([]any); return s }

func cloneValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, items := range values {
		clone[key] = append([]string(nil), items...)
	}
	return clone
}
