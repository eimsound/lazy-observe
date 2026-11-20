package scrape

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
)

func openTestStore(t *testing.T) *meta.Store {
	t.Helper()
	store, err := meta.OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestWriteConfigMultipleProjectsAndLabels(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, _ = store.CreateProject(ctx, "One", "p1")
	_, _ = store.CreateProject(ctx, "Two", "p2")
	a, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "p1", Name: "node-a", URL: "http://127.0.0.1:9100/metrics?module=node", Interval: "15s",
		Labels: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "p2", Name: "node-b", URL: "https://example.test:9443/custom", Interval: "30s",
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	_, _ = store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "p2", Name: "off", URL: "http://example.test:9100/metrics", Interval: "30s", Enabled: &disabled,
	})
	path := filepath.Join(t.TempDir(), "scrape.yml")
	if err := WriteConfig(ctx, store, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(data)
	for _, want := range []string{"job_name: " + JobName(a), "job_name: " + JobName(b), "project_id: p1", "project_id: p2", "scrape_job: " + JobName(a), "env: prod", "metrics_path: /custom", "module:"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("config missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "off") || strings.Contains(rendered, "example.test:9100") {
		t.Errorf("disabled target rendered:\n%s", rendered)
	}
}

func TestTargetValidationRejectsLoopAndReservedLabels(t *testing.T) {
	service := NewService(Config{PlatformPorts: []int{43123}})
	for _, input := range []Input{
		{Name: "loop", URL: "http://localhost:3100/metrics"},
		{Name: "dynamic-loop", URL: "http://127.0.0.1:43123/metrics"},
		{Name: "bad-scheme", URL: "ftp://host:9100/metrics"},
		{Name: "reserved", URL: "http://host:9100/metrics", Labels: map[string]string{"project_id": "escape"}},
	} {
		if err := service.validate(input, false); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("input %#v: err=%v", input, err)
		}
	}
}

func TestServiceConcurrentSyncIsSerializedAndLeavesNoTempFiles(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.CreateProject(ctx, "Alpha", "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "alpha", Name: "node", URL: "http://127.0.0.1:9100/metrics", Interval: "15s",
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "scrape.yml")
	var reloads atomic.Int32
	svc := NewService(Config{
		Store:      store,
		ConfigPath: path,
		Reload: func() error {
			reloads.Add(1)
			return nil
		},
	})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Sync(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Sync failed: %v", err)
	}
	if reloads.Load() != 12 {
		t.Fatalf("reloads=%d want 12", reloads.Load())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "scrape.yml" {
		t.Fatalf("unexpected files in scrape config dir: %v", entries)
	}
}
