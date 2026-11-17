package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
)

func TestPerformanceEndpointsUsesHistogramMetricsAndPreservesContract(t *testing.T) {
	start := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query := r.URL.Query()
		if query.Get("extra_filters[]") != `{project_id="alpha"}` {
			t.Errorf("project filter=%q", query.Get("extra_filters[]"))
		}
		expression := query.Get("query")
		if strings.Contains(expression, "span") || !strings.Contains(expression, "[120s]") {
			t.Errorf("query=%q", expression)
		}

		resultType := "vector"
		switch r.URL.Path {
		case "/api/v1/query_range":
			resultType = "matrix"
			if query.Get("start") != start.Format(time.RFC3339Nano) ||
				query.Get("end") != end.Format(time.RFC3339Nano) ||
				query.Get("step") != "60" {
				t.Errorf("range query=%v", query)
			}
		case "/api/v1/query":
			if query.Get("time") != end.Format(time.RFC3339Nano) {
				t.Errorf("instant query=%v", query)
			}
		default:
			t.Errorf("path=%q", r.URL.Path)
		}

		var result []map[string]any
		switch {
		case strings.Contains(expression, "histogram_quantiles"):
			if !strings.Contains(expression, performanceDurationBucketMetric) {
				t.Errorf("quantiles query=%q", expression)
			}
			result = performanceQuantileFixture(resultType, start)
		case strings.Contains(expression, `http\.response\.status_code=~"5.."`):
			if !strings.Contains(expression, " or ") {
				t.Errorf("error rate query must preserve zero-error endpoints: %q", expression)
			}
			result = performanceScalarFixture(resultType, start, 2.0/3.0, 0.5)
		default:
			if !strings.Contains(expression, performanceDurationCountMetric) {
				t.Errorf("throughput query=%q", expression)
			}
			result = performanceScalarFixture(resultType, start, 3.0/120.0, 2.0/60.0)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": resultType,
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
	target := "/api/v1/performance/endpoints?" + url.Values{
		"start": {fmt.Sprint(start.Unix())},
		"end":   {fmt.Sprint(end.Unix())},
		"step":  {"60"},
	}.Encode()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if requests.Load() != 6 {
		t.Fatalf("metrics requests=%d want=6", requests.Load())
	}

	var got performanceResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Start != start.UnixMilli() || got.End != end.UnixMilli() || got.StepSeconds != 60 || got.Truncated ||
		len(got.Endpoints) != 2 {
		t.Fatalf("response=%s", body)
	}
	checkout := got.Endpoints[0]
	if checkout.Service != "checkout" || checkout.Method != "POST" || checkout.Route != "/orders/:id" ||
		!closeFloat(checkout.Throughput, 3.0/120.0) || checkout.P50 != 30 || checkout.P95 != 48 ||
		!closeFloat(checkout.P99, 49.6) || !closeFloat(checkout.ErrorRate, 2.0/3.0) ||
		len(checkout.Series) != 2 {
		t.Fatalf("checkout=%#v", checkout)
	}
	if checkout.Series[0].Time != start.UnixMilli() ||
		!closeFloat(checkout.Series[0].Throughput, 2.0/60.0) ||
		checkout.Series[0].P50 != 20 ||
		checkout.Series[0].P95 != 29 ||
		checkout.Series[0].P99 != 30 ||
		checkout.Series[0].ErrorRate != 0.5 {
		t.Fatalf("first point=%#v", checkout.Series[0])
	}
}

func performanceQuantileFixture(resultType string, start time.Time) []map[string]any {
	quantiles := []struct {
		label   string
		summary float64
		first   float64
		second  float64
	}{
		{label: "0.5", summary: 0.030, first: 0.020, second: 0.050},
		{label: "0.95", summary: 0.048, first: 0.029, second: 0.050},
		{label: "0.99", summary: 0.0496, first: 0.030, second: 0.050},
	}
	result := make([]map[string]any, 0, len(quantiles)+1)
	for _, quantile := range quantiles {
		metric := map[string]string{
			"service.name":        "checkout",
			"http.request.method": "POST",
			"http.route":          "/orders/:id",
			"quantile":            quantile.label,
		}
		item := map[string]any{"metric": metric}
		if resultType == "matrix" {
			item["values"] = [][]any{
				{start.Unix(), fmt.Sprint(quantile.first)},
				{start.Add(time.Minute).Unix(), fmt.Sprint(quantile.second)},
			}
		} else {
			item["value"] = []any{start.Add(2 * time.Minute).Unix(), fmt.Sprint(quantile.summary)}
		}
		result = append(result, item)
	}
	if resultType == "vector" {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"service.name":        "gateway",
				"http.request.method": "GET",
				"http.route":          "/healthz",
				"quantile":            "0.95",
			},
			"value": []any{start.Add(2 * time.Minute).Unix(), "0.005"},
		})
	}
	return result
}

func performanceScalarFixture(
	resultType string,
	start time.Time,
	summary float64,
	first float64,
) []map[string]any {
	checkout := map[string]any{
		"metric": map[string]string{
			"service.name":        "checkout",
			"http.request.method": "POST",
			"http.route":          "/orders/:id",
		},
	}
	if resultType == "matrix" {
		checkout["values"] = [][]any{
			{start.Unix(), fmt.Sprint(first)},
			{start.Add(time.Minute).Unix(), fmt.Sprint(first / 2)},
		}
		return []map[string]any{checkout}
	}
	checkout["value"] = []any{start.Add(2 * time.Minute).Unix(), fmt.Sprint(summary)}
	gateway := map[string]any{
		"metric": map[string]string{
			"service.name":        "gateway",
			"http.request.method": "GET",
			"http.route":          "/healthz",
		},
		"value": []any{start.Add(2 * time.Minute).Unix(), "0"},
	}
	return []map[string]any{checkout, gateway}
}

func TestPerformanceTrendRateWindow(t *testing.T) {
	for _, test := range []struct {
		window time.Duration
		step   time.Duration
		want   time.Duration
	}{
		{window: time.Hour, step: 5 * time.Second, want: time.Minute},
		{window: time.Hour, step: 5 * time.Minute, want: 20 * time.Minute},
		{window: 2 * time.Minute, step: time.Minute, want: 2 * time.Minute},
	} {
		if got := performanceTrendRateWindow(test.window, test.step); got != test.want {
			t.Errorf("window=%s step=%s got=%s want=%s", test.window, test.step, got, test.want)
		}
	}
}

func TestPerformancePrometheusQuantilesQuerySupportsProjectIsolation(t *testing.T) {
	queries := []string{
		performanceQuantilesQuery(time.Minute, storagepkg.MetricsPrometheus),
		performanceThroughputQuery(time.Minute, storagepkg.MetricsPrometheus),
		performanceErrorRateQuery(time.Minute, storagepkg.MetricsPrometheus),
	}
	for _, query := range queries {
		isolated, err := storagepkg.InjectPromQL(query, "alpha")
		if err != nil {
			t.Fatalf("query=%q: %v", query, err)
		}
		if !strings.Contains(isolated, `project_id="alpha"`) {
			t.Fatalf("isolated query=%q", isolated)
		}
	}
	if query := queries[0]; strings.Contains(query, "histogram_quantiles") ||
		strings.Count(query, "histogram_quantile") != 3 {
		t.Fatalf("quantiles query=%q", query)
	}
}

func TestPerformanceEndpointsRejectInvalidWindowAndMalformedBackend(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	badBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not-json\n")
	}))
	defer badBackend.Close()
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics: badBackend.URL,
		Now:     func() time.Time { return now },
	}).App()

	for _, test := range []struct {
		target string
		want   int
	}{
		{"/api/v1/performance/endpoints?start=bad", http.StatusBadRequest},
		{
			"/api/v1/performance/endpoints?start=" + fmt.Sprint(now.Unix()) +
				"&end=" + fmt.Sprint(now.Add(-time.Minute).Unix()),
			http.StatusBadRequest,
		},
		{"/api/v1/performance/endpoints?step=0", http.StatusBadRequest},
		{"/api/v1/performance/endpoints?step=1", http.StatusBadRequest},
		{"/api/v1/performance/endpoints", http.StatusBadGateway},
	} {
		req := httptest.NewRequest(http.MethodGet, test.target, nil)
		req.Header.Set("Authorization", "Bearer "+"lou_valid")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != test.want {
			t.Errorf("%s status=%d want=%d", test.target, resp.StatusCode, test.want)
		}
	}
}

func closeFloat(got, want float64) bool {
	return math.Abs(got-want) < 1e-12
}
