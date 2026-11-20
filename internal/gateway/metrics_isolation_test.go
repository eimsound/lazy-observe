package gateway

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

// VictoriaMetrics v1.153+ ignores empty extra_label/extra_filters values instead of rejecting them, so the
// gateway must never produce an empty isolation value (that would turn into a cross-project query).
func TestApplyMetricsIsolationVMNeverEmpty(t *testing.T) {
	backend, err := storage.New(
		storage.MetricsVM,
		"http://127.0.0.1:1",
		"",
		"http://127.0.0.1:1",
		"http://127.0.0.1:1",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{storage: backend}

	values := url.Values{"query": {"up"}, "extra_filters[]": {`{project_id="evil"}`}}
	if err := g.applyMetricsIsolation(values, "alpha", true); err != nil {
		t.Fatal(err)
	}
	filters := values["extra_filters[]"]
	if len(filters) != 1 || filters[0] != `{project_id="alpha"}` {
		t.Fatalf("extra_filters[] = %q", filters)
	}

	empty := url.Values{"query": {"up"}}
	if err := g.applyMetricsIsolation(empty, "", true); err == nil {
		t.Fatal("empty project slug must be rejected, not turned into an ignored (cross-project) filter")
	}
	if len(empty["extra_filters[]"]) != 0 {
		t.Fatalf("empty project must not inject a filter: %q", empty["extra_filters[]"])
	}
}

// The metrics query proxy skips isolation when its project is empty, so the handler itself must refuse a request
// without a project context instead of forwarding it unfiltered.
func TestMetricsQueryWithoutProjectFailsClosed(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	backend, err := storage.New(storage.MetricsVM, upstream.URL, "", upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{storage: backend, client: upstream.Client()}
	app := fiber.New()
	app.Get("/api/v1/query", g.metricsQuery)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	if hits.Load() != 0 {
		t.Fatalf("query without a project reached VictoriaMetrics %d time(s)", hits.Load())
	}
}

func TestPrometheusPostFormMatchDoesNotAddDefaultMatcherUnion(t *testing.T) {
	type capturedReq struct {
		query url.Values
		form  url.Values
	}
	captured := make(chan capturedReq, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		captured <- capturedReq{query: r.URL.Query(), form: form}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","data":[]}`)
	}))
	defer upstream.Close()
	backend, err := storage.New(storage.MetricsPrometheus, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Storage: backend,
	}).App()

	// Mixed-case Content-Type with form-encoded match[] must isolate the form
	// matcher and must NOT inject an extra default match[] on the URL query
	// (which Prometheus ORs with the form matcher, breaking filtering).
	for _, endpoint := range []string{"/api/v1/series", "/api/v1/labels"} {
		body := url.Values{"match[]": {`up{job="api"}`}}.Encode()
		req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer lou_valid")
		req.Header.Set("Content-Type", "Application/X-WWW-Form-Urlencoded; charset=utf-8")
		resp, err := app.Test(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d err=%v", endpoint, resp.StatusCode, err)
		}
		resp.Body.Close()
		got := <-captured
		if len(got.query["match[]"]) != 0 {
			t.Fatalf(
				"%s added unexpected URL default match[]=%v (would OR with form match[])",
				endpoint,
				got.query["match[]"],
			)
		}
		if len(got.form["match[]"]) != 1 || !strings.Contains(got.form["match[]"][0], `project_id="alpha"`) {
			t.Fatalf("%s form match[]=%v missing project isolation", endpoint, got.form["match[]"])
		}
	}
}

func TestRemoteWriteDecodedLimitConcurrencyAndRateLimit(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	gw := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 9, Slug: "alpha"}},
		Metrics: upstream.URL,
	})
	gw.remoteWriteSem = make(chan struct{}, 1)
	gw.remoteWriteRateLimit = 2
	app := gw.App()

	// Oversized declared snappy payload (> 8 MiB remote-write limit, below 52 MiB global limit) must be 413.
	oversizedHeader := binary.AppendUvarint(nil, maxRemoteWriteDecodedBody+1)
	overReq := httptest.NewRequest(http.MethodPost, "/api/v1/write", bytes.NewReader(append(oversizedHeader, 0x00)))
	overReq.Header.Set("Authorization", "Bearer valid-token")
	overResp, err := app.Test(overReq)
	if err != nil {
		t.Fatal(err)
	}
	overResp.Body.Close()
	if overResp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized remote-write status=%d want=413", overResp.StatusCode)
	}

	// Concurrency semaphore=1: second concurrent request gets 429 immediately.
	validBody := remoteWriteBody(t, "evil")
	firstDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/write", strings.NewReader(validBody))
		req.Header.Set("Authorization", "Bearer valid-token")
		resp, testErr := app.Test(req)
		if testErr != nil {
			firstDone <- 0
			return
		}
		resp.Body.Close()
		firstDone <- resp.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first remote-write did not reach upstream")
	}
	concurrentReq := httptest.NewRequest(http.MethodPost, "/api/v1/write", strings.NewReader(validBody))
	concurrentReq.Header.Set("Authorization", "Bearer valid-token")
	concurrentResp, err := app.Test(concurrentReq)
	if err != nil {
		t.Fatal(err)
	}
	concurrentResp.Body.Close()
	if concurrentResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("concurrent remote-write status=%d want=429", concurrentResp.StatusCode)
	}
	close(block)
	if status := <-firstDone; status != http.StatusNoContent {
		t.Fatalf("first remote-write status=%d want=204", status)
	}

	// Rate limit = 2/min (oversized + first consumed 2 tokens); next request must get 429.
	rateReq := httptest.NewRequest(http.MethodPost, "/api/v1/write", strings.NewReader(validBody))
	rateReq.Header.Set("Authorization", "Bearer valid-token")
	rateResp, err := app.Test(rateReq)
	if err != nil {
		t.Fatal(err)
	}
	rateResp.Body.Close()
	if rateResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited remote-write status=%d want=429", rateResp.StatusCode)
	}
}

func TestOTLPMetricsIngestEnforcesTrustedProjectExtraLabel(t *testing.T) {
	captured := make(chan url.Values, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 12, Slug: "alpha"}},
		Metrics: upstream.URL,
	}).App()

	// Client embeds hostile project.id and project_id resource/datapoint attributes
	// and attempts extra_label smuggling in the query string.
	otlpReq := &metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricsdata.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{
				Key:   "project.id",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "victim"}},
			},
			{
				Key:   "project_id",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "victim"}},
			},
		}},
		ScopeMetrics: []*metricsdata.ScopeMetrics{{Metrics: []*metricsdata.Metric{{
			Name: "spoofed_metric",
			Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{DataPoints: []*metricsdata.NumberDataPoint{{
				TimeUnixNano: uint64(time.Now().UnixNano()),
				Value:        &metricsdata.NumberDataPoint_AsDouble{AsDouble: 7},
				Attributes: []*commonpb.KeyValue{
					{
						Key:   "project.id",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "victim"}},
					},
					{
						Key:   "project_id",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "victim"}},
					},
				},
			}}}},
		}}}},
	}}}
	raw, err := proto.Marshal(otlpReq)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics?extra_label=project_id=victim", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	got := <-captured
	if values := got["extra_label"]; len(values) != 1 || values[0] != "project_id=alpha" {
		t.Fatalf("extra_label=%v want [project_id=alpha]", values)
	}
}

func TestMetricsProxyRejectsMultipartAndNormalizesFormContentType(t *testing.T) {
	for _, mode := range []storage.MetricsKind{storage.MetricsVM, storage.MetricsPrometheus} {
		t.Run(string(mode), func(t *testing.T) {
			var upstreamHits atomic.Int32
			type capturedReq struct {
				query url.Values
				form  url.Values
			}
			captured := make(chan capturedReq, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				raw, _ := io.ReadAll(r.Body)
				form, _ := url.ParseQuery(string(raw))
				captured <- capturedReq{query: r.URL.Query(), form: form}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			}))
			defer upstream.Close()
			backend, err := storage.New(mode, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			app := newTestGateway(Config{
				Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
				Storage: backend,
			}).App()

			// 1. Mixed-case application/x-www-form-urlencoded must strip reserved params and isolate PromQL.
			formBody := url.Values{
				"query":           {"up"},
				"extra_filters[]": {`{project_id="victim"}`},
				"extra_label":     {"project_id=victim"},
			}.Encode()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(formBody))
			req.Header.Set("Authorization", "Bearer lou_valid")
			req.Header.Set("Content-Type", "Application/X-WWW-Form-URLENCODED; charset=UTF-8")
			resp, err := app.Test(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("mixed-case form status=%d err=%v", resp.StatusCode, err)
			}
			resp.Body.Close()
			got := <-captured
			if got.form.Has("extra_filters[]") || got.form.Has("extra_label") {
				t.Fatalf("reserved form params leaked: %v", got.form)
			}
			if mode == storage.MetricsPrometheus {
				if !strings.Contains(got.form.Get("query"), `project_id="alpha"`) {
					t.Fatalf("prometheus form query missing isolation: %q", got.form.Get("query"))
				}
			} else {
				if got.query.Get("extra_filters[]") != `{project_id="alpha"}` {
					t.Fatalf("vm query missing trusted extra_filters[]: %v", got.query)
				}
			}

			// 2. multipart/form-data (any case, even empty or non-POST) and body without form Content-Type must be rejected with 415.
			hitsBefore := upstreamHits.Load()
			multipartBody := "--boundary\r\nContent-Disposition: form-data; name=\"query\"\r\n\r\nup\r\n--boundary--\r\n"
			for _, tc := range []struct {
				name        string
				method      string
				contentType string
				body        string
			}{
				{
					"multipart post",
					http.MethodPost,
					"multipart/form-data; boundary=boundary",
					multipartBody,
				},
				{
					"mixed-case multipart post",
					http.MethodPost,
					"Multipart/Form-Data; boundary=boundary",
					multipartBody,
				},
				{
					"empty multipart post",
					http.MethodPost,
					"multipart/form-data; boundary=boundary",
					"",
				},
				{
					"post body without content-type",
					http.MethodPost,
					"",
					"query=up",
				},
				{
					"put body with multipart",
					http.MethodPut,
					"multipart/form-data; boundary=boundary",
					multipartBody,
				},
			} {
				badReq := httptest.NewRequest(tc.method, "/api/v1/query", strings.NewReader(tc.body))
				badReq.Header.Set("Authorization", "Bearer lou_valid")
				if tc.contentType != "" {
					badReq.Header.Set("Content-Type", tc.contentType)
				}
				badResp, badErr := app.Test(badReq)
				if badErr != nil {
					t.Fatalf("%s: %v", tc.name, badErr)
				}
				badResp.Body.Close()
				if badResp.StatusCode != http.StatusUnsupportedMediaType {
					t.Fatalf("%s: status=%d want=415", tc.name, badResp.StatusCode)
				}
			}
			if upstreamHits.Load() != hitsBefore {
				t.Fatalf("rejected metrics requests reached upstream %d times", upstreamHits.Load()-hitsBefore)
			}
		})
	}
}
