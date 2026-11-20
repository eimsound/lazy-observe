package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/llmcost"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestExtractLLMCalls(t *testing.T) {
	pricing := llmcost.Default()
	body := []byte(
		`{"_time":"2026-07-17T10:00:00Z","resource_attr:service.name":"checkout","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.request.model":"gpt-4o","span_attr:gen_ai.response.model":"gpt-4o-mini-2024-07-18","span_attr:gen_ai.usage.input_tokens":"2000","span_attr:gen_ai.usage.output_tokens":"500","duration":"125000000","trace_id":"abc","span_id":"span-1","status_code":"2"}
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
		first.SpanID != "span-1" ||
		first.TraceID != "0000000000000abc" ||
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

func TestExtractLLMCallsSanitizesDirtySpansAndEmptyFallbacks(t *testing.T) {
	pricing := llmcost.Default()
	body := []byte(strings.Join([]string{
		// Empty/whitespace response.model & resource_attr:service.name must fall back;
		// negative duration & tokens must clamp to 0; float-string tokens must parse;
		// JSON bool error=true must mark status=error; bad trace_id must normalize to 32 hex.
		`{"_time":"2026-07-17T10:00:00Z","resource_attr:service.name":"   ","service.name":"billing","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.response.model":"","span_attr:gen_ai.request.model":"gpt-4o-mini","span_attr:gen_ai.usage.input_tokens":"1000.0","span_attr:gen_ai.usage.output_tokens":-5,"duration":"-9000","trace_id":"not-a-hex-id","span_attr:error":true}`,
		// Usage missing flag must mark call unpriced even for a known model.
		`{"_time":"2026-07-17T10:01:00Z","resource_attr:service.name":"stream-svc","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.request.model":"gpt-4o","span_attr:gen_ai.usage.missing":true,"duration":15000,"trace_id":"0x1122334455667788","status_code":1}`,
		// Anthropic prompt caching tokens must be included in cost.
		`{"_time":"2026-07-17T10:02:00Z","resource_attr:service.name":"claude-svc","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.provider.name":"anthropic","span_attr:gen_ai.response.model":"claude-sonnet-4-20250514","span_attr:gen_ai.usage.input_tokens":"100","span_attr:gen_ai.usage.cache_creation.input_tokens":"5000","span_attr:gen_ai.usage.cache_read.input_tokens":"20000","span_attr:gen_ai.usage.output_tokens":"200","duration":"2000","trace_id":"12345678-1234-1234-1234-1234567890ab","status_code":"1"}`,
		// Blank operation or missing timestamp rows are skipped.
		`{"_time":"2026-07-17T10:03:00Z","span_attr:gen_ai.operation.name":"  ","span_attr:operation.name":"chat"}`,
		`{"_time":"invalid-time","span_attr:gen_ai.operation.name":"chat"}`,
	}, "\n") + "\n")

	calls, err := extractLLMCalls(body, pricing)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("len(calls)=%d want=3 calls=%#v", len(calls), calls)
	}

	first := calls[0]
	if first.Service != "billing" || first.Model != "gpt-4o-mini" || first.InputTokens != 1000 ||
		first.OutputTokens != 0 || first.Duration != 0 || first.Status != "error" ||
		first.TraceID != "00000000000000000000000000000000" || first.Unpriced {
		t.Fatalf("sanitized first call=%#v", first)
	}
	if math.Abs(first.Cost-0.00015) > 1e-12 {
		t.Fatalf("first cost=%v want=0.00015", first.Cost)
	}

	second := calls[1]
	if !second.UsageMissing || !second.Unpriced || second.Cost != 0 || second.TraceID != "1122334455667788" {
		t.Fatalf("usage missing call=%#v", second)
	}

	third := calls[2]
	if third.Unpriced || math.Abs(third.Cost-0.02805) > 1e-12 ||
		third.TraceID != "123456781234123412341234567890ab" {
		t.Fatalf("anthropic cached call=%#v", third)
	}
}

func TestLLMCallsPushesDownProjectionFilterSortAndHandlesLargeResponses(t *testing.T) {
	start := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	var seenQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query().Get("query")
		if r.URL.Query().Get("disable_latency_offset") != "true" {
			t.Errorf("missing disable_latency_offset: %v", r.URL.Query())
		}
		// Return 3 rows for limit=2 to verify truncation and descending order.
		// Include an extra >1 MiB field in the mock payload to confirm streaming
		// JSON decoding never hits bufio.Scanner's 1 MiB token limit.
		bigPadding := strings.Repeat("x", (1<<20)+64)
		for i, offset := range []time.Duration{3 * time.Minute, 2 * time.Minute, 1 * time.Minute} {
			_, _ = fmt.Fprintf(
				w,
				`{"_time":%q,"resource_attr:service.name":"checkout","span_attr:gen_ai.operation.name":"chat","span_attr:gen_ai.request.model":"gpt-4o-mini","span_attr:gen_ai.usage.input_tokens":"10","span_attr:gen_ai.usage.output_tokens":"5","duration":"1000","trace_id":"1111111111111111111111111111111%d","span_id":"sp-%d","status_code":"1","extra":%q}`+"\n",
				start.Add(offset).Format(time.RFC3339Nano),
				i,
				i,
				bigPadding,
			)
		}
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Traces: upstream.URL,
		Now:    func() time.Time { return start.Add(10 * time.Minute) },
	}).App()

	target := "/api/llm/calls?" + url.Values{
		"start":   {start.Format(time.RFC3339)},
		"end":     {start.Add(10 * time.Minute).Format(time.RFC3339)},
		"service": {"checkout"},
		"model":   {"gpt-4o-mini"},
		"limit":   {"2"},
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
	if !strings.Contains(seenQuery, "| fields ") ||
		!strings.Contains(seenQuery, "| sort by (_time) desc | limit 3") ||
		!strings.Contains(seenQuery, `"resource_attr:service.name":="checkout"`) ||
		!strings.Contains(seenQuery, `"span_attr:gen_ai.response.model":="gpt-4o-mini"`) {
		t.Fatalf("expected projection, filter pushdown, sort, and limit in query: %s", seenQuery)
	}
	var result struct {
		Calls     []llmCall `json:"calls"`
		Count     int       `json:"count"`
		Limit     int       `json:"limit"`
		Truncated bool      `json:"truncated"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || result.Limit != 2 || !result.Truncated || len(result.Calls) != 2 ||
		result.Calls[0].SpanID != "sp-0" || result.Calls[1].SpanID != "sp-1" {
		t.Fatalf("calls result=%#v", result)
	}
}

func TestLLMSummaryUsesStatsPushdownAndTracksUnpricedCalls(t *testing.T) {
	start := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	var seenQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query().Get("query")
		// Stats rows emitted by VictoriaTraces LogsQL `| stats by (...)`.
		_, _ = io.WriteString(w, strings.Join([]string{
			`{"resource_attr:service.name":"chat-api","service.name":"","span_attr:gen_ai.response.model":"claude-sonnet-4-20250514","span_attr:gen_ai.request.model":"claude-sonnet-4","span_attr:gen_ai.provider.name":"anthropic","span_attr:gen_ai.system":"anthropic","span_attr:gen_ai.usage.missing":"","calls":"2","input_tokens":"200","output_tokens":"400","cache_read_tokens":"40000","cache_creation_tokens":"10000","reasoning_tokens":"0"}`,
			`{"resource_attr:service.name":"chat-api","service.name":"","span_attr:gen_ai.response.model":"deepseek-chat","span_attr:gen_ai.request.model":"","span_attr:gen_ai.provider.name":"openai","span_attr:gen_ai.system":"openai","span_attr:gen_ai.usage.missing":"","calls":"3","input_tokens":"1.5e+03","output_tokens":"500.0","cache_read_tokens":"NaN","cache_creation_tokens":"0","reasoning_tokens":"0"}`,
			`{"resource_attr:service.name":"stream-api","service.name":"","span_attr:gen_ai.response.model":"gpt-4o","span_attr:gen_ai.request.model":"gpt-4o","span_attr:gen_ai.provider.name":"openai","span_attr:gen_ai.system":"openai","span_attr:gen_ai.usage.missing":"true","calls":"1","input_tokens":"0","output_tokens":"0","cache_read_tokens":"0","cache_creation_tokens":"0","reasoning_tokens":"0"}`,
		}, "\n")+"\n")
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Traces: upstream.URL,
		Now:    func() time.Time { return start.Add(time.Hour) },
	}).App()

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/llm/summary?start="+url.QueryEscape(
			start.Format(time.RFC3339),
		)+"&end="+url.QueryEscape(
			start.Add(time.Hour).Format(time.RFC3339),
		),
		nil,
	)
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
	if !strings.Contains(seenQuery, "| stats by (") {
		t.Fatalf("summary query must use stats pushdown: %s", seenQuery)
	}
	var summary struct {
		TotalCalls        int64               `json:"total_calls"`
		TotalInputTokens  int64               `json:"total_input_tokens"`
		TotalOutputTokens int64               `json:"total_output_tokens"`
		TotalCost         float64             `json:"total_cost"`
		Unpriced          bool                `json:"unpriced"`
		UnpricedCalls     int64               `json:"unpriced_calls"`
		UnpricedBreakdown []llmUnpricedDetail `json:"unpriced_breakdown"`
		ByModel           []llmModelAggregate `json:"by_model"`
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.TotalCalls != 6 || summary.TotalInputTokens != 1700 || summary.TotalOutputTokens != 900 ||
		!summary.Unpriced || summary.UnpricedCalls != 4 {
		t.Fatalf("summary=%#v", summary)
	}
	// Anthropic cached cost: 200*0.003/1k + 10000*0.00375/1k + 40000*0.0003/1k + 400*0.015/1k = 0.0561
	if math.Abs(summary.TotalCost-0.0561) > 1e-12 {
		t.Fatalf("summary cost=%v want=0.0561", summary.TotalCost)
	}
	if len(summary.UnpricedBreakdown) != 2 {
		t.Fatalf("unpriced_breakdown=%#v want 2 entries", summary.UnpricedBreakdown)
	}
	if summary.UnpricedBreakdown[0] != (llmUnpricedDetail{
		Model:        "deepseek-chat",
		Provider:     "openai",
		Reason:       "unknown_model",
		Calls:        3,
		InputTokens:  1500,
		OutputTokens: 500,
	}) {
		t.Fatalf("unpriced_breakdown[0]=%#v", summary.UnpricedBreakdown[0])
	}
	if summary.UnpricedBreakdown[1] != (llmUnpricedDetail{
		Model:        "gpt-4o",
		Provider:     "openai",
		Reason:       "usage_missing",
		Calls:        1,
		InputTokens:  0,
		OutputTokens: 0,
	}) {
		t.Fatalf("unpriced_breakdown[1]=%#v", summary.UnpricedBreakdown[1])
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
		q := r.URL.Query().Get("query")
		if !strings.Contains(q, `| stats by (_time:300s)`) || r.URL.Query().Get("start") == "" ||
			r.URL.Query().Get("end") == "" {
			t.Errorf("query=%v", r.URL.Query())
		}
		if r.URL.Query().Get("disable_latency_offset") != "true" {
			t.Errorf("traces LogsQL request must disable the latency offset: %v", r.URL.Query())
		}
		// Include a row slightly BEFORE firstBucket (-30s) to verify negative diff
		// does NOT truncate toward zero into bucket 0!
		for _, row := range []struct {
			at     time.Time
			input  int
			output int
		}{
			{start.Add(-30 * time.Second), 999, 999},
			{start, 10, 2},
			{start.Add(5 * time.Minute), 20, 3},
		} {
			_, _ = fmt.Fprintf(
				w,
				`{"_time":%q,"input_tokens":%q,"output_tokens":%q}`+"\n",
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

func TestStreamLLMTracesQueryFailClosedOnInvalidProjectID(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte("{}\n"))
	}))
	defer upstream.Close()

	g := newTestGateway(Config{Traces: upstream.URL, Logs: upstream.URL})
	for _, id := range []int{0, -1} {
		if err := g.streamLLMTracesQuery(
			context.Background(),
			meta.ProjectInfo{ID: id, Slug: "alpha"},
			url.Values{"query": {"*"}},
			func(map[string]json.RawMessage) error { return nil },
		); err == nil {
			t.Fatalf("streamLLMTracesQuery(id=%d) succeeded; want fail-closed error", id)
		}
	}
	if called {
		t.Fatal("upstream must not be contacted when project.ID <= 0")
	}
}
