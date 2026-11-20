package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/scrape"
)

func TestScrapeTargetViewerForbidden(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	viewer := sessionCookie(t, store, "scrape-viewer", "password", "viewer")
	service := scrape.NewService(scrape.Config{Store: store, ConfigPath: filepath.Join(t.TempDir(), "scrape.yml")})
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Scrapes: service}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/scrape-targets",
		bytes.NewBufferString(`{"name":"node","url":"http://host:9100/metrics"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(viewer)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d err=%v", resp.StatusCode, err)
	}
}

func TestHostsUseProjectFilter(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProject(context.Background(), "Project Two", "p2"); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "infra-reader", "password", "viewer")
	queries := make(chan url.Values, 16)
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		queries <- q
		expr := q.Get("query")
		result := `[]`
		switch {
		case strings.Contains(expr, "group by(instance)"):
			result = `[{"metric":{"instance":"node:9100"},"value":[1,"1"]}]`
		case strings.Contains(expr, "node_uname_info"):
			result = `[{"metric":{"instance":"node:9100","nodename":"node-a"},"value":[1,"1"]}]`
		case strings.Contains(expr, "up{"):
			result = `[{"metric":{"instance":"node:9100"},"value":[1,"1"]}]`
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
	}))
	defer metrics.Close()
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Metrics: metrics.URL}).App()

	req := httptest.NewRequest(http.MethodGet, "/api/hosts", nil)
	req.Header.Set("X-LO-Project", "p2")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("hosts status=%d err=%v", resp.StatusCode, err)
	}
	var hosts []map[string]any
	if json.NewDecoder(resp.Body).Decode(&hosts) != nil || len(hosts) != 1 || hosts[0]["hostname"] != "node-a" ||
		hosts[0]["up"] != true {
		t.Fatalf("hosts=%#v", hosts)
	}
	for _, key := range []string{"cpu_cores", "load1", "mem_used_percent", "disk_used_percent_max"} {
		value, exists := hosts[0][key]
		if !exists || value != nil {
			t.Fatalf("host field %q must be an explicit null: %#v", key, hosts[0])
		}
	}
	for _, legacy := range []string{"status", "load_1", "memory_usage_percent", "disk_usage_peak_percent"} {
		if _, exists := hosts[0][legacy]; exists {
			t.Fatalf("legacy host field %q leaked into DTO: %#v", legacy, hosts[0])
		}
	}
	for i := 0; i < 6; i++ {
		if got := (<-queries).Get("extra_filters[]"); got != `{project_id="p2"}` {
			t.Fatalf("host query %d filter=%q", i, got)
		}
	}
}

func TestHostsFiltersNaNInfGuardsZeroDivisionAndExcludesNonHostExporters(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "infra-nan", "password", "viewer")
	var sawMemZeroGuard, sawDiskZeroGuard atomic.Bool
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		if strings.Contains(expr, "node_memory_MemTotal_bytes") && strings.Contains(expr, "> 0") {
			sawMemZeroGuard.Store(true)
		}
		if strings.Contains(expr, "node_filesystem_size_bytes") && strings.Contains(expr, "> 0") {
			sawDiskZeroGuard.Store(true)
		}
		result := `[]`
		switch {
		case strings.Contains(expr, "node_uname_info"):
			result = `[{"metric":{"instance":"node:9100","nodename":"node-a"},"value":[1,"1"]}]`
		case strings.Contains(expr, "up{"):
			result = `[{"metric":{"instance":"node:9100"},"value":[1,"1"]},{"metric":{"instance":"redis:9121"},"value":[1,"1"]}]`
		case strings.Contains(expr, "node_load1"):
			result = `[{"metric":{"instance":"node:9100"},"value":[1,"NaN"]}]`
		case strings.Contains(expr, "node_memory_MemTotal_bytes"):
			result = `[{"metric":{"instance":"node:9100"},"value":[1,"+Inf"]}]`
		case strings.Contains(expr, "node_filesystem_size_bytes"):
			result = `[{"metric":{"instance":"node:9100"},"value":[1,"-Inf"]}]`
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
	}))
	defer metrics.Close()
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Metrics: metrics.URL}).App()

	req := httptest.NewRequest(http.MethodGet, "/api/hosts", nil)
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hosts status=%d want=200 (NaN/Inf must not cause 500)", resp.StatusCode)
	}
	var hosts []hostDTO
	if err := json.NewDecoder(resp.Body).Decode(&hosts); err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Instance != "node:9100" {
		t.Fatalf("expected only node exporter host, got %#v", hosts)
	}
	if hosts[0].Load1 != nil || hosts[0].MemoryUsedPercent != nil || hosts[0].DiskUsedPercentMax != nil {
		t.Fatalf("NaN/Inf values must be dropped as nil, got %#v", hosts[0])
	}
	if !sawMemZeroGuard.Load() || !sawDiskZeroGuard.Load() {
		t.Fatalf("missing > 0 zero-division guard: mem=%v disk=%v", sawMemZeroGuard.Load(), sawDiskZeroGuard.Load())
	}
}

func TestHostsAndScrapeTargetsRequireValidSessionProject(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "infra-admin", "password", "admin")
	service := scrape.NewService(scrape.Config{Store: store, ConfigPath: filepath.Join(t.TempDir(), "scrape.yml")})
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer metrics.Close()
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Scrapes: service, Metrics: metrics.URL,
	}).App()

	for _, endpoint := range []struct{ method, path string }{
		{http.MethodGet, "/api/hosts"},
		{http.MethodGet, "/api/scrape-targets"},
		{http.MethodGet, "/api/scrape-targets/1"},
		{http.MethodGet, "/api/scrape-targets/1/health"},
		{http.MethodDelete, "/api/scrape-targets/1"},
	} {
		req := httptest.NewRequest(endpoint.method, endpoint.path, nil)
		req.Header.Set("X-LO-Project", "nonexistent-project")
		req.AddCookie(admin)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s %s with unknown X-LO-Project returned 200; want rejected", endpoint.method, endpoint.path)
		}
	}
}

func TestDeleteProjectSyncsScrapeTargets(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	p2, err := store.CreateProject(context.Background(), "Temp", "temp-proj")
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "del-admin", "password", "admin")
	var reloads atomic.Int32
	cfgPath := filepath.Join(t.TempDir(), "scrape.yml")
	service := scrape.NewService(scrape.Config{
		Store:      store,
		ConfigPath: cfgPath,
		Reload: func() error {
			reloads.Add(1)
			return nil
		},
	})
	if _, err := service.Create(context.Background(), "temp-proj", scrape.Input{
		Name: "temp-exporter", URL: "http://exporter.example.com:9100/metrics", Interval: "15s",
	}); err != nil {
		t.Fatal(err)
	}
	beforeReloads := reloads.Load()
	app := newMetadataTestGateway(store, Config{
		Tokens:     store,
		Store:      store,
		Management: management.New(store),
		Scrapes:    service,
	}).App()

	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/projects/%d", p2.ID), nil)
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete project status=%d want=204", resp.StatusCode)
	}
	if reloads.Load() <= beforeReloads {
		t.Fatalf("expected vmagent reload after deleting project: before=%d after=%d", beforeReloads, reloads.Load())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "temp-proj") {
		t.Fatalf("deleted project target still present in scrape.yml:\n%s", data)
	}
}

func TestScrapeTargetHealthUsesTimestampAndLatestSeries(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "health-admin", "password", "admin")
	service := scrape.NewService(scrape.Config{Store: store, ConfigPath: filepath.Join(t.TempDir(), "scrape.yml")})
	created, err := service.Create(context.Background(), "default", scrape.Input{
		Name: "multi", URL: "http://exporter.example.com:9100/metrics", Interval: "15s",
	})
	if err != nil {
		t.Fatal(err)
	}
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		if strings.HasPrefix(expr, "timestamp(") {
			// Two series: old instance (down at t=1700000100), newer active instance (up at t=1700000500).
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[`+
				`{"metric":{"instance":"old:9100"},"value":[1700000999,"1700000100"]},`+
				`{"metric":{"instance":"new:9100"},"value":[1700000999,"1700000500"]}]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[`+
			`{"metric":{"instance":"old:9100"},"value":[1700000999,"0"]},`+
			`{"metric":{"instance":"new:9100"},"value":[1700000999,"1"]}]}}`)
	}))
	defer metrics.Close()
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Scrapes: service, Metrics: metrics.URL,
	}).App()

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/scrape-targets/%d/health", created.ID), nil)
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Status       string `json:"status"`
		LastScrapeAt string `json:"last_scrape_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "up" || body.LastScrapeAt != "2023-11-14T22:21:40Z" {
		t.Fatalf(
			"health=%+v want status=up last_scrape_at=2023-11-14T22:21:40Z (from timestamp() of latest series)",
			body,
		)
	}
}
