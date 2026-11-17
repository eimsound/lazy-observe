package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/llmcost"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestExtractLLMCalls(t *testing.T) {
	pricing := llmcost.Default()
	body := []byte(
		`{"_time":"2026-07-17T10:00:00Z","resource_attr:service.name":"checkout","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.request.model":"gpt-4o","span_attr:gen_ai.response.model":"gpt-4o-mini-2024-07-18","span_attr:gen_ai.usage.input_tokens":"2000","span_attr:gen_ai.usage.output_tokens":"500","duration":"125000000","trace_id":"abc","status_code":"2"}
{"_time":"2026-07-17T10:01:00Z","resource_attr:service.name":"agent","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.request.model":"unknown-model","span_attr:gen_ai.usage.input_tokens":10,"duration":"1000","trace_id":"def","status_code":"1"}
{"_time":"2026-07-17T10:02:00Z","span_attr:gen_ai.operation.name":"chat","span_attr:error.type":"timeout","duration":"1000","trace_id":"fed","status_code":"0"}
`,
	)
	calls, err := extractLLMCalls(body, pricing)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls=%d", len(calls))
	}
	first := calls[0]
	if first.Service != "checkout" || first.Model != "gpt-4o-mini-2024-07-18" || first.Operation != "chat" ||
		first.Duration != 125000 ||
		first.Status != "error" ||
		first.Unpriced {
		t.Fatalf("first call=%#v", first)
	}
	if math.Abs(first.Cost-0.0006) > 1e-12 {
		t.Fatalf("cost=%v", first.Cost)
	}
	if !calls[1].Unpriced || calls[1].Cost != 0 || calls[1].Status != "ok" {
		t.Fatalf("unknown call=%#v", calls[1])
	}
	if calls[2].Service != "unknown" || calls[2].Model != "unknown" || calls[2].Status != "error" {
		t.Fatalf("current semantic convention error call=%#v", calls[2])
	}
}

func TestLLMTimeseriesBucketsAndTenantQuery(t *testing.T) {
	start := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" || r.Header.Get("AccountID") != "0" ||
			r.Header.Get("ProjectID") != "42" {
			t.Errorf(
				"isolated query path=%s account=%s project=%s",
				r.URL.Path,
				r.Header.Get("AccountID"),
				r.Header.Get("ProjectID"),
			)
		}
		if r.URL.Query().Get("query") != `"span_attr:gen_ai.operation.name":*` || r.URL.Query().Get("start") == "" ||
			r.URL.Query().Get("end") == "" {
			t.Errorf("query=%v", r.URL.Query())
		}
		for _, row := range []struct {
			at     time.Time
			input  int
			output int
		}{{start.Add(time.Minute), 10, 2}, {start.Add(6 * time.Minute), 20, 3}} {
			_, _ = fmt.Fprintf(
				w,
				`{"_time":%q,"resource_attr:service.name":"svc","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.request.model":"gpt-4o-mini","span_attr:gen_ai.usage.input_tokens":%q,"span_attr:gen_ai.usage.output_tokens":%q,"duration":"1000","trace_id":"trace","status_code":"1"}`+"\n",
				row.at.Format(time.RFC3339Nano),
				fmt.Sprint(row.input),
				fmt.Sprint(row.output),
			)
		}
	}))
	defer upstream.Close()
	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}}, Traces: upstream.URL,
		Now: func() time.Time { return start.Add(10 * time.Minute) },
	}).App()
	target := "/api/llm/timeseries?" + url.Values{
		"start": {start.Format(time.RFC3339)}, "end": {start.Add(10 * time.Minute).Format(time.RFC3339)},
		"step": {"5m"}, "metric": {"tokens"},
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
	var result struct {
		Metric string               `json:"metric"`
		Points []llmTimeseriesPoint `json:"points"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Metric != "tokens" || len(result.Points) != 2 || result.Points[0].Value != 12 ||
		result.Points[1].Value != 23 {
		t.Fatalf("result=%s", body)
	}
}

func TestLLMRangeAndLimitProtection(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	app := newTestGateway(
		Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 1}}, Now: func() time.Time { return now }},
	).App()
	for _, target := range []string{
		"/api/llm/summary?start=" + url.QueryEscape(now.Add(-8*24*time.Hour).Format(time.RFC3339)) + "&end=" + url.QueryEscape(now.Format(time.RFC3339)),
		"/api/llm/calls?limit=1001",
		"/api/llm/timeseries?step=1s",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+"lou_valid")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status=%d", target, resp.StatusCode)
		}
	}
}
