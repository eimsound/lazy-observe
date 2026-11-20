package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestInitialScrapeConfigMustSucceedBeforeStorageStarts(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.CreateProject(ctx, "Alpha", "alpha"); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	path, err := writeInitialScrapeConfig(ctx, store, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("initial scrape config: %v", err)
	}
	if _, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "alpha", Name: "broken", URL: "://invalid", Interval: "30s",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeInitialScrapeConfig(ctx, store, t.TempDir()); err == nil {
		t.Fatal("invalid target unexpectedly produced an initial scrape config")
	}
}

func TestHADoctorReportsMissingExternalState(t *testing.T) {
	err := run(
		[]string{
			"doctor",
			"--ha",
			"--standalone",
			"--metrics-query-url",
			"http://127.0.0.1:1",
			"--logs-url",
			"http://127.0.0.1:1",
			"--traces-url",
			"http://127.0.0.1:1",
		},
	)
	if err == nil {
		t.Fatal("doctor unexpectedly passed")
	}
	message := err.Error()
	for _, want := range []string{
		"redis.enabled",
		"meta.db_url",
		"blob.backend",
		"security.secret_key_file",
		"security.admin_password_file",
		"storage.metrics.ingest_url",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("doctor error %q does not contain %q", message, want)
		}
	}
}

func TestDoctorCheckEndpointEnforcesHTTPStatusAndTimeout(t *testing.T) {
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	if err := checkEndpoint(context.Background(), srv.Client(), srv.URL, "/health"); err == nil ||
		!strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("500 endpoint err=%v want HTTP 500 failure", err)
	}
	status = http.StatusNotFound
	if err := checkEndpoint(context.Background(), srv.Client(), srv.URL, "/health"); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404 endpoint err=%v want HTTP 404 failure", err)
	}
	status = http.StatusOK
	if err := checkEndpoint(context.Background(), srv.Client(), srv.URL, "/health"); err != nil {
		t.Fatalf("200 endpoint err=%v", err)
	}

	cfg := appconfig.Defaults()
	if got := doctorTimeout(cfg); got != time.Minute {
		t.Fatalf("doctorTimeout=%s want 1m (matching MigrationLockTimeout)", got)
	}
	cfg.Meta.MigrationLockTimeout = appconfig.Duration(5 * time.Second)
	if got := doctorTimeout(cfg); got != doctorMinTimeout {
		t.Fatalf("doctorTimeout=%s want %s", got, doctorMinTimeout)
	}
}

func TestOpenBlobStoreUsesDedicatedBlobsSubdirectory(t *testing.T) {
	dataDir := t.TempDir()
	cfg := appconfig.Defaults()
	cfg.DataDir = dataDir
	cfg.Blob.Backend = "local"
	store, err := openBlobStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "replay/demo/sess/0.json.gz", strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "blobs", "replay", "demo", "sess", "0.json.gz")); err != nil {
		t.Fatalf("blob not stored under dedicated blobs/ subdirectory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "replay")); !os.IsNotExist(err) {
		t.Fatalf("blob leaked into DataDir root: err=%v", err)
	}
}

func TestRootCommandHelpAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "help", args: []string{"--help"}, want: []string{"Available Commands:", "server", "admin", "doctor", "mcp"}},
		{
			name: "admin reset-password flags",
			args: []string{"admin", "reset-password", "--help"},
			want: []string{"--config", "--username", "--password-file", "initial-admin-password"},
		},
		{name: "server flags", args: []string{"server", "--help"}, want: []string{"--config", "--meta-db-url", "--self-monitoring", "--serve-web-ui"}},
		{name: "version", args: []string{"--version"}, want: []string{"dev", "commit none", "built unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			command := newRootCommand()
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs(tc.args)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("output %q does not contain %q", output.String(), want)
				}
			}
		})
	}
}

func TestCommandRequiredFlagsAndArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "doctor mode", args: []string{"doctor"}, want: "--ha is required"},
		{name: "MCP DSN", args: []string{"mcp"}, want: "--dsn is required"},
		{name: "server positional argument", args: []string{"server", "unexpected"}, want: "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want substring %q", err, tc.want)
			}
		})
	}
}
