package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"

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
