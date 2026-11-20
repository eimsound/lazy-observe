package storage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
)

func TestStandaloneBackendExposesOnlyInterfaceBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	backend, err := New(
		MetricsPrometheus,
		server.URL+"/",
		server.URL+"/",
		server.URL+"/",
		server.URL+"/",
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var signals SignalBackend = backend
	if signals.MetricsMode() != MetricsPrometheus || signals.MetricsQueryBase() != server.URL ||
		signals.MetricsIngestBase() != server.URL ||
		!signals.MetricsIngestEnabled() ||
		signals.MetricsClustered() ||
		signals.LogsBase() != server.URL ||
		signals.TracesBase() != server.URL {
		t.Fatalf("standalone backend mismatch: %#v", backend)
	}
	header := http.Header{"Accountid": {"evil"}, "Projectid": {"evil"}}
	signals.SetTenantHeaders(header, 42)
	if header.Get("AccountID") != "0" || header.Get("ProjectID") != strconv.Itoa(42) {
		t.Fatalf("tenant headers=%v", header)
	}
}

func TestVMClusterTenantEndpoints(t *testing.T) {
	server := httptest.NewServer(nil)
	server.Close()
	b, err := NewWithMetricsOptions(
		MetricsVM,
		server.URL+"/",
		server.URL+"/",
		server.URL,
		server.URL,
		MetricsOptions{Cluster: true, TenantAccountID: "7"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.MetricsQueryBase(), server.URL+"/select/7/prometheus"; got != want {
		t.Fatalf("query base=%q want=%q", got, want)
	}
	if got, want := b.MetricsIngestBase(), server.URL+"/insert/7"; got != want {
		t.Fatalf("ingest base=%q want=%q", got, want)
	}
	if got, want := b.PrometheusRemoteWriteBase(), server.URL+"/insert/7/prometheus"; got != want {
		t.Fatalf("Prometheus remote-write base=%q want=%q", got, want)
	}
	header := http.Header{}
	b.SetTenantHeaders(header, 42)
	if header.Get("AccountID") != "7" || header.Get("ProjectID") != "42" {
		t.Fatalf("cluster tenant headers=%v want AccountID=7 ProjectID=42", header)
	}
}

func TestVMClusterHealthProbesVMSelectAndVMInsert(t *testing.T) {
	var selectProbes, insertProbes atomic.Int64
	selectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("select path=%q", r.URL.Path)
		}
		selectProbes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer selectServer.Close()
	insertServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		insertProbes.Add(1)
		switch r.URL.Path {
		case "/-/healthy":
			w.WriteHeader(http.StatusNotFound)
		case "/health":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("insert path=%q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer insertServer.Close()
	b, err := NewWithMetricsOptions(
		MetricsVM,
		selectServer.URL,
		insertServer.URL,
		selectServer.URL,
		selectServer.URL,
		MetricsOptions{Cluster: true, TenantAccountID: "7"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result := b.Health(context.Background())
	if result["metrics"] != nil || result["metrics-ingest"] != nil {
		t.Fatalf("health=%v", result)
	}
	if selectProbes.Load() != 3 || insertProbes.Load() != 2 {
		t.Fatalf("select probes=%d insert probes=%d", selectProbes.Load(), insertProbes.Load())
	}
}

func TestStandaloneHealthIncludesSeparateMetricsWriteEndpoint(t *testing.T) {
	query := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/healthy" && r.URL.Path != "/health" {
			t.Errorf("query health path=%q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer query.Close()
	write := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/healthy" {
			t.Errorf("write health path=%q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer write.Close()
	backend, err := New(MetricsPrometheus, query.URL, write.URL, query.URL, query.URL, query.Client())
	if err != nil {
		t.Fatal(err)
	}
	health := backend.Health(context.Background())
	if len(health) != 4 || health["metrics"] != nil || health["metrics-ingest"] != nil || health["logs"] != nil ||
		health["traces"] != nil {
		t.Fatalf("health=%v", health)
	}
}

func TestStandaloneHealthReportsExternalFailureAndRecoversWithoutReconstruction(t *testing.T) {
	var unavailable atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	query := httptest.NewServer(handler)
	defer query.Close()
	write := httptest.NewServer(handler)
	defer write.Close()

	backend, err := New(MetricsPrometheus, query.URL, write.URL, query.URL, query.URL, query.Client())
	if err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	failed := backend.Health(context.Background())
	for _, component := range []string{"metrics", "metrics-ingest", "logs", "traces"} {
		if failed[component] == nil {
			t.Fatalf("external %s failure was not reported: %v", component, failed)
		}
	}

	unavailable.Store(false)
	recovered := backend.Health(context.Background())
	for _, component := range []string{"metrics", "metrics-ingest", "logs", "traces"} {
		if recovered[component] != nil {
			t.Fatalf("external %s did not recover through the existing backend: %v", component, recovered)
		}
	}
}

func TestInjectPromQLCoversEverySelector(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"nested function", `sum(rate(http_requests_total{method="GET"}[5m]))`, 1},
		{"subquery", `max_over_time(rate(requests_total[5m])[1h:5m])`, 1},
		{"binary both arms", `requests_total{code="200"} / requests_total{code="500"}`, 2},
		{"label replace", `label_replace(up, "service", "$1", "job", "(.*)")`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InjectPromQL(tt.query, "alpha")
			if err != nil {
				t.Fatal(err)
			}
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(got)
			if err != nil {
				t.Fatalf("parse injected query %q: %v", got, err)
			}
			selectors := parser.ExtractSelectors(expr)
			if len(selectors) != tt.want {
				t.Fatalf("selectors=%d want=%d; %s", len(selectors), tt.want, got)
			}
			for _, matchers := range selectors {
				found := false
				for _, matcher := range matchers {
					if matcher.Name == "project_id" && matcher.Type.String() == "=" && matcher.Value == "alpha" {
						found = true
					}
				}
				if !found {
					t.Fatalf("selector lacks trusted matcher: %s", got)
				}
			}
		})
	}
}

func TestInjectPromQLHostileProjectMatcherIsANDed(t *testing.T) {
	got, err := InjectPromQL(`{project_id=~".*"}`, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(got)
	if err != nil {
		t.Fatal(err)
	}
	matchers := parser.ExtractSelectors(expr)[0]
	var hostile, trusted bool
	for _, matcher := range matchers {
		if matcher.Name == "project_id" && matcher.Type.String() == "=~" && matcher.Value == ".*" {
			hostile = true
		}
		if matcher.Name == "project_id" && matcher.Type.String() == "=" && matcher.Value == "alpha" {
			trusted = true
		}
	}
	if !hostile || !trusted {
		t.Fatalf("hostile=%v trusted=%v query=%s", hostile, trusted, got)
	}
}

func TestInjectPromQLRejectsInvalidQuery(t *testing.T) {
	if _, err := InjectPromQL(`sum(`, "alpha"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSetTenantHeadersRejectsNonPositiveProjectID(t *testing.T) {
	b := &Backend{}
	for _, id := range []int{0, -1, -42} {
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			b.SetTenantHeaders(http.Header{}, id)
		}()
		if !panicked {
			t.Fatalf("SetTenantHeaders(%d) did not panic", id)
		}
	}
}

func TestInjectPromQLPreservesUTF8QuotedMetricName(t *testing.T) {
	if seeded := seedBracedSelectors(
		`{"http.server.request.duration_count"}`,
		"alpha",
	); seeded != `{"http.server.request.duration_count",project_id="alpha"}` {
		t.Fatalf("seedBracedSelectors placed matcher before quoted metric name: %q", seeded)
	}
	if seeded := seedBracedSelectors(
		`{"http.server.request.duration_count", method="GET"}`,
		"alpha",
	); seeded != `{"http.server.request.duration_count",project_id="alpha", method="GET"}` {
		t.Fatalf("seedBracedSelectors broke quoted metric name with matchers: %q", seeded)
	}
	queries := []string{
		`{"http.server.request.duration_count"}`,
		`{"http.server.request.duration_count", method="GET"}`,
		`{"http.server.request.duration_count", project_id=~".*"}`,
		`rate({"http.server.request.duration_bucket", "http.request.method"="POST"}[5m])`,
		`{"label.name"="val"}`,
	}
	for _, q := range queries {
		got, err := InjectPromQL(q, "alpha")
		if err != nil {
			t.Fatalf("InjectPromQL(%q) failed: %v", q, err)
		}
		expr, err := parser.NewParser(parser.Options{}).ParseExpr(got)
		if err != nil {
			t.Fatalf("re-parse %q (from %q): %v", got, q, err)
		}
		selectors := parser.ExtractSelectors(expr)
		if len(selectors) == 0 {
			t.Fatalf("no selectors in %q", got)
		}
		for _, matchers := range selectors {
			found := false
			for _, m := range matchers {
				if m.Name == "project_id" && m.Type.String() == "=" && m.Value == "alpha" {
					found = true
				}
			}
			if !found {
				t.Fatalf("selector missing project_id=alpha in %q", got)
			}
		}
	}
}

func TestLogsAndTracesReadWriteSplitEndpointsAndHealth(t *testing.T) {
	var logsQueryHits, logsIngestHits, tracesQueryHits, tracesIngestHits atomic.Int32
	var logsIngestDown atomic.Bool
	newServer := func(counter *atomic.Int32, down *atomic.Bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counter.Add(1)
			if down != nil && down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	}
	metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer metricsSrv.Close()
	logsQuery := newServer(&logsQueryHits, nil)
	defer logsQuery.Close()
	logsIngest := newServer(&logsIngestHits, &logsIngestDown)
	defer logsIngest.Close()
	tracesQuery := newServer(&tracesQueryHits, nil)
	defer tracesQuery.Close()
	tracesIngest := newServer(&tracesIngestHits, nil)
	defer tracesIngest.Close()

	// Partial override: traces falls back to URL for query, overrides ingest.
	b, err := NewWithOptions(BackendOptions{
		MetricsKind:     MetricsVM,
		MetricsQueryURL: metricsSrv.URL,
		Logs: SignalEndpoint{
			QueryURL:  logsQuery.URL + "/",
			IngestURL: logsIngest.URL + "/",
		},
		Traces: SignalEndpoint{
			URL:       tracesQuery.URL + "/",
			IngestURL: tracesIngest.URL + "/",
		},
	}, metricsSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if b.LogsBase() != logsQuery.URL || b.LogsQueryBase() != logsQuery.URL || b.LogsIngestBase() != logsIngest.URL {
		t.Fatalf("logs bases query=%q ingest=%q", b.LogsQueryBase(), b.LogsIngestBase())
	}
	if b.TracesBase() != tracesQuery.URL || b.TracesQueryBase() != tracesQuery.URL ||
		b.TracesIngestBase() != tracesIngest.URL {
		t.Fatalf("traces bases query=%q ingest=%q", b.TracesQueryBase(), b.TracesIngestBase())
	}

	health := b.Health(context.Background())
	for _, key := range []string{"metrics", "logs", "logs-ingest", "traces", "traces-ingest"} {
		if err, exists := health[key]; !exists || err != nil {
			t.Fatalf("health[%q] exists=%v err=%v", key, exists, err)
		}
	}
	if logsQueryHits.Load() != 1 || logsIngestHits.Load() != 1 ||
		tracesQueryHits.Load() != 1 || tracesIngestHits.Load() != 1 {
		t.Fatalf(
			"probe counts logsQ=%d logsI=%d tracesQ=%d tracesI=%d",
			logsQueryHits.Load(), logsIngestHits.Load(), tracesQueryHits.Load(), tracesIngestHits.Load(),
		)
	}

	logsIngestDown.Store(true)
	degraded := b.Health(context.Background())
	if degraded["logs"] != nil || degraded["logs-ingest"] == nil {
		t.Fatalf("degraded health=%v want logs=nil logs-ingest!=nil", degraded)
	}

	// Missing one side without fallback URL must fail construction.
	if _, err := NewWithOptions(BackendOptions{
		MetricsKind:     MetricsVM,
		MetricsQueryURL: metricsSrv.URL,
		Logs:            SignalEndpoint{QueryURL: logsQuery.URL},
		Traces:          SignalEndpoint{URL: tracesQuery.URL},
	}, nil); err == nil {
		t.Fatal("expected error when logs ingest URL is missing and fallback URL is empty")
	}
}

func TestUnifiedTenantHelpersFailClosed(t *testing.T) {
	b, err := NewWithMetricsOptions(
		MetricsVM,
		"http://127.0.0.1:8428",
		"http://127.0.0.1:8428",
		"http://127.0.0.1:9428",
		"http://127.0.0.1:10428",
		MetricsOptions{TenantAccountID: "19"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if b.TenantAccountID() != "19" {
		t.Fatalf("TenantAccountID=%q want=19", b.TenantAccountID())
	}
	hdr := http.Header{"AccountID": {"spoof"}, "ProjectID": {"spoof"}}
	if err := b.ApplyTenantHeaders(hdr, 0); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("ApplyTenantHeaders(0) err=%v want ErrInvalidTenant", err)
	}
	if err := b.ApplyTenantHeaders(hdr, 55); err != nil {
		t.Fatalf("ApplyTenantHeaders(55): %v", err)
	}
	if hdr.Get("AccountID") != "19" || hdr.Get("ProjectID") != "55" {
		t.Fatalf("headers=%v", hdr)
	}
	if _, err := b.MetricsIngestExtraLabel(""); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("MetricsIngestExtraLabel('') err=%v want ErrInvalidTenant", err)
	}
	if got, err := b.MetricsIngestExtraLabel("alpha"); err != nil || got != "project_id=alpha" {
		t.Fatalf("MetricsIngestExtraLabel(alpha)=(%q, %v)", got, err)
	}
	values := url.Values{"extra_filters[]": {"evil"}}
	if err := b.ApplyMetricsQueryIsolation(values, "", true); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("ApplyMetricsQueryIsolation('') err=%v want ErrInvalidTenant", err)
	}
	if err := b.ApplyMetricsQueryIsolation(values, "alpha", true); err != nil {
		t.Fatalf("ApplyMetricsQueryIsolation(alpha): %v", err)
	}
	if got := values.Get("extra_filters[]"); got != `{project_id="alpha"}` {
		t.Fatalf("extra_filters[]=%q", got)
	}
}
