package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

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
