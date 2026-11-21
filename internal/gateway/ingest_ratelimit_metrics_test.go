package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type multiProjectTokens struct {
	projects map[string]meta.ProjectInfo
}

func (m *multiProjectTokens) AuthenticateProjectAPIToken(
	_ context.Context,
	token string,
) (meta.ProjectInfo, error) {
	if p, ok := m.projects[token]; ok {
		return p, nil
	}
	return meta.ProjectInfo{}, meta.ErrInvalidProjectAPIToken
}

func (m *multiProjectTokens) Revision() uint64 { return 1 }

func TestUnifiedIngestRateLimitCoversAllHTTPProjectIngestRoutes(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	var clockMu sync.Mutex
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	limiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             2,
		Now:               nowFn,
	})
	metricsReg := selfmetrics.New(selfmetrics.Config{
		Version:   "v0.1.0-test",
		Commit:    "1234567",
		StartedAt: now,
		Now:       nowFn,
	})
	tokens := &multiProjectTokens{
		projects: map[string]meta.ProjectInfo{
			"lo_project_a": {ID: 101, Slug: "project-a"},
			"lo_project_b": {ID: 202, Slug: "project-b"},
		},
	}

	gw := newTestGateway(Config{
		Tokens:        tokens,
		Metrics:       upstream.URL,
		Logs:          upstream.URL,
		Traces:        upstream.URL,
		Now:           nowFn,
		IngestLimiter: limiter,
		SelfMetrics:   metricsReg,
		MetricsEndpoint: MetricsEndpointConfig{
			Enabled:     true,
			BearerToken: "scrape-secret-token",
		},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	// Verify every project-token ingest route hits the shared rate limiter.
	ingestRoutes := []string{
		"/v1/traces",
		"/v1/metrics",
		"/v1/logs",
		"/api/v1/write",
		"/api/events/errors",
		"/api/events/vitals",
		"/api/events/diagnostics",
		"/api/replay/segments",
		"/api/profiles",
	}

	// Consume project A's burst (2) on /v1/traces -> both reach upstream.
	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer lo_project_a")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("burst %d failed: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("burst %d status=%d want 204", i, resp.StatusCode)
		}
	}
	hitsBeforeThrottle := upstreamHits.Load()

	// Every single HTTP project-token ingest route must immediately reject project A with 429 + Retry-After
	// before forwarding to upstream storage or running route handlers.
	for _, routePath := range ingestRoutes {
		req := httptest.NewRequest(http.MethodPost, routePath, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer lo_project_a")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("POST %s failed: %v", routePath, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("POST %s status=%d want 429 body=%s", routePath, resp.StatusCode, string(raw))
		}
		retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil || retryAfter < 1 {
			t.Fatalf("POST %s Retry-After=%q want positive integer", routePath, resp.Header.Get("Retry-After"))
		}
		var payload struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("POST %s invalid JSON body=%q: %v", routePath, string(raw), err)
		}
		if payload.Code != "ingest_rate_limited" || payload.Error == "" {
			t.Fatalf("POST %s unexpected error payload=%+v", routePath, payload)
		}
		if strings.Contains(string(raw), "project-a") || strings.Contains(string(raw), "101") {
			t.Fatalf("POST %s leaked internal project identifier in body=%s", routePath, string(raw))
		}
	}
	if upstreamHits.Load() != hitsBeforeThrottle {
		t.Fatalf("rate-limited requests reached upstream: before=%d after=%d", hitsBeforeThrottle, upstreamHits.Load())
	}

	// Project B must be isolated and succeed immediately.
	reqB := httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(`{}`))
	reqB.Header.Set("Authorization", "Bearer lo_project_b")
	respB, err := app.Test(reqB)
	if err != nil {
		t.Fatal(err)
	}
	_ = respB.Body.Close()
	if respB.StatusCode != http.StatusNoContent {
		t.Fatalf("project B status=%d want 204", respB.StatusCode)
	}

	// Advance clock by 1 second -> project A refills 1 token and succeeds again.
	advance(time.Second)
	reqRecovered := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	reqRecovered.Header.Set("Authorization", "Bearer lo_project_a")
	respRecovered, err := app.Test(reqRecovered)
	if err != nil {
		t.Fatal(err)
	}
	_ = respRecovered.Body.Close()
	if respRecovered.StatusCode != http.StatusNoContent {
		t.Fatalf("project A after refill status=%d want 204", respRecovered.StatusCode)
	}

	// Scrape /metrics and validate counters and low-cardinality constraints.
	scrapeReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	scrapeReq.Header.Set("Authorization", "Bearer scrape-secret-token")
	scrapeResp, err := app.Test(scrapeReq)
	if err != nil {
		t.Fatal(err)
	}
	scrapeBody, _ := io.ReadAll(scrapeResp.Body)
	_ = scrapeResp.Body.Close()
	if scrapeResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status=%d body=%s", scrapeResp.StatusCode, string(scrapeBody))
	}
	text := string(scrapeBody)
	for _, forbidden := range []string{"project-a", "project-b", "lo_project_a", "scrape-secret-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("/metrics output leaked sensitive or high-cardinality token %q", forbidden)
		}
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("invalid Prometheus text format: %v", err)
	}
	if _, ok := families["lazy_observe_ingest_rate_limited_total"]; !ok {
		t.Fatal("missing lazy_observe_ingest_rate_limited_total")
	}
	var totalHTTP429 float64
	for _, m := range families["lazy_observe_ingest_rate_limited_total"].GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "protocol" && lp.GetValue() == "http" {
				totalHTTP429 += m.GetCounter().GetValue()
			}
		}
	}
	if totalHTTP429 != float64(len(ingestRoutes)) {
		t.Fatalf("http rate_limited_total=%v want %d", totalHTTP429, len(ingestRoutes))
	}
}

func TestUnifiedIngestByteRateLimitRejectsBeforeUpstream(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	limiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 100,
		Burst:             100,
		BytesPerSecond:    100,
		ByteBurst:         100,
		Now:               func() time.Time { return now },
	})
	gw := newTestGateway(Config{
		Tokens:        &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Traces:        upstream.URL,
		IngestLimiter: limiter,
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	// 80 bytes succeeds.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(bytes.Repeat([]byte("a"), 80)))
	req1.Header.Set("Authorization", "Bearer lo_valid")
	resp1, err := app.Test(req1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusNoContent {
		t.Fatalf("first 80B request status=%d want 204", resp1.StatusCode)
	}

	// Another 80 bytes exceeds remaining 20B budget -> 429 before upstream.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(bytes.Repeat([]byte("b"), 80)))
	req2.Header.Set("Authorization", "Bearer lo_valid")
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests || resp2.Header.Get("Retry-After") != "1" {
		t.Fatalf(
			"second 80B request status=%d retry-after=%q want 429/1",
			resp2.StatusCode,
			resp2.Header.Get("Retry-After"),
		)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstreamHits=%d want 1", upstreamHits.Load())
	}
}

func TestMetricsEndpointModesAndSecurityBoundaries(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	bootstrapPassword := bootstrapGeneratedAdmin(t, store)
	flaggedID := mustUserID(t, store, meta.BootstrapAdminUsername)
	_, flaggedAdminToken, err := store.CreateUserAPIToken(ctx, flaggedID, "metrics-flagged-admin")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	projectToken, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, uploadToken, err := store.CreateSourceMapUploadToken(ctx, project.ID, "metrics-test")
	if err != nil {
		t.Fatal(err)
	}
	viewerCookie := sessionCookie(t, store, "metrics-viewer", "password-123", "viewer")
	memberCookie := sessionCookie(t, store, "metrics-member", "password-123", "member")
	adminCookie := sessionCookie(t, store, "metrics-admin", "password-123", "admin")
	adminID := mustUserID(t, store, "metrics-admin")
	_, adminToken, err := store.CreateUserAPIToken(ctx, adminID, "metrics-admin-tok")
	if err != nil {
		t.Fatal(err)
	}

	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>SPA</html>")}}

	t.Run("disabled returns 404 even with WebUI enabled", func(t *testing.T) {
		gw := newMetadataTestGateway(store, Config{
			Store:      store,
			Tokens:     store,
			Management: management.New(store),
			WebUI:      ui,
		})
		t.Cleanup(func() { _ = gw.Shutdown() })
		resp, err := gw.App().Test(httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("disabled GET /metrics status=%d want 404", resp.StatusCode)
		}
	})

	t.Run("enabled default requires admin session or admin user token", func(t *testing.T) {
		gw := newMetadataTestGateway(store, Config{
			Store:           store,
			Tokens:          store,
			Management:      management.New(store),
			MetricsEndpoint: MetricsEndpointConfig{Enabled: true},
		})
		t.Cleanup(func() { _ = gw.Shutdown() })
		app := gw.App()

		// Log in as bootstrap admin with must_change_password=true.
		loginReq := httptest.NewRequest(
			http.MethodPost,
			"/api/auth/login",
			strings.NewReader(`{"username":"admin","password":"`+bootstrapPassword+`"}`),
		)
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		if err != nil || loginResp.StatusCode != http.StatusOK {
			t.Fatalf("flagged admin login status=%d err=%v", loginResp.StatusCode, err)
		}
		var flaggedCookie *http.Cookie
		for _, c := range loginResp.Cookies() {
			if c.Name == "lo_session" {
				flaggedCookie = c
			}
		}
		_ = loginResp.Body.Close()
		if flaggedCookie == nil {
			t.Fatal("flagged admin login missing lo_session cookie")
		}

		cases := []struct {
			name       string
			customize  func(*http.Request)
			wantStatus int
			wantCode   string
		}{
			{name: "anonymous", wantStatus: http.StatusUnauthorized},
			{
				name:       "project token rejected",
				customize:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+projectToken) },
				wantStatus: http.StatusForbidden,
				wantCode:   "ingest_only_token",
			},
			{
				name:       "upload token rejected",
				customize:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+uploadToken) },
				wantStatus: http.StatusForbidden,
				wantCode:   "upload_only_token",
			},
			{
				name:       "viewer session rejected",
				customize:  func(r *http.Request) { r.AddCookie(viewerCookie) },
				wantStatus: http.StatusForbidden,
			},
			{
				name:       "member session rejected",
				customize:  func(r *http.Request) { r.AddCookie(memberCookie) },
				wantStatus: http.StatusForbidden,
			},
			{
				name:       "must_change_password admin session rejected",
				customize:  func(r *http.Request) { r.AddCookie(flaggedCookie) },
				wantStatus: http.StatusForbidden,
				wantCode:   "password_change_required",
			},
			{
				name:       "must_change_password admin user token rejected",
				customize:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+flaggedAdminToken) },
				wantStatus: http.StatusForbidden,
				wantCode:   "password_change_required",
			},
			{
				name:       "admin session allowed",
				customize:  func(r *http.Request) { r.AddCookie(adminCookie) },
				wantStatus: http.StatusOK,
			},
			{
				name:       "admin user token allowed",
				customize:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) },
				wantStatus: http.StatusOK,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
				if tc.customize != nil {
					tc.customize(req)
				}
				resp, err := app.Test(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.wantStatus, string(body))
				}
				if tc.wantCode != "" {
					var parsed struct {
						Code string `json:"code"`
					}
					if err := json.Unmarshal(body, &parsed); err != nil || parsed.Code != tc.wantCode {
						t.Fatalf("code=%q want=%q body=%s", parsed.Code, tc.wantCode, string(body))
					}
				}
				if tc.wantStatus == http.StatusOK {
					if ct := resp.Header.Get("Content-Type"); ct != selfmetrics.ContentType {
						t.Fatalf("Content-Type=%q want %q", ct, selfmetrics.ContentType)
					}
					parser := expfmt.NewTextParser(model.UTF8Validation)
					if _, err := parser.TextToMetricFamilies(strings.NewReader(string(body))); err != nil {
						t.Fatalf("invalid prometheus body: %v", err)
					}
				}
			})
		}
	})

	t.Run("bearer token takes fail-closed precedence over allow_unauthenticated", func(t *testing.T) {
		gw := newMetadataTestGateway(store, Config{
			Store:      store,
			Tokens:     store,
			Management: management.New(store),
			MetricsEndpoint: MetricsEndpointConfig{
				Enabled:              true,
				BearerToken:          "prom-scrape-secret",
				AllowUnauthenticated: true,
			},
		})
		t.Cleanup(func() { _ = gw.Shutdown() })
		app := gw.App()

		for _, tc := range []struct {
			name      string
			customize func(*http.Request)
			want      int
		}{
			{name: "anonymous", want: http.StatusUnauthorized},
			{
				name:      "wrong token",
				customize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong-secret") },
				want:      http.StatusUnauthorized,
			},
			{
				name:      "project token rejected in bearer mode",
				customize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+projectToken) },
				want:      http.StatusUnauthorized,
			},
			{
				name:      "upload token rejected in bearer mode",
				customize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+uploadToken) },
				want:      http.StatusUnauthorized,
			},
			{
				name:      "admin token rejected in bearer mode",
				customize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) },
				want:      http.StatusUnauthorized,
			},
			{
				name:      "admin session cookie rejected in bearer mode",
				customize: func(r *http.Request) { r.AddCookie(adminCookie) },
				want:      http.StatusUnauthorized,
			},
			{
				name:      "valid bearer token",
				customize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer prom-scrape-secret") },
				want:      http.StatusOK,
			},
		} {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.customize != nil {
				tc.customize(req)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s: status=%d want=%d body=%s", tc.name, resp.StatusCode, tc.want, string(body))
			}
		}
	})

	t.Run("whitespace-only bearer token fails closed even with allow_unauthenticated", func(t *testing.T) {
		gw := newMetadataTestGateway(store, Config{
			Store:      store,
			Tokens:     store,
			Management: management.New(store),
			MetricsEndpoint: MetricsEndpointConfig{
				Enabled:              true,
				BearerToken:          "   ",
				AllowUnauthenticated: true,
			},
		})
		t.Cleanup(func() { _ = gw.Shutdown() })
		app := gw.App()

		for _, tc := range []struct {
			name      string
			customize func(*http.Request)
		}{
			{name: "anonymous"},
			{name: "admin cookie", customize: func(r *http.Request) { r.AddCookie(adminCookie) }},
			{name: "whitespace bearer", customize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer    ") }},
		} {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.customize != nil {
				tc.customize(req)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: status=%d want 401", tc.name, resp.StatusCode)
			}
		}
	})

	t.Run("explicit allow_unauthenticated permits public scrape", func(t *testing.T) {
		gw := newMetadataTestGateway(store, Config{
			Store:      store,
			Tokens:     store,
			Management: management.New(store),
			MetricsEndpoint: MetricsEndpointConfig{
				Enabled:              true,
				AllowUnauthenticated: true,
			},
		})
		t.Cleanup(func() { _ = gw.Shutdown() })
		resp, err := gw.App().Test(httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d want 200 body=%s", resp.StatusCode, string(body))
		}
		parser := expfmt.NewTextParser(model.UTF8Validation)
		if _, err := parser.TextToMetricFamilies(strings.NewReader(string(body))); err != nil {
			t.Fatalf("invalid prometheus body: %v", err)
		}
	})
}

type panickingRoundTripper struct{}

func (panickingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	panic("simulated HTTP ingest handler panic")
}

func TestHTTPIngestPanicRecordsUpstreamErrorMetric(t *testing.T) {
	metricsReg := selfmetrics.New(selfmetrics.Config{})
	gw := newTestGateway(Config{
		Tokens:      &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Traces:      "http://traces.invalid",
		Client:      &http.Client{Transport: panickingRoundTripper{}},
		SelfMetrics: metricsReg,
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer lo_valid")
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("panic status=%d want 500", resp.StatusCode)
	}

	var buf bytes.Buffer
	if err := metricsReg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("parse prometheus output: %v", err)
	}

	var ingestErrCount float64
	for _, m := range families["lazy_observe_ingest_requests_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["protocol"] == "http" && lbls["signal"] == "traces" && lbls["outcome"] == "upstream_error" {
			ingestErrCount = m.GetCounter().GetValue()
		}
	}
	if ingestErrCount != 1 {
		t.Fatalf("http traces upstream_error=%v want 1", ingestErrCount)
	}

	var http5xxCount float64
	for _, m := range families["lazy_observe_http_requests_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["surface"] == "ingest" && lbls["status_class"] == "5xx" {
			http5xxCount = m.GetCounter().GetValue()
		}
	}
	if http5xxCount != 1 {
		t.Fatalf("http ingest 5xx=%v want 1", http5xxCount)
	}
}
