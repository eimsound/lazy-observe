package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
)

func newVitalsIngestGatewayWithProxies(t *testing.T, slug string, trustedProxies []string) metricIngestSetup {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	t.Cleanup(upstream.Close)
	logs := &strings.Builder{}
	gw := newMetadataTestGateway(store, Config{
		Tokens:         store,
		Store:          store,
		Metrics:        upstream.URL,
		TrustedProxies: trustedProxies,
		Logger:         slog.New(slog.NewTextHandler(logs, nil)),
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	return metricIngestSetup{gateway: gw, token: token, storage: storage, logs: logs}
}

func postVitalsWithHeaders(
	t *testing.T,
	setup metricIngestSetup,
	vitals []any,
	headers map[string]string,
) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"vitals": vitals})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/events/vitals", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+setup.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := setup.gateway.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, payload
}

func TestVitalsIngestScrubsHighCardinalityAndInvalidDimensions(t *testing.T) {
	setup := newVitalsIngestGatewayWithProxies(t, "vitals-dim-scrub", nil)
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	status, payload := postVitalsWithHeaders(t, setup, []any{
		map[string]any{
			"name":           "LCP",
			"value":          1850,
			"page":           "/checkout",
			"timestamp":      at.Format(time.RFC3339Nano),
			"device_type":    "iPhone 15 Pro Max; evil=1",
			"browser_family": "Mozilla/5.0 Chrome/126.0.6478.127",
			"os_family":      "Windows 11 Build 26100",
			"country":        "San Francisco, CA",
		},
		map[string]any{
			"name":           "CLS",
			"value":          0.08,
			"page":           "/checkout",
			"timestamp":      at.Format(time.RFC3339Nano),
			"device_type":    "Mobile",
			"browser_family": "Safari",
			"os_family":      "iOS",
			"country":        "cn",
		},
	}, map[string]string{
		"X-Forwarded-For": "203.0.113.77",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}

	samples := setup.samples()
	wantScrubbed := `web_vitals_lcp{project_id="vitals-dim-scrub",page="/checkout",device_type="unknown",browser_family="other",os_family="other",country="unknown"} 1850`
	if !strings.Contains(samples, wantScrubbed) {
		t.Fatalf("scrubbed sample missing:\nwant: %s\ngot:  %s", wantScrubbed, samples)
	}
	wantValid := `web_vitals_cls{project_id="vitals-dim-scrub",page="/checkout",device_type="mobile",browser_family="safari",os_family="ios",country="CN"} 0.08`
	if !strings.Contains(samples, wantValid) {
		t.Fatalf("valid sample missing:\nwant: %s\ngot:  %s", wantValid, samples)
	}
	for _, forbidden := range []string{"iPhone", "Mozilla", "Build 26100", "San Francisco", "203.0.113.77"} {
		if strings.Contains(samples, forbidden) {
			t.Fatalf("high-cardinality or PII value %q leaked into metrics labels:\n%s", forbidden, samples)
		}
	}
}

func TestVitalsIngestFallsBackToNormalizedUserAgentWhenOmitted(t *testing.T) {
	setup := newVitalsIngestGatewayWithProxies(t, "vitals-ua-fallback", nil)
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		ua          string
		wantDevice  string
		wantBrowser string
		wantOS      string
	}{
		{
			name:        "windows edge desktop",
			ua:          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
			wantDevice:  "desktop",
			wantBrowser: "edge",
			wantOS:      "windows",
		},
		{
			name:        "android mobile chrome",
			ua:          "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/126.0.0.0 Mobile Safari/537.36",
			wantDevice:  "mobile",
			wantBrowser: "chrome",
			wantOS:      "android",
		},
		{
			name:        "ipad tablet safari",
			ua:          "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 Version/17.5 Safari/605.1.15",
			wantDevice:  "tablet",
			wantBrowser: "safari",
			wantOS:      "ios",
		},
		{
			name:        "linux firefox desktop",
			ua:          "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
			wantDevice:  "desktop",
			wantBrowser: "firefox",
			wantOS:      "linux",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, payload := postVitalsWithHeaders(t, setup, []any{
				vitalPayload("LCP", 1200, "/home", at),
			}, map[string]string{"User-Agent": tc.ua})
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, payload)
			}
			want := fmt.Sprintf(
				`web_vitals_lcp{project_id="vitals-ua-fallback",page="/home",device_type=%q,browser_family=%q,os_family=%q,country="unknown"} 1200`,
				tc.wantDevice,
				tc.wantBrowser,
				tc.wantOS,
			)
			if !strings.Contains(setup.samples(), want) {
				t.Fatalf("samples missing %q:\n%s", want, setup.samples())
			}
		})
	}
}

func TestVitalsIngestCountryTrustsEdgeHeadersOnlyFromTrustedProxies(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	t.Run("ignores spoofed proxy country headers when TrustedProxies is empty", func(t *testing.T) {
		setup := newVitalsIngestGatewayWithProxies(t, "vitals-untrusted", nil)
		status, payload := postVitalsWithHeaders(t, setup, []any{
			vitalPayload("LCP", 1500, "/checkout", at),
		}, map[string]string{
			"CF-IPCountry":              "US",
			"CloudFront-Viewer-Country": "DE",
			"X-Vercel-IP-Country":       "JP",
			"X-Country-Code":            "GB",
			"X-Forwarded-For":           "198.51.100.42",
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, payload)
		}
		samples := setup.samples()
		if !strings.Contains(samples, `country="unknown"`) {
			t.Fatalf("spoofed proxy headers must be ignored, got:\n%s", samples)
		}
		if strings.Contains(samples, "198.51.100.42") {
			t.Fatalf("client IP leaked into samples:\n%s", samples)
		}
	})

	t.Run("extracts country from trusted proxy headers in precedence order and rejects XX/T1", func(t *testing.T) {
		// Fiber app.Test uses remote peer 0.0.0.0.
		setup := newVitalsIngestGatewayWithProxies(t, "vitals-trusted", []string{"0.0.0.0/32", "127.0.0.0/8"})

		// Trusted CF-IPCountry overrides SDK-provided country.
		status, payload := postVitalsWithHeaders(t, setup, []any{
			map[string]any{
				"name":      "LCP",
				"value":     1400,
				"page":      "/a",
				"timestamp": at.Format(time.RFC3339Nano),
				"country":   "FR",
			},
		}, map[string]string{
			"CF-IPCountry":              "jp",
			"CloudFront-Viewer-Country": "DE",
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, payload)
		}
		if !strings.Contains(
			setup.samples(),
			`page="/a",device_type="unknown",browser_family="other",os_family="other",country="JP"`,
		) {
			t.Fatalf("expected trusted CF-IPCountry JP, got:\n%s", setup.samples())
		}

		// Reserved unknown code (XX/T1) from trusted proxy becomes unknown and is not
		// overridden by either client JSON or lower-precedence spoofed proxy headers.
		status, payload = postVitalsWithHeaders(t, setup, []any{
			map[string]any{
				"name":      "LCP",
				"value":     1600,
				"page":      "/b",
				"timestamp": at.Format(time.RFC3339Nano),
				"country":   "US",
			},
		}, map[string]string{
			"CF-IPCountry":   "T1",
			"X-Country-Code": "US",
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, payload)
		}
		if !strings.Contains(
			setup.samples(),
			`page="/b",device_type="unknown",browser_family="other",os_family="other",country="unknown"`,
		) {
			t.Fatalf("expected T1 with spoofed X-Country-Code to normalize to unknown, got:\n%s", setup.samples())
		}

		// Trusted proxy with no geo headers falls back to valid SDK country.
		status, payload = postVitalsWithHeaders(t, setup, []any{
			map[string]any{
				"name":      "LCP",
				"value":     1700,
				"page":      "/c",
				"timestamp": at.Format(time.RFC3339Nano),
				"country":   "sg",
			},
		}, nil)
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, payload)
		}
		if !strings.Contains(
			setup.samples(),
			`page="/c",device_type="unknown",browser_family="other",os_family="other",country="SG"`,
		) {
			t.Fatalf("expected SDK fallback SG, got:\n%s", setup.samples())
		}
	})
}

func TestPerformanceVitalsBreakdownQueryAndFilters(t *testing.T) {
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	var (
		mu       sync.Mutex
		queries  []string
		isoCount int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path=%q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("extra_filters[]") != `{project_id="alpha"}` {
			t.Errorf("extra_filters[]=%q want alpha", q.Get("extra_filters[]"))
		}
		expr := q.Get("query")
		mu.Lock()
		queries = append(queries, expr)
		isoCount++
		mu.Unlock()

		var result []map[string]any
		switch {
		case strings.Contains(expr, "avg_over_time(web_vitals_lcp"):
			result = []map[string]any{
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "mobile",
						"browser_family": "safari",
						"os_family":      "ios",
						"country":        "US",
					},
					"value": []any{end.Unix(), "3000"},
				},
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "desktop",
						"browser_family": "chrome",
						"os_family":      "macos",
						"country":        "CN",
					},
					"value": []any{end.Unix(), "1500"},
				},
				{
					// Legacy series lacking dimension labels must normalize to unknown/other.
					"metric": map[string]string{
						"page": "/home",
					},
					"value": []any{end.Unix(), "2000"},
				},
			}
		case strings.Contains(expr, "count_over_time(web_vitals_lcp"):
			result = []map[string]any{
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "mobile",
						"browser_family": "safari",
						"os_family":      "ios",
						"country":        "US",
					},
					"value": []any{end.Unix(), "3"},
				},
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "desktop",
						"browser_family": "chrome",
						"os_family":      "macos",
						"country":        "CN",
					},
					"value": []any{end.Unix(), "1"},
				},
				{
					"metric": map[string]string{
						"page": "/home",
					},
					"value": []any{end.Unix(), "1"},
				},
			}
		case strings.Contains(expr, "avg_over_time(web_vitals_cls"):
			result = []map[string]any{
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "mobile",
						"browser_family": "safari",
						"os_family":      "ios",
						"country":        "US",
					},
					"value": []any{end.Unix(), "0.18"},
				},
			}
		case strings.Contains(expr, "count_over_time(web_vitals_cls"):
			result = []map[string]any{
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "mobile",
						"browser_family": "safari",
						"os_family":      "ios",
						"country":        "US",
					},
					"value": []any{end.Unix(), "2"},
				},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     result,
			},
		})
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Metrics: upstream.URL,
		Now:     func() time.Time { return end },
	}).App()

	target := "/api/v1/performance/vitals?" + url.Values{
		"start": {fmt.Sprint(start.Unix())},
		"end":   {fmt.Sprint(end.Unix())},
		"step":  {"60"},
	}.Encode()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var got performanceVitalsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	// Weighted LCP mean: (3000*3 + 1500*1 + 2000*1) / 5 = 12500 / 5 = 2500.
	if got.Summary.LCP == nil || math.Abs(*got.Summary.LCP-2500) > 1e-6 {
		t.Fatalf("summary.lcp=%v want 2500", got.Summary.LCP)
	}
	if got.Summary.CLS == nil || math.Abs(*got.Summary.CLS-0.18) > 1e-6 {
		t.Fatalf("summary.cls=%v want 0.18", got.Summary.CLS)
	}
	if got.TotalSamples != 7 {
		t.Fatalf("total_samples=%d want 7", got.TotalSamples)
	}
	if len(got.ByDevice) != 3 || got.ByDevice[0].Value != "mobile" || got.ByDevice[0].Samples != 5 {
		t.Fatalf("by_device=%#v", got.ByDevice)
	}
	if len(got.ByCountry) != 3 || got.ByCountry[0].Value != "US" || got.ByCountry[0].Samples != 5 {
		t.Fatalf("by_country=%#v", got.ByCountry)
	}
	if len(got.ByBrowser) != 3 || len(got.ByOS) != 3 || len(got.ByPage) != 2 {
		t.Fatalf("breakdowns sizes: browser=%d os=%d page=%d", len(got.ByBrowser), len(got.ByOS), len(got.ByPage))
	}
	// Legacy /home series normalized into unknown/other.
	foundUnknownCountry := false
	for _, row := range got.ByCountry {
		if row.Value == "unknown" && row.Samples == 1 && row.Values.LCP != nil && *row.Values.LCP == 2000 {
			foundUnknownCountry = true
		}
	}
	if !foundUnknownCountry {
		t.Fatalf("legacy series missing from unknown country bucket: %#v", got.ByCountry)
	}
	mu.Lock()
	if isoCount != 10 {
		t.Fatalf("expected 10 vector queries, got %d", isoCount)
	}
	mu.Unlock()
}

func TestPerformanceVitalsFilterValidationAndPrometheusIsolation(t *testing.T) {
	end := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)

	var (
		mu       sync.Mutex
		captured []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		captured = append(captured, r.URL.Query().Get("query"))
		mu.Unlock()
		if r.URL.Query().Has("extra_filters[]") {
			t.Errorf("prometheus backend should inject project_id into PromQL instead of extra_filters[]")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     []any{},
			},
		})
	}))
	defer upstream.Close()

	backend, err := storagepkg.New(
		storagepkg.MetricsPrometheus,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "beta"}},
		Storage: backend,
		Now:     func() time.Time { return end },
	}).App()

	// Invalid filters must fail closed with 400 before touching storage.
	for _, badQuery := range []string{
		"device_type=smartwatch",
		"browser_family=ie6",
		"os_family=solaris",
		"country=USA",
		"country=XX",
		"country=T1",
		"page=/bad%00path",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/performance/vitals?"+badQuery, nil)
		req.Header.Set("Authorization", "Bearer lou_valid")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q status=%d want 400", badQuery, resp.StatusCode)
		}
	}

	// Valid filters inject project_id="beta" and match unknown/other via regex for legacy series.
	target := "/api/v1/performance/vitals?" + url.Values{
		"start":          {fmt.Sprint(start.Unix())},
		"end":            {fmt.Sprint(end.Unix())},
		"page":           {"/checkout"},
		"device_type":    {"mobile"},
		"browser_family": {"other"},
		"country":        {"unknown"},
	}.Encode()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 10 {
		t.Fatalf("captured queries=%d want 10", len(captured))
	}
	for _, expr := range captured {
		if !strings.Contains(expr, `project_id="beta"`) {
			t.Fatalf("missing project isolation in PromQL: %q", expr)
		}
		if !strings.Contains(expr, `page="/checkout"`) ||
			!strings.Contains(expr, `device_type="mobile"`) ||
			!strings.Contains(expr, `browser_family=~"|other"`) ||
			!strings.Contains(expr, `country=~"|unknown"`) {
			t.Fatalf("missing expected dimension filters in PromQL: %q", expr)
		}
	}
}

func TestPerformanceVitalsMergesLegacyAndExplicitUnknownSeriesWithoutDoubleCounting(t *testing.T) {
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		var result []map[string]any
		switch {
		case strings.Contains(expr, "avg_over_time(web_vitals_lcp"):
			result = []map[string]any{
				{
					"metric": map[string]string{
						"page": "/checkout",
					},
					"value": []any{end.Unix(), "1000"},
				},
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "unknown",
						"browser_family": "other",
						"os_family":      "other",
						"country":        "unknown",
					},
					"value": []any{end.Unix(), "3000"},
				},
			}
		case strings.Contains(expr, "count_over_time(web_vitals_lcp"):
			result = []map[string]any{
				{
					"metric": map[string]string{
						"page": "/checkout",
					},
					"value": []any{end.Unix(), "1"},
				},
				{
					"metric": map[string]string{
						"page":           "/checkout",
						"device_type":    "unknown",
						"browser_family": "other",
						"os_family":      "other",
						"country":        "unknown",
					},
					"value": []any{end.Unix(), "9"},
				},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     result,
			},
		})
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Metrics: upstream.URL,
		Now:     func() time.Time { return end },
	}).App()

	target := "/api/v1/performance/vitals?" + url.Values{
		"start": {fmt.Sprint(start.Unix())},
		"end":   {fmt.Sprint(end.Unix())},
		"step":  {"60"},
	}.Encode()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var got performanceVitalsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	// Weighted mean must be (1000*1 + 3000*9) / (1 + 9) = 2800, total_samples = 10.
	if got.Summary.LCP == nil || math.Abs(*got.Summary.LCP-2800) > 1e-6 {
		t.Fatalf("summary.lcp=%v want 2800", got.Summary.LCP)
	}
	if got.TotalSamples != 10 {
		t.Fatalf("total_samples=%d want 10", got.TotalSamples)
	}
	if len(got.ByPage) != 1 || got.ByPage[0].Samples != 10 ||
		got.ByPage[0].Values.LCP == nil || math.Abs(*got.ByPage[0].Values.LCP-2800) > 1e-6 {
		t.Fatalf("by_page=%#v want 1 row with samples=10 lcp=2800", got.ByPage)
	}
	if len(got.ByDevice) != 1 || got.ByDevice[0].Value != "unknown" || got.ByDevice[0].Samples != 10 ||
		got.ByDevice[0].Values.LCP == nil || math.Abs(*got.ByDevice[0].Values.LCP-2800) > 1e-6 {
		t.Fatalf("by_device=%#v want unknown samples=10 lcp=2800", got.ByDevice)
	}
	if len(got.ByCountry) != 1 || got.ByCountry[0].Value != "unknown" || got.ByCountry[0].Samples != 10 {
		t.Fatalf("by_country=%#v want unknown samples=10", got.ByCountry)
	}
}

func TestPerformanceVitalsUpstreamErrorIsSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `secret internal http://10.0.0.9:8428 failure`)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 1, Slug: "alpha"}},
		Metrics: upstream.URL,
	}).App()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/performance/vitals", nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "10.0.0.9") || strings.Contains(string(body), upstream.URL) {
		t.Fatalf("upstream address leaked in 502 body: %s", body)
	}
}
