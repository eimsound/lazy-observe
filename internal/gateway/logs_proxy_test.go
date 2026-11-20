package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestLogsRoutesRestrictMethodsAndRespectDisableHeadAutoRegister(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
	}).App()

	paths := []string{
		"/api/v1/logs/query",
		"/api/v1/logs/hits",
		"/api/v1/logs/stats_query_range",
		"/api/v1/logs/query_time_range",
		"/api/v1/logs/field_names",
		"/api/v1/logs/field_values",
		"/api/v1/logs/stream_field_names",
		"/api/v1/logs/stream_field_values",
		"/api/v1/logs/tail",
	}
	for _, p := range paths {
		for _, method := range []string{http.MethodHead, http.MethodDelete, http.MethodPut} {
			req := httptest.NewRequest(method, p+"?query=*", nil)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, p, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s %s unexpectedly succeeded with 200", method, p)
			}
		}
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("disallowed methods on /api/v1/logs/* reached upstream %d times", hits)
	}
}

func TestProxyAllowlistsRequestAndResponseHeaders(t *testing.T) {
	captured := make(chan http.Header, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "evil=1; Path=/")
		w.Header().Set("Access-Control-Allow-Origin", "https://evil.example")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("X-Upstream-Secret", "leak")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
		Traces: upstream.URL,
	}).App()

	req := httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer lou_valid")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("VL-Extra-Fields", "injected=1")
	req.Header.Set("VL-Stream-Fields", "injected")
	req.Header.Set("VL-Time-Field", "fake_time")
	req.Header.Set("VL-Ignore-Fields", "_msg")
	req.Header.Set("VL-Debug", "1")
	req.Header.Set("X-Custom-Untrusted", "drop")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want=200", resp.StatusCode)
	}

	gotReq := <-captured
	for _, forbidden := range []string{
		"VL-Extra-Fields",
		"VL-Stream-Fields",
		"VL-Time-Field",
		"VL-Ignore-Fields",
		"VL-Debug",
		"X-Custom-Untrusted",
		"Authorization",
	} {
		if v := gotReq.Get(forbidden); v != "" {
			t.Errorf("request header %s leaked to upstream: %q", forbidden, v)
		}
	}
	if gotReq.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q want application/json", gotReq.Get("Content-Type"))
	}
	for _, forbidden := range []string{
		"Set-Cookie",
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"X-Upstream-Secret",
	} {
		if v := resp.Header.Get(forbidden); v != "" {
			t.Errorf("response header %s leaked from upstream: %q", forbidden, v)
		}
	}

	// Browser ingest /v1/traces must preserve gateway-owned CORS headers while dropping upstream CORS overrides.
	traceReq := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(`{}`))
	traceReq.Header.Set("Origin", "https://app.example")
	traceReq.Header.Set("Authorization", "Bearer lou_valid")
	traceReq.Header.Set("Content-Type", "application/json")
	traceResp, err := app.Test(traceReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = traceResp.Body.Close()
	<-captured
	if got := traceResp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("/v1/traces Access-Control-Allow-Origin=%q want *", got)
	}
	if got := traceResp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("/v1/traces leaked upstream Access-Control-Allow-Credentials=%q", got)
	}
}

func TestProxyDoesNotFollowRedirectsAndSanitizes502(t *testing.T) {
	var redirectTargetHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
	}).App()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d want=302", resp.StatusCode)
	}
	if redirectTargetHits.Load() != 0 {
		t.Fatalf("proxy followed 3xx redirect %d times", redirectTargetHits.Load())
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("proxy leaked upstream Location header: %q", loc)
	}

	// Shut down upstream so proxy returns 502; body must not echo internal address.
	upstream.Close()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req2.Header.Set("Authorization", "Bearer lou_valid")
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2 := string(readBody(resp2))
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d want=502 body=%s", resp2.StatusCode, body2)
	}
	if strings.Contains(body2, "127.0.0.1") || strings.Contains(body2, "dial tcp") ||
		strings.Contains(body2, "connection refused") {
		t.Fatalf("502 response leaked internal upstream error: %q", body2)
	}
}

func TestTenantProxyFailsClosedWhenProjectIDIsNonPositive(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	for _, id := range []int{0, -1} {
		app := newTestGateway(Config{
			Tokens: &fakeTokens{project: meta.ProjectInfo{ID: id, Slug: "broken"}},
			Logs:   upstream.URL,
			Traces: upstream.URL,
		}).App()
		for _, target := range []string{
			"/api/v1/logs/query?query=*",
			"/api/v1/logs/tail?query=*",
			"/api/traces/services",
			"/api/traces/search?service=api",
		} {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("id=%d %s: %v", id, target, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("id=%d %s status=%d want=500", id, target, resp.StatusCode)
			}
		}
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("non-positive project ID reached upstream %d times", upstreamHits.Load())
	}
}

func TestProxyNoContentAndStreamErrorAccounting(t *testing.T) {
	t.Run("204_and_304_skip_stream_writer", func(t *testing.T) {
		for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			app := newTestGateway(Config{
				Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
				Logs:   upstream.URL,
			}).App()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			upstream.Close()
			if err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != code {
				t.Fatalf("status=%d want=%d", resp.StatusCode, code)
			}
		}
	})

	t.Run("stream_copy_error_increments_counter", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not support hijacking")
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			// Advertise 100 bytes but close immediately after 5 bytes to force unexpected EOF.
			_, _ = rw.WriteString(
				"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\nshort",
			)
			_ = rw.Flush()
			_ = conn.Close()
		}))
		defer upstream.Close()

		gw := newTestGateway(Config{
			Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Logs:   upstream.URL,
		})
		req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
		req.Header.Set("Authorization", "Bearer lou_valid")
		resp, err := gw.App().Test(req)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		if gw.proxyStreamErrors.Load() < 1 {
			t.Fatalf("proxyStreamErrors=%d want >= 1", gw.proxyStreamErrors.Load())
		}
	})
}
