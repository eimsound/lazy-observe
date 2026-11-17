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

	alertpkg "github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/meta"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// TestLogsAlertE2E is opt-in because it starts every embedded storage binary.
// Run it through `mise run e2e`, or set LO_E2E_BINARY and
// LO_E2E_STORAGE_BIN_DIR when invoking go test directly.
func TestLogsAlertE2E(t *testing.T) {
	binary, binDir := os.Getenv("LO_E2E_BINARY"), os.Getenv("LO_E2E_STORAGE_BIN_DIR")
	if binary == "" || binDir == "" {
		t.Skip("LO_E2E_BINARY and LO_E2E_STORAGE_BIN_DIR are required")
	}

	dataDir := t.TempDir()
	metaDBURL := filepath.Join(dataDir, "meta", "lazy-observe.db")
	store, err := meta.OpenStore(context.Background(), metaDBURL)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(context.Background(), "Logs Alert", "logs-alert")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(context.Background(), "admin", "logs-alert-password", "admin"); err != nil {
		t.Fatal(err)
	}
	reader := e2eReader{token: userQueryToken(t, store, "logs-alert-reader"), project: project.Slug}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	notifications := make(chan alertpkg.Notification, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var notification alertpkg.Notification
		if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		notifications <- notification
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
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
	cmd.Env = append(os.Environ(),
		"LO_META_DB_URL="+metaDBURL,
		"LO_SELF_MONITORING_ENABLED=false",
		"LO_ALERT_GROUP_WAIT=250ms",
	)
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
				t.Errorf("server did not exit cleanly after SIGTERM: %v", err)
			}
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("server did not exit within 20s after SIGTERM")
		}
		if t.Failed() {
			t.Logf("server output:\n%s", output.String())
		}
	})

	client := &http.Client{Timeout: 5 * time.Second}
	waitFor(t, 40*time.Second, func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	admin := loginE2E(t, client, base, "admin", "logs-alert-password")

	var channel notificationChannelResponse
	jsonProjectAPI(t, client, http.MethodPost, base+"/api/channels", admin, project.Slug, map[string]any{
		"type": "webhook", "name": "logs-alert-e2e", "config": map[string]any{"url": webhook.URL},
	}, &channel, http.StatusCreated)

	marker := fmt.Sprintf("logs-alert-e2e-%d", time.Now().UnixNano())
	now := time.Now().UTC()
	// Keep one event in the prior complete minute and one in the current minute
	// so the native stats query remains deterministic across a minute boundary.
	postProto(
		t,
		client,
		base+"/v1/logs",
		token,
		logsAlertPayloadAt(t, marker, now.Truncate(time.Minute).Add(-30*time.Second)),
	)
	postProto(t, client, base+"/v1/logs", token, logsAlertPayloadAt(t, marker, now.Add(-time.Second)))
	query := `_msg:="` + marker + `"`
	queryURL := base + "/api/v1/logs/query?" + url.Values{"query": {query}}.Encode()
	waitFor(t, 20*time.Second, func() bool {
		return bytes.Contains(authorizedGet(t, client, queryURL, reader), []byte(marker))
	})

	ruleName := fmt.Sprintf("LogsAlertE2E%d", time.Now().UnixNano())
	expr := query + ` | stats count() as matched_logs | filter matched_logs:>0`
	var rule alertRuleResponse
	jsonProjectAPI(t, client, http.MethodPost, base+"/api/alert-rules", admin, project.Slug, map[string]any{
		"name":        ruleName,
		"signal":      "logs",
		"expr":        expr,
		"for":         "0s",
		"severity":    "critical",
		"channel_ids": []string{channel.ID},
	}, &rule, http.StatusCreated)
	if rule.Signal != "logs" {
		t.Fatalf("created rule signal=%q want logs", rule.Signal)
	}

	firing := waitNotification(t, notifications, 90*time.Second)
	if firing.Status != "firing" || firing.Project != project.Slug || firing.Rule != ruleName ||
		len(firing.Alerts) == 0 {
		t.Fatalf("unexpected LogsQL firing notification: %#v", firing)
	}
	if got := firing.Alerts[0].Labels["project_id"]; got != project.Slug {
		t.Fatalf("notification project_id=%q want %q", got, project.Slug)
	}

	var events []alertEventResponse
	jsonProjectAPI(
		t,
		client,
		http.MethodGet,
		base+"/api/alerts?status=firing",
		admin,
		project.Slug,
		nil,
		&events,
		http.StatusOK,
	)
	if len(events) != 1 || events[0].Rule != ruleName || events[0].Status != "firing" {
		t.Fatalf("alert service events=%#v", events)
	}
	t.Log(
		"LogsQL alert E2E: a tenant log triggered a type:vlogs rule through the internal datasource proxy and reached alert service",
	)
}

func logsAlertPayloadAt(t *testing.T, marker string, at time.Time) []byte {
	t.Helper()
	var request logspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(logsPayload(marker), &request); err != nil {
		t.Fatal(err)
	}
	request.ResourceLogs[0].ScopeLogs[0].LogRecords[0].TimeUnixNano = uint64(at.UnixNano())
	payload, err := proto.Marshal(&request)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
