package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

type standaloneCapture struct {
	signal, method, path, query, accountID, projectID, contentType, contentEncoding, remoteWriteVersion, body string
}

func TestStandaloneStorageRoutesQueriesWritesAndTenantsThroughBackend(t *testing.T) {
	captures := make(chan standaloneCapture, 8)
	upstream := func(signal string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			captures <- standaloneCapture{
				signal: signal, method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
				accountID: r.Header.Get("AccountID"), projectID: r.Header.Get("ProjectID"),
				contentType: r.Header.Get("Content-Type"), contentEncoding: r.Header.Get("Content-Encoding"), remoteWriteVersion: r.Header.Get("X-Prometheus-Remote-Write-Version"), body: string(body),
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	metricsQuery, metricsIngest := upstream("metrics-query"), upstream("metrics-ingest")
	logs, traces := upstream("logs"), upstream("traces")
	defer metricsQuery.Close()
	defer metricsIngest.Close()
	defer logs.Close()
	defer traces.Close()

	backend, err := storage.New(
		storage.MetricsPrometheus,
		metricsQuery.URL,
		metricsIngest.URL,
		logs.URL,
		traces.URL,
		metricsQuery.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(
		Config{
			Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Storage: backend,
			Client:  metricsQuery.Client(),
		},
	).App()

	request := func(method, target, contentType, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+"lou_valid")
		req.Header.Set("AccountID", "evil")
		req.Header.Set("ProjectID", "evil")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, testErr := app.Test(req)
		if testErr != nil || resp.StatusCode != want {
			responseBody, _ := io.ReadAll(resp.Body)
			t.Fatalf(
				"%s %s status=%d want=%d err=%v body=%s",
				method,
				target,
				resp.StatusCode,
				want,
				testErr,
				responseBody,
			)
		}
		_ = resp.Body.Close()
	}

	request(http.MethodGet, "/api/v1/query?"+url.Values{"query": {"up"}}.Encode(), "", "", http.StatusNoContent)
	request(http.MethodPost, "/v1/metrics?extra_label=evil", "application/x-protobuf", "otlp", http.StatusNoContent)
	request(
		http.MethodPost,
		"/api/v1/write?extra_label=evil",
		"application/x-protobuf",
		remoteWriteBody(t, "evil"),
		http.StatusNoContent,
	)
	vitals := `{"vitals":[{"name":"LCP","value":123.4,"page":"/checkout","timestamp":"` + time.Now().
		UTC().
		Format(time.RFC3339Nano) +
		`"}]}`
	request(http.MethodPost, "/api/events/vitals", "application/json", vitals, http.StatusOK)
	request(http.MethodPost, "/v1/logs", "application/x-protobuf", "logs", http.StatusNoContent)
	request(http.MethodGet, "/api/v1/logs/query?query=*", "", "", http.StatusNoContent)
	request(http.MethodPost, "/v1/traces", "application/json", "traces", http.StatusNoContent)
	request(http.MethodGet, "/api/traces/search?service=checkout", "", "", http.StatusNoContent)
	request(
		http.MethodPost,
		"/api/v1/write",
		"application/x-protobuf;proto=io.prometheus.write.v2.Request",
		"unsupported-v2",
		http.StatusUnsupportedMediaType,
	)

	got := make([]standaloneCapture, 0, 8)
	for range 8 {
		got = append(got, <-captures)
	}
	want := []struct{ signal, method, path string }{
		{"metrics-query", http.MethodGet, "/api/v1/query"},
		{"metrics-ingest", http.MethodPost, "/opentelemetry/v1/metrics"},
		{"metrics-ingest", http.MethodPost, "/api/v1/write"},
		{"metrics-ingest", http.MethodPost, "/api/v1/import/prometheus"},
		{"logs", http.MethodPost, "/insert/opentelemetry/v1/logs"},
		{"logs", http.MethodGet, "/select/logsql/query"},
		{"traces", http.MethodPost, "/insert/opentelemetry/v1/traces"},
		{"traces", http.MethodGet, "/select/jaeger/api/traces"},
	}
	for i := range want {
		if got[i].signal != want[i].signal || got[i].method != want[i].method || got[i].path != want[i].path {
			t.Errorf(
				"capture[%d]=%#v want signal=%s method=%s path=%s",
				i,
				got[i],
				want[i].signal,
				want[i].method,
				want[i].path,
			)
		}
	}
	query, _ := url.ParseQuery(got[0].query)
	if !strings.Contains(query.Get("query"), `project_id="alpha"`) || query.Has("extra_filters[]") {
		t.Errorf("Prometheus isolation query=%v", query)
	}
	values, _ := url.ParseQuery(got[1].query)
	if values.Get("extra_label") != "project_id=alpha" {
		t.Errorf("OTLP metrics write query=%v", values)
	}
	remoteQuery, _ := url.ParseQuery(got[2].query)
	if remoteQuery.Has("extra_label") {
		t.Errorf("remote-write retained backend-specific query=%v", remoteQuery)
	}
	if got[2].contentType != "application/x-protobuf" || got[2].contentEncoding != "snappy" ||
		got[2].remoteWriteVersion != "0.1.0" {
		t.Errorf("remote-write headers=%#v", got[2])
	}
	decoded, err := snappy.Decode(nil, []byte(got[2].body))
	if err != nil {
		t.Fatal(err)
	}
	var remote prompb.WriteRequest
	if err := remote.Unmarshal(decoded); err != nil {
		t.Fatal(err)
	}
	if labels := remote.Timeseries[0].Labels; len(labels) != 2 || labels[1].Name != "project_id" ||
		labels[1].Value != "alpha" {
		t.Errorf("remote-write labels=%v", labels)
	}
	if !bytes.Contains([]byte(got[3].body), []byte(`web_vitals_lcp{project_id="alpha",page="/checkout"}`)) {
		t.Errorf("Web Vitals write body=%q", got[3].body)
	}
	for _, i := range []int{4, 5, 6, 7} {
		if got[i].accountID != "0" || got[i].projectID != "42" {
			t.Errorf("tenant headers[%d]=AccountID:%q ProjectID:%q", i, got[i].accountID, got[i].projectID)
		}
	}
}

func TestProxyReplacesFiberDefaultContentType(t *testing.T) {
	const requestProof = "loreq_0123456789abcdef01234567_1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Lazy-Observe-Perf-Request"); got != requestProof {
			t.Errorf("upstream request proof=%q", got)
		}
		w.Header().Set("Content-Type", "application/stream+json")
		w.Header().Set("X-Lazy-Observe-Perf-Sink", "losink_testfixture")
		w.Header().Set("X-Lazy-Observe-Perf-Receipt", "body-digest")
		w.Header().Set("X-Lazy-Observe-Perf-Request", requestProof)
		_, _ = io.WriteString(w, "{\"_msg\":\"fixture\"}\n")
	}))
	defer upstream.Close()
	backend, err := storage.New(
		storage.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(
		Config{
			Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Storage: backend,
			Client:  upstream.Client(),
		},
	).App()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	req.Header.Set("X-Lazy-Observe-Perf-Request", requestProof)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/stream+json" {
		t.Fatalf("Content-Type=%q, want the single upstream media type", got)
	}
	if got := resp.Header.Get("X-Lazy-Observe-Perf-Sink"); got != "losink_testfixture" {
		t.Fatalf("sink proof=%q", got)
	}
	if got := resp.Header.Get("X-Lazy-Observe-Perf-Receipt"); got != "body-digest" {
		t.Fatalf("payload receipt=%q", got)
	}
	if got := resp.Header.Get("X-Lazy-Observe-Perf-Request"); got != requestProof {
		t.Fatalf("request proof echo=%q", got)
	}
	if body, readErr := io.ReadAll(resp.Body); readErr != nil || string(body) != "{\"_msg\":\"fixture\"}\n" {
		t.Fatalf("body=%q err=%v", body, readErr)
	}
}

func remoteWriteBody(t *testing.T, project string) string {
	t.Helper()
	request := prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "requests_total"}, {Name: "project_id", Value: project}},
		Samples: []prompb.Sample{{Value: 1, Timestamp: 123}},
	}}}
	protobuf, err := request.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(snappy.Encode(nil, protobuf))
}
