package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
)

func TestSelfMonitoringInstrumentationSkipsIngestAndInternalToken(t *testing.T) {
	var observed int
	instrumentation := func(c fiber.Ctx) error {
		observed++
		return c.Next()
	}
	upstream := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	defer upstream.Close()
	tokens := &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}
	app := newTestGateway(
		Config{
			Tokens:              tokens,
			Metrics:             upstream.URL,
			Instrumentation:     instrumentation,
			SelfMonitoringToken: "internal",
		},
	).App()

	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent || observed != 0 {
		t.Fatalf("ingest status=%d observed=%d err=%v", resp.StatusCode, observed, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+"internal")
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK || observed != 0 {
		t.Fatalf("internal marker status=%d observed=%d err=%v", resp.StatusCode, observed, err)
	}
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if err != nil || resp.StatusCode != http.StatusOK || observed != 1 {
		t.Fatalf("normal request status=%d observed=%d err=%v", resp.StatusCode, observed, err)
	}
}

type fakeTokens struct {
	mu       sync.Mutex
	revision uint64
	project  meta.ProjectInfo
	err      error
	calls    int
}

func TestMCPAuthenticationRequiresBearer(t *testing.T) {
	tokens := &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}
	app := newTestGateway(Config{Tokens: tokens}).App()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	for _, test := range []struct {
		name, target, header string
		want                 int
	}{{"missing", "/mcp", "", http.StatusUnauthorized}, {"user-token", "/mcp", "lou_valid", http.StatusOK}, {"project-token-forbidden", "/mcp", "valid", http.StatusForbidden}, {"query-token-rejected", "/mcp?token=lou_valid", "", http.StatusUnauthorized}} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.target, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if test.header != "" {
				req.Header.Set("Authorization", "Bearer "+test.header)
			}
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
			if err != nil || resp.StatusCode != test.want {
				t.Fatalf("status=%d want=%d err=%v", resp.StatusCode, test.want, err)
			}
		})
	}
}

func TestPrometheusQueriesUseASTIsolation(t *testing.T) {
	captured := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.URL.Query().Get("query")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	backend, err := storage.New(storage.MetricsPrometheus, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(
		Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}, Storage: backend},
	).App()
	query := `rate(requests_total{project_id=~".*"}[5m]) + up`
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?"+url.Values{"query": {query}, "extra_filters[]": {`{project_id="evil"}`}}.Encode(),
		nil,
	)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%v err=%v", resp.StatusCode, err)
	}
	got := <-captured
	expr, err := storage.InjectPromQL(query, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got != expr {
		t.Fatalf("query=%q want=%q", got, expr)
	}
}

func TestMetricsPrettifyUsesAuthenticatedParserOnlyRoute(t *testing.T) {
	type capture struct {
		path  string
		query url.Values
	}
	captured := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capture{path: r.URL.Path, query: r.URL.Query()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","query":"sum(up)"}`)
	}))
	defer upstream.Close()
	backend, err := storage.New(storage.MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(
		Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}, Storage: backend},
	).App()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prettify-query?"+url.Values{
		"query":           {"sum( up )"},
		"extra_filters[]": {`{project_id="evil"}`},
		"unexpected":      {"drop-me"},
	}.Encode(), nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d err=%v", resp.StatusCode, err)
	}
	got := <-captured
	if got.path != "/prettify-query" {
		t.Fatalf("path=%q want=/prettify-query", got.path)
	}
	if got.query.Get("query") != "sum( up )" || len(got.query) != 1 {
		t.Fatalf("query=%v want parser input only", got.query)
	}
}

func TestMetricsAutocompleteMetadataIsProjectScoped(t *testing.T) {
	for _, mode := range []storage.MetricsKind{storage.MetricsVM, storage.MetricsPrometheus} {
		t.Run(string(mode), func(t *testing.T) {
			captured := make(chan url.Values, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"success","data":["up"]}`)
			}))
			defer upstream.Close()
			backend, err := storage.New(mode, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			app := newTestGateway(
				Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}, Storage: backend},
			).App()
			selector := `{__name__=~"http_.*"}`
			req := httptest.NewRequest(http.MethodGet, "/api/v1/label/__name__/values?"+url.Values{
				"match[]":         {selector},
				"extra_filters[]": {`{project_id="evil"}`},
			}.Encode(), nil)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d err=%v", resp.StatusCode, err)
			}
			values := <-captured
			if mode == storage.MetricsVM {
				if values.Get("extra_filters[]") != `{project_id="alpha"}` || values.Get("match[]") != selector {
					t.Fatalf("VM metadata query=%v", values)
				}
			} else {
				want, injectErr := storage.InjectPromQL(selector, "alpha")
				if injectErr != nil {
					t.Fatal(injectErr)
				}
				if values.Get("match[]") != want || values.Has("extra_filters[]") {
					t.Fatalf("Prometheus metadata query=%v want match=%q", values, want)
				}
			}
		})
	}
}

func TestMetricsIngestWithoutURLReturns501(t *testing.T) {
	upstream := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) { t.Fatalf("unexpected upstream request: %s", r.URL) },
		),
	)
	defer upstream.Close()
	backend, err := storage.New(storage.MetricsVM, upstream.URL, "", upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(
		Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}, Storage: backend},
	).App()
	for _, path := range []string{"/v1/metrics", "/api/v1/write"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("payload"))
		req.Header.Set("Authorization", "Bearer "+"lou_valid")
		resp, testErr := app.Test(req)
		if testErr != nil {
			t.Fatal(testErr)
		}
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s status=%d want=501", path, resp.StatusCode)
		}
	}
}

func TestVMClusterUsesPlatformTenantPathAndProjectLabels(t *testing.T) {
	type capture struct{ path, query string }
	captures := make(chan capture, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures <- capture{path: r.URL.Path, query: r.URL.RawQuery}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	backend, err := storage.NewWithMetricsOptions(
		storage.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		storage.MetricsOptions{Cluster: true, TenantAccountID: "7"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(
		Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}}, Storage: backend},
	).App()
	for _, tc := range []struct {
		method, target, wantPath string
		wantLabel, wantFilter    bool
	}{
		{http.MethodGet, "/api/v1/query?query=up&extra_filters%5B%5D=evil", "/select/7/prometheus/api/v1/query", false, true},
		{http.MethodPost, "/v1/metrics?extra_label=evil", "/insert/7/opentelemetry/v1/metrics", true, false},
		{http.MethodPost, "/api/v1/write?extra_label=evil", "/insert/7/prometheus/api/v1/write", false, false},
	} {
		body := "payload"
		if strings.HasPrefix(tc.target, "/api/v1/write") {
			body = remoteWriteBody(t, "evil")
		}
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+"lou_valid")
		resp, testErr := app.Test(req)
		if testErr != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s %s status=%d err=%v", tc.method, tc.target, resp.StatusCode, testErr)
		}
		got := <-captures
		if got.path != tc.wantPath {
			t.Errorf("%s path=%q want=%q", tc.target, got.path, tc.wantPath)
		}
		values, _ := url.ParseQuery(got.query)
		if values.Has("extra_filters") || values.Has("extra_label") != tc.wantLabel ||
			(tc.wantLabel && values.Get("extra_label") != "project_id=alpha") ||
			values.Has("extra_filters[]") != tc.wantFilter ||
			(tc.wantFilter && values.Get("extra_filters[]") != `{project_id="alpha"}`) {
			t.Errorf("%s sanitized query=%v", tc.target, values)
		}
	}
}

func (f *fakeTokens) AuthenticateProjectAPIToken(context.Context, string) (meta.ProjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.project, f.err
}

func (f *fakeTokens) Revision() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision
}

func TestAuthentication401And200(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	tokens := &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}
	app := newTestGateway(Config{Tokens: tokens, Metrics: upstream.URL}).App()

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
	resp, err := app.Test(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without token: got status %d, want 401", resp.StatusCode)
	}

	authorized := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
	authorized.Header.Set("Authorization", "Bearer valid-token")
	resp, err = app.Test(authorized)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("with token: got status %d, want 204", resp.StatusCode)
	}
}

func TestAuthenticationCacheInvalidatesOnStoreWrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	tokens := &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}
	app := newTestGateway(Config{Tokens: tokens, Metrics: upstream.URL}).App()
	request := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+"cached-token")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if got := request(); got != http.StatusNoContent {
		t.Fatalf("first request status = %d", got)
	}
	if got := request(); got != http.StatusNoContent {
		t.Fatalf("cached request status = %d", got)
	}
	if tokens.calls != 1 {
		t.Fatalf("store calls = %d, want 1 cache hit", tokens.calls)
	}
	tokens.mu.Lock()
	tokens.revision++
	tokens.err = meta.ErrInvalidProjectAPIToken
	tokens.mu.Unlock()
	if got := request(); got != http.StatusUnauthorized {
		t.Fatalf("request after revoke status = %d, want 401", got)
	}
	if tokens.calls != 2 {
		t.Fatalf("store calls after invalidation = %d, want 2", tokens.calls)
	}
}

func TestAuthenticationStoreFailureReturns503(t *testing.T) {
	tokens := &fakeTokens{err: errors.New("metadata unavailable")}
	app := newTestGateway(Config{Tokens: tokens}).App()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+"valid-shape-token")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d err=%v", resp.StatusCode, err)
	}
}

func TestDefaultProxyClientKeepsAConcurrentIdlePool(t *testing.T) {
	gateway := newTestGateway(Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 1, Slug: "default"}}})
	transport, ok := gateway.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default proxy transport type = %T", gateway.client.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("gateway mutated or reused the process-global default transport")
	}
	if transport.MaxIdleConns != defaultProxyMaxIdleConnections ||
		transport.MaxIdleConnsPerHost != defaultProxyMaxIdleConnectionsPerHost {
		t.Fatalf(
			"idle pool=%d/%d, want %d/%d",
			transport.MaxIdleConns,
			transport.MaxIdleConnsPerHost,
			defaultProxyMaxIdleConnections,
			defaultProxyMaxIdleConnectionsPerHost,
		)
	}
	defaultTransport := http.DefaultTransport.(*http.Transport)
	if transport.Proxy == nil || defaultTransport.Proxy == nil ||
		transport.TLSHandshakeTimeout != defaultTransport.TLSHandshakeTimeout {
		t.Fatal("default proxy/TLS transport behavior was not preserved by Clone")
	}
}

func TestFlushingWriterMakesTailChunksImmediatelyVisible(t *testing.T) {
	var output bytes.Buffer
	buffered := bufio.NewWriterSize(&output, 1_024)
	writer := flushingWriter{writer: buffered}

	if _, err := writer.Write([]byte("{\"_msg\":\"one\"}\n")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "{\"_msg\":\"one\"}\n" {
		t.Fatalf("flushed output=%q", got)
	}
}

func TestLogsQueryAliasesMapPathsAndTenants(t *testing.T) {
	type capture struct {
		method      string
		path        string
		body        string
		contentType string
		accountID   string
		projectID   string
	}
	captures := make(chan capture, 7)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captures <- capture{
			method:      r.Method,
			path:        r.URL.Path,
			body:        string(body),
			contentType: r.Header.Get("Content-Type"),
			accountID:   r.Header.Get("AccountID"),
			projectID:   r.Header.Get("ProjectID"),
		}
		switch r.URL.Path {
		case "/select/logsql/tail":
			w.Header().Set("Content-Type", "application/stream+json")
			_, _ = io.WriteString(w, "{\"_msg\":\"one\"}\n{\"_msg\":\"two\"}\n")
		case "/select/logsql/query_time_range":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(
				w,
				`{"start":"2026-07-24T00:00:00Z","end":"2026-07-24T01:00:00Z","hasTimeFilter":true}`,
			)
		case "/select/logsql/stats_query_range":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"values":[{"value":"service","hits":2}]}`)
		}
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
		Client: upstream.Client(),
	}).App()

	unauthorizedRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/logs/field_names",
		strings.NewReader(url.Values{"query": {"*"}}.Encode()),
	)
	unauthorizedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unauthorizedResponse, err := app.Test(unauthorizedRequest)
	if err != nil || unauthorizedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized metadata status=%d err=%v", unauthorizedResponse.StatusCode, err)
	}
	_ = unauthorizedResponse.Body.Close()
	if len(captures) != 0 {
		t.Fatal("unauthorized metadata request reached VictoriaLogs")
	}

	tests := []struct {
		name        string
		apiPath     string
		upstream    string
		form        url.Values
		contentType string
		response    string
	}{
		{
			name:     "stats query range",
			apiPath:  "/api/v1/logs/stats_query_range",
			upstream: "/select/logsql/stats_query_range",
			form: url.Values{
				"query": {"* | stats by (service) count()"},
				"start": {"2026-07-24T00:00:00Z"},
				"end":   {"2026-07-24T01:00:00Z"},
				"step":  {"1m"},
			},
			contentType: "application/json",
			response:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		},
		{
			name:     "query time range",
			apiPath:  "/api/v1/logs/query_time_range",
			upstream: "/select/logsql/query_time_range",
			form: url.Values{
				"query": {"_time:1h"},
				"start": {"1753315200000000000"},
				"end":   {"1753318800000000000"},
			},
			contentType: "application/json",
			response:    `{"start":"2026-07-24T00:00:00Z","end":"2026-07-24T01:00:00Z","hasTimeFilter":true}`,
		},
		{
			name:     "field names",
			apiPath:  "/api/v1/logs/field_names",
			upstream: "/select/logsql/field_names",
			form: url.Values{
				"query": {"*"},
				"start": {"1753315200000000000"},
				"end":   {"1753318800000000000"},
				"limit": {"1000"},
			},
			contentType: "application/json",
			response:    `{"values":[{"value":"service","hits":2}]}`,
		},
		{
			name:     "field values",
			apiPath:  "/api/v1/logs/field_values",
			upstream: "/select/logsql/field_values",
			form: url.Values{
				"query": {"*"},
				"field": {"service"},
				"start": {"1753315200000000000"},
				"end":   {"1753318800000000000"},
				"limit": {"1000"},
			},
			contentType: "application/json",
			response:    `{"values":[{"value":"service","hits":2}]}`,
		},
		{
			name:     "stream field names",
			apiPath:  "/api/v1/logs/stream_field_names",
			upstream: "/select/logsql/stream_field_names",
			form: url.Values{
				"query":                {"*"},
				"start":                {"1753315200000000000"},
				"end":                  {"1753318800000000000"},
				"ignore_pipes":         {"1"},
				"extra_stream_filters": {`service:="api"`},
			},
			contentType: "application/json",
			response:    `{"values":[{"value":"service","hits":2}]}`,
		},
		{
			name:     "stream field values",
			apiPath:  "/api/v1/logs/stream_field_values",
			upstream: "/select/logsql/stream_field_values",
			form: url.Values{
				"query":                {"*"},
				"field":                {"service"},
				"start":                {"1753315200000000000"},
				"end":                  {"1753318800000000000"},
				"ignore_pipes":         {"1"},
				"extra_stream_filters": {`service:="api"`},
			},
			contentType: "application/json",
			response:    `{"values":[{"value":"service","hits":2}]}`,
		},
		{
			name:     "tail",
			apiPath:  "/api/v1/logs/tail",
			upstream: "/select/logsql/tail",
			form: url.Values{
				"query":  {"*"},
				"offset": {"5s"},
			},
			contentType: "application/stream+json",
			response:    "{\"_msg\":\"one\"}\n{\"_msg\":\"two\"}\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encodedForm := test.form.Encode()
			req := httptest.NewRequest(http.MethodPost, test.apiPath, strings.NewReader(encodedForm))
			req.Header.Set("Authorization", "Bearer lou_valid")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("AccountID", "999")
			req.Header.Set("ProjectID", "999")

			resp, requestErr := app.Test(req)
			if requestErr != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d err=%v", resp.StatusCode, requestErr)
			}
			responseBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := resp.Header.Get("Content-Type"); got != test.contentType {
				t.Fatalf("response content type=%q want=%q", got, test.contentType)
			}
			if string(responseBody) != test.response {
				t.Fatalf("response=%q want=%q", responseBody, test.response)
			}

			got := <-captures
			if got.method != http.MethodPost || got.path != test.upstream {
				t.Fatalf("upstream request=%s %s want=POST %s", got.method, got.path, test.upstream)
			}
			if got.body != encodedForm {
				t.Fatalf("upstream body=%q want=%q", got.body, encodedForm)
			}
			if got.contentType != "application/x-www-form-urlencoded" {
				t.Fatalf("upstream content type=%q", got.contentType)
			}
			if got.accountID != "0" || got.projectID != "42" {
				t.Fatalf("upstream tenant=(%q,%q) want=(0,42)", got.accountID, got.projectID)
			}
		})
	}
}

func TestReservedParametersAndTenantHeadersAreStripped(t *testing.T) {
	type captured struct {
		path, rawQuery, body, accountID, projectID string
	}
	captures := make(chan captured, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captures <- captured{r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("AccountID"), r.Header.Get("ProjectID")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	tokens := &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}}
	app := newTestGateway(Config{Tokens: tokens, Metrics: upstream.URL, Logs: upstream.URL}).App()

	query := url.Values{
		"query":           []string{"up"},
		"extra_filters":   []string{`{project_id="other"}`},
		"extra_filters[]": []string{`{project_id="other"}`, `{project_id="also-other"}`},
		"extra_label":     []string{"project_id=other"},
		"unrelated":       []string{"kept"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+query.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("GET proxy: status=%v err=%v", resp.StatusCode, err)
	}
	got := <-captures
	parsed, _ := url.ParseQuery(got.rawQuery)
	if values := parsed["extra_filters[]"]; len(values) != 1 || values[0] != `{project_id="alpha"}` {
		t.Fatalf("trusted filters = %#v", values)
	}
	if parsed.Has("extra_filters") || parsed.Has("extra_label") || parsed.Get("unrelated") != "kept" {
		t.Fatalf("sanitized query = %#v", parsed)
	}

	form := url.Values{
		"query":           []string{"up"},
		"extra_filters":   []string{"evil"},
		"extra_filters[]": []string{"evil"},
		"extra_label":     []string{"evil"},
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/query?extra_label=evil", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST proxy: status=%v err=%v", resp.StatusCode, err)
	}
	got = <-captures
	formAfter, _ := url.ParseQuery(got.body)
	if formAfter.Has("extra_filters") || formAfter.Has("extra_filters[]") || formAfter.Has("extra_label") ||
		formAfter.Get("query") != "up" {
		t.Fatalf("sanitized form = %#v", formAfter)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*&extra_label=client-value", nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	req.Header.Set("AccountID", "999")
	req.Header.Set("ProjectID", "999")
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logs proxy: status=%v err=%v", resp.StatusCode, err)
	}
	got = <-captures
	if got.accountID != "0" || got.projectID != "42" {
		t.Fatalf("tenant headers = (%q,%q), want (0,42)", got.accountID, got.projectID)
	}
}

func TestLegacyPoCLogsQueryRouteIsNotRegistered(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
	}).App()
	req := httptest.NewRequest(http.MethodGet, "/select/logsql/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request legacy Logs route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy Logs route status=%d want=%d", resp.StatusCode, http.StatusNotFound)
	}
	if called {
		t.Fatal("legacy Logs route reached the upstream backend")
	}
}

func TestMemberRequiredUsesExplicitRoleWhitelist(t *testing.T) {
	app := fiber.New()
	app.Get("/protected", func(c fiber.Ctx) error {
		role := c.Get("X-Test-Role")
		if !roleSatisfies(role, roleMember) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	for _, tc := range []struct {
		name string
		role string
		want int
	}{
		{"admin allowed", "admin", http.StatusNoContent},
		{"member allowed", "member", http.StatusNoContent},
		{"viewer forbidden", "viewer", http.StatusForbidden},
		{"empty role forbidden", "", http.StatusForbidden},
		{"unknown role forbidden", "auditor", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tc.role != "" {
				req.Header.Set("X-Test-Role", tc.role)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("role=%q status=%d want=%d", tc.role, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestMissingOrDeletedProjectReturns404WithProjectNotFoundCode(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.CreateProject(ctx, "Default", "default"); err != nil {
		t.Fatal(err)
	}
	doomed, err := store.CreateProject(ctx, "Doomed", "doomed-proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, doomed.ID); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "proj-404-admin", "password", "admin")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store,
		Metrics: upstream.URL, Logs: upstream.URL, Traces: upstream.URL,
	}).App()

	for _, slug := range []string{"nonexistent-project", "doomed-proj"} {
		for _, path := range []string{"/api/issues", "/api/hosts", "/api/v1/query?query=up", "/api/v1/logs/query?query=*"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-LO-Project", slug)
			req.AddCookie(admin)
			resp, testErr := app.Test(req)
			if testErr != nil {
				t.Fatal(testErr)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("slug=%q path=%s status=%d want=404 body=%s", slug, path, resp.StatusCode, body)
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("slug=%q path=%s invalid JSON: %v body=%s", slug, path, err, body)
			}
			if decoded["code"] != "project_not_found" {
				t.Fatalf("slug=%q path=%s code=%v want=project_not_found body=%s", slug, path, decoded["code"], body)
			}
		}
	}
}
