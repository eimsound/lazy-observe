package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	maxQueryResponseBytes = 32 << 20
	defaultMinFixtureRows = 25
	defaultReadProject    = "default"
	projectHeader         = "X-LO-Project"
	userTokenPrefix       = "lou_"
)

type queryEndpointResult struct {
	Name       string         `json:"name"`
	Path       string         `json:"path"`
	Requests   int            `json:"requests"`
	Failures   int            `json:"failures"`
	Latency    latencySummary `json:"latency"`
	MaxP95MS   float64        `json:"max_p95_ms"`
	GatePassed bool           `json:"gate_passed"`
}

type queryGateResult struct {
	Version      int                   `json:"version"`
	GitCommit    string                `json:"git_commit,omitempty"`
	GeneratedAt  string                `json:"generated_at,omitempty"`
	BaseURL      string                `json:"base_url"`
	Warmup       int                   `json:"warmup_requests"`
	Fixture      queryFixtureResult    `json:"medium_fixture"`
	Endpoints    []queryEndpointResult `json:"endpoints"`
	GatePassed   bool                  `json:"gate_passed"`
	GateFailures []string              `json:"gate_failures,omitempty"`
}

type fixtureCorpus struct {
	Issues int `json:"issues"`
	Logs   int `json:"logs"`
	Traces int `json:"traces"`
}

type queryFixtureResult struct {
	Required fixtureCorpus `json:"required"`
	Observed fixtureCorpus `json:"observed"`
	Verified bool          `json:"verified"`
}

type queryTarget struct {
	name     string
	path     string
	validate func(http.Header, []byte) error
}

type issueFixture struct {
	ID          int     `json:"id"`
	Fingerprint string  `json:"fingerprint"`
	Title       string  `json:"title"`
	Level       string  `json:"level"`
	Status      string  `json:"status"`
	Count       int64   `json:"count"`
	UserCount   int64   `json:"user_count"`
	FirstSeen   int64   `json:"first_seen"`
	LastSeen    int64   `json:"last_seen"`
	Trend       []int64 `json:"trend"`
}

func runQueryGate(args []string) error {
	flags := flag.NewFlagSet("query", flag.ContinueOnError)
	configPath := flags.String("config", defaultGateConfigPath, "performance gate config")
	baseURL := flags.String("base-url", "", "live lazy-observe replica URL")
	tokenEnv := flags.String(
		"token-env",
		"LO_PERF_QUERY_TOKEN",
		"environment variable containing a user API token (lou_...) used for every read; project tokens are ingest-only",
	)
	project := flags.String("project", defaultReadProject, "project slug sent as "+projectHeader+" on every read")
	issueID := flags.Int("issue-id", 0, "expected positive issue ID from the controlled fixture")
	logMarker := flags.String("log-marker", "", "expected non-empty _msg value from the controlled fixture")
	traceID := flags.String("trace-id", "", "existing trace ID used for the trace detail query")
	traceService := flags.String("trace-service", "", "service name shared by the controlled medium trace corpus")
	logStart := flags.Int64("log-start", 0, "logs query start as Unix seconds (default: one hour before invocation)")
	logEnd := flags.Int64("log-end", 0, "logs query end as Unix seconds (default: one minute after invocation)")
	logLimit := flags.Int("log-limit", 100, "maximum log rows requested from the controlled fixture")
	minIssues := flags.Int(
		"min-issues",
		defaultMinFixtureRows,
		"minimum issue rows required from the controlled fixture",
	)
	minLogRows := flags.Int(
		"min-log-rows",
		defaultMinFixtureRows,
		"minimum matching log rows required from the controlled fixture",
	)
	output := flags.String("output", "", "optional JSON result path")
	warmupOverride := flags.Int("warmup", -1, "override warmup request count")
	requestsOverride := flags.Int("requests", -1, "override measured request count")
	timeout := flags.Duration("timeout", 10*time.Second, "per-request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadGateConfig(*configPath)
	if err != nil {
		return err
	}
	normalizedBase, err := validateBaseURL(*baseURL)
	if err != nil {
		return err
	}
	auth, err := loadReadCredential(*tokenEnv, *project)
	if err != nil {
		return err
	}
	if *issueID <= 0 {
		return fmt.Errorf("issue-id must identify the positive numeric ID created by the controlled fixture")
	}
	marker := *logMarker
	if strings.TrimSpace(marker) == "" {
		return fmt.Errorf("log-marker is required and must identify the controlled fixture")
	}
	trace := strings.TrimSpace(*traceID)
	if trace == "" {
		return fmt.Errorf("trace-id is required; the gate does not substitute a services-list query")
	}
	service := strings.TrimSpace(*traceService)
	if service == "" {
		return fmt.Errorf("trace-service is required to verify the medium trace corpus")
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	warmup, requests := cfg.Query.WarmupRequests, cfg.Query.MeasuredRequests
	if *warmupOverride >= 0 {
		warmup = *warmupOverride
	}
	if *requestsOverride >= 0 {
		requests = *requestsOverride
	}
	if warmup < 0 || requests <= 0 {
		return fmt.Errorf("warmup must be non-negative and requests must be positive")
	}
	now := time.Now()
	start, end := *logStart, *logEnd
	if start == 0 {
		start = now.Add(-time.Hour).Unix()
	}
	if end == 0 {
		end = now.Add(time.Minute).Unix()
	}
	if start <= 0 || end <= start {
		return fmt.Errorf("log-start and log-end must define a positive, increasing Unix-second range")
	}
	if *logLimit <= 0 || *logLimit > 1000 {
		return fmt.Errorf("log-limit must be between 1 and 1000")
	}
	if *minIssues <= 0 || *minIssues > 50 {
		return fmt.Errorf("min-issues must be between 1 and the 50-row issues page size")
	}
	if *minLogRows <= 0 || *minLogRows > *logLimit {
		return fmt.Errorf("min-log-rows must be between 1 and log-limit")
	}
	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	required := fixtureCorpus{
		Issues: cfg.Query.MediumFixture.Issues,
		Logs:   cfg.Query.MediumFixture.Logs,
		Traces: cfg.Query.MediumFixture.Traces,
	}
	fixture, err := verifyMediumFixture(
		context.Background(),
		client,
		normalizedBase,
		auth,
		*issueID,
		marker,
		service,
		trace,
		start,
		end,
		required,
	)
	if err != nil {
		return fmt.Errorf("verify medium query fixture: %w", err)
	}
	targets := buildQueryTargets(
		*issueID,
		marker,
		trace,
		start,
		end,
		*logLimit,
		*minIssues,
		*minLogRows,
		required.Issues,
	)
	gitCommit, generatedAt := resultMetadata()
	result := queryGateResult{
		Version:     2,
		GitCommit:   gitCommit,
		GeneratedAt: generatedAt,
		BaseURL:     normalizedBase,
		Warmup:      warmup,
		Fixture:     fixture,
		GatePassed:  true,
	}
	for _, target := range targets {
		endpoint, endpointErr := measureQueryEndpoint(
			context.Background(),
			client,
			normalizedBase,
			auth,
			target,
			warmup,
			requests,
			cfg.Query.MaxP95MS[target.name],
		)
		if endpointErr != nil {
			return fmt.Errorf("measure %s query: %w", target.name, endpointErr)
		}
		result.Endpoints = append(result.Endpoints, endpoint)
		if endpoint.Failures > 0 {
			result.GateFailures = append(
				result.GateFailures,
				fmt.Sprintf("%s query had %d failed requests", target.name, endpoint.Failures),
			)
		}
		if endpoint.Latency.P95MS >= endpoint.MaxP95MS {
			result.GateFailures = append(
				result.GateFailures,
				fmt.Sprintf(
					"%s query p95 %.3fms is not below %.3fms",
					target.name,
					endpoint.Latency.P95MS,
					endpoint.MaxP95MS,
				),
			)
		}
	}
	result.GatePassed = len(result.GateFailures) == 0
	if err := writeResult(*output, result); err != nil {
		return err
	}
	if !result.GatePassed {
		return failResult(strings.Join(result.GateFailures, "; "))
	}
	for _, endpoint := range result.Endpoints {
		fmt.Printf(
			"PASS: %s query p95 %.3fms (< %.3fms), %d measured requests\n",
			endpoint.Name,
			endpoint.Latency.P95MS,
			endpoint.MaxP95MS,
			endpoint.Requests,
		)
	}
	fmt.Printf(
		"PASS: medium fixture verified with %d Issues, %d logs, and %d traces\n",
		fixture.Observed.Issues,
		fixture.Observed.Logs,
		fixture.Observed.Traces,
	)
	return nil
}

// readCredential authenticates read/query requests. Project API tokens are
// ingest-only, so reads use a user API token plus an explicit project slug.
type readCredential struct {
	token   string
	project string
}

func (c readCredential) apply(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(projectHeader, c.project)
}

// loadReadCredential reads the user API token from tokenEnv and pairs it with
// the selected project slug. Project tokens are rejected up front because every
// read route answers them with 403 ingest_only_token.
func loadReadCredential(tokenEnv, project string) (readCredential, error) {
	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if token == "" {
		return readCredential{}, fmt.Errorf("user API token environment variable %s is empty", tokenEnv)
	}
	if !strings.HasPrefix(token, userTokenPrefix) {
		return readCredential{}, fmt.Errorf(
			"%s must hold a user API token (%s...); project tokens are ingest-only and cannot read",
			tokenEnv,
			userTokenPrefix,
		)
	}
	slug := strings.TrimSpace(project)
	if slug == "" || strings.ContainsAny(slug, " \t\r\n") {
		return readCredential{}, fmt.Errorf("project must be a non-empty project slug")
	}
	return readCredential{token: token, project: slug}, nil
}

func buildQueryTargets(
	issueID int,
	logMarker, traceID string,
	logStart, logEnd int64,
	logLimit, minIssues, minLogRows, minIssueTotal int,
) []queryTarget {
	logValues := url.Values{
		"query": {`_msg:=` + strconv.Quote(logMarker)},
		"start": {strconv.FormatInt(logStart, 10)},
		"end":   {strconv.FormatInt(logEnd, 10)},
		"limit": {strconv.Itoa(logLimit)},
	}
	return []queryTarget{
		{
			name:     "issues",
			path:     "/api/v1/issues?status=open&sort=last_seen&page=1",
			validate: validateIssueListResponse(issueID, minIssues, minIssueTotal),
		},
		{
			name:     "logs",
			path:     "/api/v1/logs/query?" + logValues.Encode(),
			validate: validateLogsResponse(logMarker, minLogRows),
		},
		{
			name:     "trace",
			path:     "/api/traces/" + url.PathEscape(traceID),
			validate: validateTraceResponse(traceID),
		},
	}
}

func verifyMediumFixture(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	auth readCredential,
	issueID int,
	logMarker, traceService, traceID string,
	start, end int64,
	required fixtureCorpus,
) (queryFixtureResult, error) {
	result := queryFixtureResult{Required: required}
	issuePath := "/api/v1/issues?status=open&sort=last_seen&page=1"
	status, header, body, err := requestQuery(ctx, client, baseURL+issuePath, auth)
	if err != nil {
		return result, fmt.Errorf("issues corpus request: %w", err)
	}
	if status/100 != 2 {
		return result, fmt.Errorf("issues corpus request returned HTTP %d", status)
	}
	issueTotal, err := inspectIssueListResponse(header, body, issueID, 1, required.Issues)
	if err != nil {
		return result, fmt.Errorf("issues corpus: %w", err)
	}
	result.Observed.Issues = issueTotal

	hitValues := url.Values{
		"query": {`_msg:=` + strconv.Quote(logMarker)},
		"start": {strconv.FormatInt(start, 10)},
		"end":   {strconv.FormatInt(end, 10)},
		"step":  {"1h"},
	}
	status, header, body, err = requestQuery(ctx, client, baseURL+"/api/v1/logs/hits?"+hitValues.Encode(), auth)
	if err != nil {
		return result, fmt.Errorf("logs corpus request: %w", err)
	}
	if status/100 != 2 {
		return result, fmt.Errorf("logs corpus request returned HTTP %d", status)
	}
	logTotal, err := inspectLogsHitsResponse(header, body, required.Logs)
	if err != nil {
		return result, fmt.Errorf("logs corpus: %w", err)
	}
	result.Observed.Logs = logTotal

	traceValues := url.Values{
		"service": {traceService},
		"start":   {strconv.FormatInt(start*1_000_000, 10)},
		"end":     {strconv.FormatInt(end*1_000_000, 10)},
		"limit":   {strconv.Itoa(required.Traces)},
	}
	status, header, body, err = requestQuery(ctx, client, baseURL+"/api/traces/search?"+traceValues.Encode(), auth)
	if err != nil {
		return result, fmt.Errorf("traces corpus request: %w", err)
	}
	if status/100 != 2 {
		return result, fmt.Errorf("traces corpus request returned HTTP %d", status)
	}
	traceTotal, err := inspectTraceSearchResponse(header, body, traceID, required.Traces)
	if err != nil {
		return result, fmt.Errorf("traces corpus: %w", err)
	}
	result.Observed.Traces = traceTotal
	result.Verified = true
	return result, nil
}

func validateBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("base-url is not a valid URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("base-url must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("base-url must not contain credentials, a query, or a fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func measureQueryEndpoint(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	auth readCredential,
	target queryTarget,
	warmup, requests int,
	maxP95MS float64,
) (queryEndpointResult, error) {
	if target.validate == nil {
		return queryEndpointResult{}, fmt.Errorf("query target %s has no response contract validator", target.name)
	}
	for index := 0; index < warmup; index++ {
		status, _, err := executeQuery(ctx, client, baseURL+target.path, auth, target.validate)
		if err != nil {
			return queryEndpointResult{}, fmt.Errorf("warmup request %d: %w", index+1, err)
		}
		if status/100 != 2 {
			return queryEndpointResult{}, fmt.Errorf("warmup request %d returned HTTP %d", index+1, status)
		}
	}
	latencies := make([]time.Duration, 0, requests)
	failures := 0
	for index := 0; index < requests; index++ {
		status, latency, err := executeQuery(ctx, client, baseURL+target.path, auth, target.validate)
		if latency > 0 {
			latencies = append(latencies, latency)
		}
		if err != nil || status/100 != 2 {
			failures++
		}
	}
	if len(latencies) != requests {
		return queryEndpointResult{}, fmt.Errorf(
			"only %d of %d requests produced a latency sample",
			len(latencies),
			requests,
		)
	}
	summary := summarizeLatencies(latencies)
	return queryEndpointResult{
		Name: target.name, Path: target.path, Requests: requests, Failures: failures,
		Latency: summary, MaxP95MS: maxP95MS,
		GatePassed: failures == 0 && summary.P95MS < maxP95MS,
	}, nil
}

func executeQuery(
	ctx context.Context,
	client *http.Client,
	target string,
	auth readCredential,
	validate func(http.Header, []byte) error,
) (int, time.Duration, error) {
	started := time.Now()
	status, header, body, err := requestQuery(ctx, client, target, auth)
	if err != nil {
		return status, time.Since(started), err
	}
	if status/100 == 2 && validate != nil {
		if err := validate(header, body); err != nil {
			return status, time.Since(started), err
		}
	}
	return status, time.Since(started), nil
}

func requestQuery(
	ctx context.Context,
	client *http.Client,
	target string,
	auth readCredential,
) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	auth.apply(req)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxQueryResponseBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, resp.Header, nil, readErr
	}
	if closeErr != nil {
		return resp.StatusCode, resp.Header, nil, closeErr
	}
	if len(body) > maxQueryResponseBytes {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf(
			"response exceeds %d-byte validation limit",
			maxQueryResponseBytes,
		)
	}
	return resp.StatusCode, resp.Header, body, nil
}

func validateIssueListResponse(expectedID, minIssues, minTotal int) func(http.Header, []byte) error {
	return func(header http.Header, body []byte) error {
		_, err := inspectIssueListResponse(header, body, expectedID, minIssues, minTotal)
		return err
	}
}

func inspectIssueListResponse(header http.Header, body []byte, expectedID, minIssues, minTotal int) (int, error) {
	if err := requireMediaType(header, "application/json"); err != nil {
		return 0, fmt.Errorf("issues response: %w", err)
	}
	var payload struct {
		Issues   []issueFixture `json:"issues"`
		Total    int            `json:"total"`
		Page     int            `json:"page"`
		PageSize int            `json:"page_size"`
	}
	if err := decodeSingleJSON(body, &payload); err != nil {
		return 0, fmt.Errorf("issues response is not a single JSON object: %w", err)
	}
	if payload.Total <= 0 || payload.Page <= 0 || payload.PageSize <= 0 || len(payload.Issues) == 0 {
		return 0, fmt.Errorf("issues response has no paginated fixture rows")
	}
	if payload.Total < minTotal {
		return 0, fmt.Errorf(
			"issues corpus has %d rows, below the required medium fixture of %d",
			payload.Total,
			minTotal,
		)
	}
	if len(payload.Issues) < minIssues {
		return 0, fmt.Errorf(
			"issues response has %d rows, below the required measured page size of %d",
			len(payload.Issues),
			minIssues,
		)
	}
	found := false
	for index, issue := range payload.Issues {
		if issue.ID <= 0 || !validIssueFixture(issue) {
			return 0, fmt.Errorf("issues response row %d does not match the issue-list contract", index+1)
		}
		if issue.ID == expectedID {
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("issues response does not contain the expected fixture identity")
	}
	return payload.Total, nil
}

func inspectLogsHitsResponse(header http.Header, body []byte, minTotal int) (int, error) {
	if err := requireMediaType(header, "application/json"); err != nil {
		return 0, fmt.Errorf("logs hits response: %w", err)
	}
	var payload struct {
		Hits []struct {
			Total json.Number `json:"total"`
		} `json:"hits"`
	}
	if err := decodeSingleJSON(body, &payload); err != nil {
		return 0, fmt.Errorf("logs hits response is not a single JSON object: %w", err)
	}
	if len(payload.Hits) == 0 {
		return 0, fmt.Errorf("logs hits response has no series")
	}
	total := int64(0)
	for index, hit := range payload.Hits {
		value, err := hit.Total.Int64()
		if err != nil || value < 0 {
			return 0, fmt.Errorf("logs hits series %d has an invalid total", index+1)
		}
		total += value
	}
	if total < int64(minTotal) {
		return int(
				total,
			), fmt.Errorf(
				"logs corpus has %d rows, below the required medium fixture of %d",
				total,
				minTotal,
			)
	}
	if total > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("logs corpus count exceeds the platform integer range")
	}
	return int(total), nil
}

func inspectTraceSearchResponse(header http.Header, body []byte, expectedTraceID string, minTotal int) (int, error) {
	if err := requireMediaType(header, "application/json"); err != nil {
		return 0, fmt.Errorf("trace search response: %w", err)
	}
	var payload struct {
		Data []struct {
			TraceID string `json:"traceID"`
		} `json:"data"`
	}
	if err := decodeSingleJSON(body, &payload); err != nil {
		return 0, fmt.Errorf("trace search response is not a single JSON object: %w", err)
	}
	unique := make(map[string]struct{}, len(payload.Data))
	found := false
	for index, trace := range payload.Data {
		traceID := strings.TrimSpace(trace.TraceID)
		if traceID == "" {
			return 0, fmt.Errorf("trace search row %d has no trace ID", index+1)
		}
		unique[traceID] = struct{}{}
		found = found || traceID == expectedTraceID
	}
	if len(unique) < minTotal {
		return len(
				unique,
			), fmt.Errorf(
				"trace corpus has %d unique traces, below the required medium fixture of %d",
				len(unique),
				minTotal,
			)
	}
	if !found {
		return len(unique), fmt.Errorf("trace corpus does not contain the measured trace identity")
	}
	return len(unique), nil
}

func validIssueFixture(issue issueFixture) bool {
	validLevel := issue.Level == "error" || issue.Level == "warning" || issue.Level == "info"
	return strings.TrimSpace(issue.Fingerprint) != "" &&
		strings.TrimSpace(issue.Title) != "" &&
		validLevel && issue.Status == "open" &&
		issue.Count > 0 && issue.UserCount >= 0 &&
		issue.FirstSeen > 0 && issue.LastSeen >= issue.FirstSeen &&
		issue.Trend != nil
}

func validateLogsResponse(expectedMarker string, minRows int) func(http.Header, []byte) error {
	return func(header http.Header, body []byte) error {
		if err := requireMediaType(header, "application/stream+json", "application/x-ndjson"); err != nil {
			return fmt.Errorf("logs response: %w", err)
		}
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 64<<10), maxQueryResponseBytes)
		rows := 0
		matchingRows := 0
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			rows++
			var row map[string]json.RawMessage
			if err := decodeSingleJSON(line, &row); err != nil || row == nil {
				return fmt.Errorf("logs response line %d is not a JSON object", rows)
			}
			var message string
			if err := json.Unmarshal(row["_msg"], &message); err != nil || strings.TrimSpace(message) == "" {
				return fmt.Errorf("logs response line %d has no non-empty _msg", rows)
			}
			if !validLogTimestamp(row["_time"]) {
				return fmt.Errorf("logs response line %d has no valid _time", rows)
			}
			if message == expectedMarker {
				matchingRows++
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("scan logs response: %w", err)
		}
		if rows < minRows {
			return fmt.Errorf(
				"logs response has %d JSON Lines rows, below the required fixture size of %d",
				rows,
				minRows,
			)
		}
		if matchingRows < minRows {
			return fmt.Errorf(
				"logs response has %d matching fixture rows, below the required fixture size of %d",
				matchingRows,
				minRows,
			)
		}
		return nil
	}
}

func validLogTimestamp(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var number json.Number
	return json.Unmarshal(raw, &number) == nil && number.String() != ""
}

func validateTraceResponse(expectedTraceID string) func(http.Header, []byte) error {
	return func(header http.Header, body []byte) error {
		if err := requireMediaType(header, "application/json"); err != nil {
			return fmt.Errorf("trace response: %w", err)
		}
		var payload struct {
			Data []struct {
				TraceID string `json:"traceID"`
				Spans   []struct {
					TraceID       string `json:"traceID"`
					SpanID        string `json:"spanID"`
					OperationName string `json:"operationName"`
				} `json:"spans"`
			} `json:"data"`
		}
		if err := decodeSingleJSON(body, &payload); err != nil {
			return fmt.Errorf("trace response is not a single JSON object: %w", err)
		}
		for _, trace := range payload.Data {
			if trace.TraceID != expectedTraceID || strings.TrimSpace(trace.TraceID) == "" || len(trace.Spans) == 0 {
				continue
			}
			for _, span := range trace.Spans {
				if span.TraceID == expectedTraceID && strings.TrimSpace(span.SpanID) != "" &&
					strings.TrimSpace(span.OperationName) != "" {
					return nil
				}
			}
		}
		return fmt.Errorf("trace response does not contain the expected non-empty fixture identity and span")
	}
}

func requireMediaType(header http.Header, accepted ...string) error {
	raw := strings.TrimSpace(header.Get("Content-Type"))
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("invalid Content-Type %q", raw)
	}
	for _, candidate := range accepted {
		if strings.EqualFold(mediaType, candidate) {
			return nil
		}
	}
	return fmt.Errorf("unexpected Content-Type %q", mediaType)
}

func decodeSingleJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
