package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
)

// TestScrapeManagementE2E is opt-in because it starts all embedded release binaries.
// Run with LO_E2E_BINARY and LO_E2E_STORAGE_BIN_DIR set.
func TestScrapeManagementE2E(t *testing.T) {
	binary, binDir := os.Getenv("LO_E2E_BINARY"), os.Getenv("LO_E2E_STORAGE_BIN_DIR")
	if binary == "" || binDir == "" {
		t.Skip("LO_E2E_BINARY and LO_E2E_STORAGE_BIN_DIR are required")
	}
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `# TYPE node_uname_info gauge
node_uname_info{nodename="fake-host",sysname="Linux",release="test",machine="arm64"} 1
# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 100
node_cpu_seconds_total{cpu="0",mode="user"} 20
# TYPE node_load1 gauge
node_load1 0.5
# TYPE node_memory_MemAvailable_bytes gauge
node_memory_MemAvailable_bytes 750
# TYPE node_memory_MemTotal_bytes gauge
node_memory_MemTotal_bytes 1000
# TYPE node_filesystem_avail_bytes gauge
node_filesystem_avail_bytes{mountpoint="/",fstype="ext4"} 600
# TYPE node_filesystem_size_bytes gauge
node_filesystem_size_bytes{mountpoint="/",fstype="ext4"} 1000
`)
	}))
	defer exporter.Close()

	dataDir := t.TempDir()
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(dataDir, "meta", "lazy-observe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defaultProject, _ := store.CreateProject(ctx, "Default", "default")
	p2, _ := store.CreateProject(ctx, "P2", "p2")
	_, _ = store.CreateUser(ctx, "admin", "admin-password", "admin")
	_, _ = store.CreateUser(ctx, "viewer", "viewer-password", "viewer")
	readerToken := userQueryToken(t, store, "scrape-reader")
	defaultToken := e2eReader{token: readerToken, project: defaultProject.Slug}
	p2Token := e2eReader{token: readerToken, project: p2.Slug}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	var output lockedBuffer
	cmd := exec.Command(
		binary,
		"server",
		"--listen",
		addr,
		"--storage-bin-dir",
		binDir,
		"--data-dir",
		dataDir,
		"--storage-startup-timeout",
		"30s",
	)
	cmd.Env = append(os.Environ(), "LO_SECURITY_OUTBOUND_ALLOWLIST=127.0.0.1/32")
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server exit: %v", err)
			}
		case <-time.After(25 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("server shutdown timed out")
		}
		if t.Failed() {
			t.Logf("server output:\n%s", output.String())
		}
	})
	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	waitFor(t, 40*time.Second, func() bool {
		resp, e := client.Get(base + "/healthz")
		if e != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	admin := loginE2E(t, client, base, "admin", "admin-password")
	viewer := loginE2E(t, client, base, "viewer", "viewer-password")

	jsonProjectAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/scrape-targets",
		viewer,
		"p2",
		map[string]any{"name": "denied", "url": exporter.URL + "/metrics"},
		nil,
		http.StatusForbidden,
	)
	var target scrapeTargetResponse
	jsonProjectAPI(
		t,
		client,
		http.MethodPost,
		base+"/api/scrape-targets",
		admin,
		"p2",
		map[string]any{
			"name":     "fake-node",
			"url":      exporter.URL + "/metrics",
			"interval": "1s",
			"labels":   map[string]string{"environment": "e2e"},
		},
		&target,
		http.StatusCreated,
	)
	job := "scrape-" + target.ID
	query := func(token e2eReader, expr string) []byte {
		return authorizedGet(t, client, base+"/api/v1/query?"+url.Values{"query": {expr}}.Encode(), token)
	}
	waitFor(
		t,
		30*time.Second,
		func() bool { return metricValuesAre(query(p2Token, `up{scrape_job="`+job+`",project_id="p2"}`), "1") },
	)
	if bytes.Contains(query(defaultToken, `up{scrape_job="`+job+`"}`), []byte(`"scrape_job":"`+job+`"`)) {
		t.Fatal("default project saw p2 scrape target")
	}

	var health struct {
		Status       string     `json:"status"`
		LastScrapeAt *time.Time `json:"last_scrape_at"`
	}
	jsonProjectAPI(
		t,
		client,
		http.MethodGet,
		fmt.Sprintf("%s/api/scrape-targets/%s/health", base, target.ID),
		admin,
		"p2",
		nil,
		&health,
		http.StatusOK,
	)
	if health.Status != "up" || health.LastScrapeAt == nil {
		t.Fatalf("health=%#v", health)
	}
	var hosts []hostDTO
	jsonProjectAPI(t, client, http.MethodGet, base+"/api/hosts", admin, "p2", nil, &hosts, http.StatusOK)
	if len(hosts) != 1 || hosts[0].Hostname != "fake-host" || hosts[0].Up == nil || !*hosts[0].Up {
		t.Fatalf("hosts=%#v", hosts)
	}

	jsonProjectAPI(
		t,
		client,
		http.MethodPatch,
		fmt.Sprintf("%s/api/scrape-targets/%s", base, target.ID),
		admin,
		"p2",
		map[string]any{"enabled": false},
		&target,
		http.StatusOK,
	)
	// The scrape interval is 1s, but a sample scraped just before the PATCH can
	// become queryable a few seconds later under CI load. Require the timestamp
	// to hold for several intervals so late-visible in-flight samples settle
	// before the "stays stopped" assertion below.
	type scrapeE2EQueryResponse struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	const stableWindow = 5 * time.Second
	var before []byte
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		first := query(p2Token, `timestamp(up{scrape_job="`+job+`"})`)
		time.Sleep(stableWindow)
		second := query(p2Token, `timestamp(up{scrape_job="`+job+`"})`)
		var fq, sq scrapeE2EQueryResponse
		_ = json.Unmarshal(first, &fq)
		_ = json.Unmarshal(second, &sq)
		if len(fq.Data.Result) > 0 && len(sq.Data.Result) > 0 &&
			string(fq.Data.Result[0].Value[1]) == string(sq.Data.Result[0].Value[1]) {
			before = second
			break
		}
	}
	if before == nil {
		t.Fatal("disabled target did not stop producing samples")
	}
	time.Sleep(3 * time.Second)
	after := query(p2Token, `timestamp(up{scrape_job="`+job+`"})`)
	var bq, aq scrapeE2EQueryResponse
	_ = json.Unmarshal(before, &bq)
	_ = json.Unmarshal(after, &aq)
	if len(bq.Data.Result) == 0 || len(aq.Data.Result) == 0 ||
		string(bq.Data.Result[0].Value[1]) != string(aq.Data.Result[0].Value[1]) {
		t.Fatalf("disabled target timestamp changed: before=%s after=%s", before, after)
	}
}
