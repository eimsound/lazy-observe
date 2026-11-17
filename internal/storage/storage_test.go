package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
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
