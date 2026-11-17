package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

const vitalPagesSelector = `{__name__=~"web_vitals_(lcp|inp|cls|ttfb|fcp)"}`

func TestPerformanceVitalPageDiscoveryUsesIsolatedSeriesAPI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/series" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("match[]") != vitalPagesSelector || query.Get("start") != "1784368800" ||
			query.Get("end") != "1784368920" {
			t.Errorf("query=%v", query)
		}
		if filters := query["extra_filters[]"]; len(filters) != 1 || filters[0] != `{project_id="alpha"}` {
			t.Errorf("trusted project filters=%#v", filters)
		}

		switch query.Get("scenario") {
		case "empty":
			_, _ = io.WriteString(w, `{"status":"success","data":[]}`)
		case "error":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"error","error":"metrics unavailable"}`)
		default:
			_, _ = io.WriteString(
				w,
				`{"status":"success","data":[{"__name__":"web_vitals_lcp","project_id":"alpha","page":"/checkout"}]}`,
			)
		}
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Metrics: upstream.URL,
	}).App()
	request := func(t *testing.T, scenario string, wantStatus int) map[string]any {
		t.Helper()
		params := url.Values{
			"match[]":         {vitalPagesSelector},
			"start":           {"1784368800"},
			"end":             {"1784368920"},
			"extra_filters[]": {`{project_id="attacker"}`},
		}
		if scenario != "" {
			params.Set("scenario", scenario)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/series?"+params.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+"lou_valid")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != wantStatus {
			t.Fatalf("scenario=%q status=%d body=%s", scenario, resp.StatusCode, body)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("scenario=%q decode body %q: %v", scenario, body, err)
		}
		return decoded
	}

	if got := request(t, "", http.StatusOK); got["status"] != "success" || len(got["data"].([]any)) != 1 {
		t.Fatalf("series response=%#v", got)
	}
	if got := request(t, "empty", http.StatusOK); len(got["data"].([]any)) != 0 {
		t.Fatalf("empty series response=%#v", got)
	}
	if got := request(t, "error", http.StatusServiceUnavailable); got["error"] != "metrics unavailable" {
		t.Fatalf("error series response=%#v", got)
	}
}

func TestPerformanceVitalPageDiscoveryInjectsPrometheusMatcher(t *testing.T) {
	captured := make(chan url.Values, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.URL.Query()
		_, _ = io.WriteString(w, `{"status":"success","data":[]}`)
	}))
	defer upstream.Close()
	backend, err := storage.New(storage.MetricsPrometheus, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Storage: backend,
	}).App()
	params := url.Values{"match[]": {vitalPagesSelector}, "extra_filters[]": {`{project_id="attacker"}`}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/series?"+params.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	want, err := storage.InjectPromQL(vitalPagesSelector, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	query := <-captured
	if query.Get("match[]") != want || query.Has("extra_filters[]") {
		t.Fatalf("isolated Prometheus series query=%v want matcher=%q", query, want)
	}
}
