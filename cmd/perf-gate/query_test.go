package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const validIssueListFixture = `{"issues":[{"id":42,"fingerprint":"fp-42","title":"fixture panic","level":"error","status":"open","count":1,"user_count":0,"first_seen":1784332800000,"last_seen":1784332800000,"trend":[]}],"total":1,"page":1,"page_size":50}`

func TestQueryResponseValidators(t *testing.T) {
	issue := validateIssueListResponse(42, 1, 1)
	logs := validateLogsResponse("fixture-log", 1)
	trace := validateTraceResponse("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	tests := []struct {
		name        string
		validate    func(http.Header, []byte) error
		contentType string
		body        string
		wantErr     bool
	}{
		{
			name:     "issues pass",
			validate: issue, contentType: "application/json; charset=utf-8",
			body: validIssueListFixture,
		},
		{name: "issues empty", validate: issue, contentType: "application/json", wantErr: true},
		{name: "issues HTML", validate: issue, contentType: "text/html", body: `<html>ok</html>`, wantErr: true},
		{
			name:        "issues unrelated JSON",
			validate:    issue,
			contentType: "application/json",
			body:        `{"ok":true}`,
			wantErr:     true,
		},
		{
			name:        "issues empty rows",
			validate:    issue,
			contentType: "application/json",
			body:        `{"issues":[],"total":0,"page":1,"page_size":50}`,
			wantErr:     true,
		},
		{
			name:        "issues wrong identity",
			validate:    issue,
			contentType: "application/json",
			body:        `{"issues":[{"id":41,"fingerprint":"fp-41","title":"other","level":"error","status":"open","count":1,"user_count":0,"first_seen":1784332800000,"last_seen":1784332800000,"trend":[]}],"total":1,"page":1,"page_size":50}`,
			wantErr:     true,
		},
		{
			name:        "issues empty fixture identity",
			validate:    issue,
			contentType: "application/json",
			body:        `{"issues":[{"id":42,"fingerprint":"","title":"fixture panic","level":"error","status":"open","count":1,"user_count":0,"first_seen":1784332800000,"last_seen":1784332800000,"trend":[]}],"total":1,"page":1,"page_size":50}`,
			wantErr:     true,
		},
		{
			name:        "logs stream JSON pass",
			validate:    logs,
			contentType: "application/stream+json; charset=utf-8",
			body:        "{\"_msg\":\"fixture-log\",\"_time\":\"2026-07-18T00:00:00Z\",\"_stream\":\"{service=\\\"fixture\\\"}\"}\n",
		},
		{
			name:     "logs NDJSON pass",
			validate: logs, contentType: "application/x-ndjson",
			body: "{\"_msg\":\"fixture-log\",\"_time\":1784332800}\n",
		},
		{name: "logs empty", validate: logs, contentType: "application/stream+json", wantErr: true},
		{name: "logs HTML", validate: logs, contentType: "text/html", body: `<html>ok</html>`, wantErr: true},
		{
			name:        "logs unrelated JSON",
			validate:    logs,
			contentType: "application/stream+json",
			body:        `{"ok":true}`,
			wantErr:     true,
		},
		{
			name:        "logs wrong identity",
			validate:    logs,
			contentType: "application/stream+json",
			body:        `{"_msg":"other","_time":"2026-07-18T00:00:00Z"}`,
			wantErr:     true,
		},
		{
			name:        "logs missing timestamp",
			validate:    logs,
			contentType: "application/stream+json",
			body:        `{"_msg":"fixture-log"}`,
			wantErr:     true,
		},
		{
			name:        "trace pass",
			validate:    trace,
			contentType: "application/json",
			body:        `{"data":[{"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spans":[{"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanID":"bbbbbbbbbbbbbbbb","operationName":"GET /fixture"}]}]}`,
		},
		{name: "trace empty", validate: trace, contentType: "application/json", wantErr: true},
		{name: "trace HTML", validate: trace, contentType: "text/html", body: `<html>ok</html>`, wantErr: true},
		{
			name:        "trace unrelated JSON",
			validate:    trace,
			contentType: "application/json",
			body:        `{"ok":true}`,
			wantErr:     true,
		},
		{
			name:        "trace wrong identity",
			validate:    trace,
			contentType: "application/json",
			body:        `{"data":[{"traceID":"cccccccccccccccccccccccccccccccc","spans":[{"traceID":"cccccccccccccccccccccccccccccccc","spanID":"bbbbbbbbbbbbbbbb","operationName":"GET /fixture"}]}]}`,
			wantErr:     true,
		},
		{
			name:        "trace empty spans",
			validate:    trace,
			contentType: "application/json",
			body:        `{"data":[{"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spans":[]}]}`,
			wantErr:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			if test.contentType != "" {
				header.Set("Content-Type", test.contentType)
			}
			err := test.validate(header, []byte(test.body))
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestQueryResponseValidatorsEnforceFixtureSize(t *testing.T) {
	issueTwoRows := `{"issues":[` +
		`{"id":42,"fingerprint":"fp-42","title":"fixture panic","level":"error","status":"open","count":1,"user_count":0,"first_seen":1784332800000,"last_seen":1784332800000,"trend":[]},` +
		`{"id":43,"fingerprint":"fp-43","title":"second fixture","level":"error","status":"open","count":1,"user_count":0,"first_seen":1784332800000,"last_seen":1784332800000,"trend":[]}` +
		`],"total":2,"page":1,"page_size":50}`
	logRow := "{\"_msg\":\"fixture-log\",\"_time\":\"2026-07-18T00:00:00Z\"}\n"
	tests := []struct {
		name        string
		validate    func(http.Header, []byte) error
		contentType string
		body        string
		wantErr     bool
	}{
		{
			name:        "issues enough rows",
			validate:    validateIssueListResponse(42, 2, 2),
			contentType: "application/json",
			body:        issueTwoRows,
		},
		{
			name:        "issues total below minimum",
			validate:    validateIssueListResponse(42, 1, 2),
			contentType: "application/json",
			body:        validIssueListFixture,
			wantErr:     true,
		},
		{
			name:        "issues returned rows below minimum",
			validate:    validateIssueListResponse(42, 2, 2),
			contentType: "application/json",
			body:        strings.Replace(validIssueListFixture, `"total":1`, `"total":2`, 1),
			wantErr:     true,
		},
		{
			name:        "logs enough matching rows",
			validate:    validateLogsResponse("fixture-log", 2),
			contentType: "application/stream+json",
			body:        logRow + logRow,
		},
		{
			name:        "logs rows below minimum",
			validate:    validateLogsResponse("fixture-log", 2),
			contentType: "application/stream+json",
			body:        logRow,
			wantErr:     true,
		},
		{
			name:        "logs matching rows below minimum",
			validate:    validateLogsResponse("fixture-log", 2),
			contentType: "application/stream+json",
			body:        logRow + "{\"_msg\":\"other\",\"_time\":\"2026-07-18T00:00:00Z\"}\n",
			wantErr:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			header.Set("Content-Type", test.contentType)
			err := test.validate(header, []byte(test.body))
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestMediumFixtureInspectorsRequireObservedCorpus(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")

	if total, err := inspectIssueListResponse(
		header,
		[]byte(validIssueListFixture),
		42,
		1,
		1,
	); err != nil ||
		total != 1 {
		t.Fatalf("inspectIssueListResponse() = %d, %v", total, err)
	}
	if _, err := inspectIssueListResponse(header, []byte(validIssueListFixture), 42, 1, 2); err == nil {
		t.Fatal("undersized Issue corpus passed")
	}

	hits := []byte(`{"hits":[{"fields":{},"timestamps":["2026-07-18T00:00:00Z"],"values":[100000],"total":100000}]}`)
	if total, err := inspectLogsHitsResponse(header, hits, 100000); err != nil || total != 100000 {
		t.Fatalf("inspectLogsHitsResponse() = %d, %v", total, err)
	}
	if _, err := inspectLogsHitsResponse(header, hits, 100001); err == nil {
		t.Fatal("undersized logs corpus passed")
	}

	traces := []byte(
		`{"data":[{"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"traceID":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`,
	)
	if total, err := inspectTraceSearchResponse(
		header,
		traces,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		2,
	); err != nil ||
		total != 2 {
		t.Fatalf("inspectTraceSearchResponse() = %d, %v", total, err)
	}
	duplicate := []byte(
		`{"data":[{"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`,
	)
	if _, err := inspectTraceSearchResponse(header, duplicate, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 2); err == nil {
		t.Fatal("duplicate trace IDs inflated the corpus")
	}
}

func TestBuildQueryTargetsUsesBoundedFixtureQueries(t *testing.T) {
	targets := buildQueryTargets(42, `fixture "marker"`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 100, 200, 17, 1, 2, 1)
	if len(targets) != 3 || targets[0].validate == nil || targets[1].validate == nil || targets[2].validate == nil {
		t.Fatalf("targets = %#v", targets)
	}
	parsed, err := url.Parse(targets[1].path)
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	if values.Get("query") != `_msg:="fixture \"marker\""` || values.Get("start") != "100" ||
		values.Get("end") != "200" ||
		values.Get("limit") != "17" {
		t.Fatalf("logs query = %#v", values)
	}
	if !strings.Contains(targets[0].path, "status=open") || !strings.Contains(targets[0].path, "sort=last_seen") {
		t.Fatalf("issues path is not a controlled first-page query: %s", targets[0].path)
	}
}

func TestMeasureQueryEndpointValidatesEveryResponse(t *testing.T) {
	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if request == 3 {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = fmt.Fprint(w, validIssueListFixture)
	}))
	defer server.Close()

	var validated atomic.Int32
	contract := validateIssueListResponse(42, 1, 1)
	target := queryTarget{
		name: "issues",
		path: "/api/v1/issues",
		validate: func(header http.Header, body []byte) error {
			validated.Add(1)
			return contract(header, body)
		},
	}
	result, err := measureQueryEndpoint(
		context.Background(),
		server.Client(),
		server.URL,
		testReadAuth,
		target,
		1,
		3,
		1000,
		1500,
	)
	if err != nil {
		t.Fatal(err)
	}
	if served.Load() != 4 || validated.Load() != 4 || result.Failures != 1 || result.GatePassed {
		t.Fatalf("served=%d validated=%d result=%#v", served.Load(), validated.Load(), result)
	}
}

func TestMeasureQueryEndpointRejectsP99OverBudget(t *testing.T) {
	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) == 20 {
			time.Sleep(25 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, validIssueListFixture)
	}))
	defer server.Close()

	target := queryTarget{
		name:     "issues",
		path:     "/api/v1/issues",
		validate: validateIssueListResponse(42, 1, 1),
	}
	result, err := measureQueryEndpoint(
		context.Background(),
		server.Client(),
		server.URL,
		testReadAuth,
		target,
		0,
		20,
		20,
		20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Latency.P95MS >= 20 {
		t.Fatalf("expected p95 below 20ms so only p99 fails: %#v", result.Latency)
	}
	if result.Latency.P99MS < 20 || result.GatePassed {
		t.Fatalf("p99 over budget must fail endpoint gate: %#v", result)
	}
}

func TestMeasureQueryEndpointRejectsMissingValidator(t *testing.T) {
	_, err := measureQueryEndpoint(
		context.Background(),
		http.DefaultClient,
		"http://127.0.0.1",
		testReadAuth,
		queryTarget{name: "unsafe"},
		0,
		1,
		1000,
		1500,
	)
	if err == nil || !strings.Contains(err.Error(), "no response contract validator") {
		t.Fatalf("missing-validator error = %v", err)
	}
}

func TestMeasureQueryEndpointIncludesValidationLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	target := queryTarget{name: "fixture", path: "/fixture", validate: func(http.Header, []byte) error {
		time.Sleep(12 * time.Millisecond)
		return nil
	}}
	result, err := measureQueryEndpoint(
		context.Background(),
		server.Client(),
		server.URL,
		testReadAuth,
		target,
		0,
		2,
		1000,
		1500,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Latency.P95MS < 10 {
		t.Fatalf("validation was excluded from latency: %#v", result.Latency)
	}
}

func TestWarmupContractErrorDoesNotLeakToken(t *testing.T) {
	const token = "secret-token-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"token":%q}`, token)
	}))
	defer server.Close()

	auth := readCredential{token: token, project: "default"}
	_, err := measureQueryEndpoint(context.Background(), server.Client(), server.URL, auth, queryTarget{
		name: "issues", path: "/api/v1/issues", validate: validateIssueListResponse(42, 1, 1),
	}, 1, 1, 1000, 1500)
	if err == nil {
		t.Fatal("invalid warmup response passed")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("contract error leaked token: %v", err)
	}
}

func TestRequestQuerySendsUserTokenAndProjectSelector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertReadCredential(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	status, _, _, err := requestQuery(context.Background(), server.Client(), server.URL+"/api/v1/issues", testReadAuth)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
}

func TestRequestQueryDoesNotFollowRedirectWithCredential(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get(projectHeader) != "" {
			leaked.Add(1)
		}
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	status, _, _, err := requestQuery(context.Background(), client, server.URL+"/api/v1/issues", testReadAuth)
	if err != nil || status != http.StatusFound || leaked.Load() != 0 {
		t.Fatalf("status=%d err=%v leaked=%d", status, err, leaked.Load())
	}
}

func TestLoadReadCredentialRequiresUserTokenAndProject(t *testing.T) {
	t.Setenv("LO_PERF_TEST_READ", "")
	if _, err := loadReadCredential("LO_PERF_TEST_READ", "default"); err == nil ||
		!strings.Contains(err.Error(), "LO_PERF_TEST_READ is empty") {
		t.Fatalf("empty token error = %v", err)
	}
	t.Setenv("LO_PERF_TEST_READ", "lo_project_secret")
	_, err := loadReadCredential("LO_PERF_TEST_READ", "default")
	if err == nil || !strings.Contains(err.Error(), "ingest-only") ||
		strings.Contains(err.Error(), "lo_project_secret") {
		t.Fatalf("project token error = %v", err)
	}
	t.Setenv("LO_PERF_TEST_READ", " lou_user_secret ")
	if _, err := loadReadCredential("LO_PERF_TEST_READ", " "); err == nil {
		t.Fatal("empty project slug accepted")
	}
	auth, err := loadReadCredential("LO_PERF_TEST_READ", " team-a ")
	if err != nil || auth.token != "lou_user_secret" || auth.project != "team-a" {
		t.Fatalf("auth=%#v err=%v", auth, err)
	}
}

// ADR 0010: perf-gate query only reads, so by default it must take the user API
// token from LO_PERF_QUERY_TOKEN, the same variable scale uses for reads.
// LO_PERF_TOKEN holds the project ingest token and must not be consulted.
func TestRunQueryGateDefaultsToQueryTokenEnv(t *testing.T) {
	args := []string{
		"--config", filepath.Join("..", "..", "bench", "performance-gates.json"),
		"--base-url", "http://127.0.0.1:1",
	}
	t.Setenv("LO_PERF_TOKEN", testIngestToken)
	t.Setenv("LO_PERF_QUERY_TOKEN", "")
	err := runQueryGate(args)
	if err == nil || !strings.Contains(err.Error(), "LO_PERF_QUERY_TOKEN is empty") {
		t.Fatalf("missing query token error = %v", err)
	}
	t.Setenv("LO_PERF_TOKEN", "")
	t.Setenv("LO_PERF_QUERY_TOKEN", testReadAuth.token)
	err = runQueryGate(args)
	if err == nil || !strings.Contains(err.Error(), "issue-id") {
		t.Fatalf("user token in LO_PERF_QUERY_TOKEN was not accepted; err = %v", err)
	}
}
