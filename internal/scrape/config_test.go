package scrape

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
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

func TestTargetValidationHardeningPortSSRFMetaLabelsAndInterval(t *testing.T) {
	service := NewService(Config{PlatformPorts: []int{43123}})
	cases := []struct {
		name  string
		input Input
	}{
		{"leading-zero-gateway-port", Input{Name: "a", URL: "http://exporter.example.com:03100/metrics"}},
		{"leading-zero-vm-port", Input{Name: "a", URL: "http://exporter.example.com:018428/metrics"}},
		{"zero-port", Input{Name: "a", URL: "http://exporter.example.com:0/metrics"}},
		{"loopback-ssrf", Input{Name: "a", URL: "http://127.0.0.1:9100/metrics"}},
		{"ipv6-loopback-ssrf", Input{Name: "a", URL: "http://[::1]:9100/metrics"}},
		{"metadata-ssrf", Input{Name: "a", URL: "http://169.254.169.254:80/latest/meta-data/"}},
		{"private-ssrf", Input{Name: "a", URL: "http://10.0.0.5:9100/metrics"}},
		{
			"meta-label-address",
			Input{
				Name:   "a",
				URL:    "http://exporter.example.com:9100/metrics",
				Labels: map[string]string{"__address__": "127.0.0.1:18428"},
			},
		},
		{
			"meta-label-interval",
			Input{
				Name:   "a",
				URL:    "http://exporter.example.com:9100/metrics",
				Labels: map[string]string{"__scrape_interval__": "10ms"},
			},
		},
		{
			"meta-label-name",
			Input{
				Name:   "a",
				URL:    "http://exporter.example.com:9100/metrics",
				Labels: map[string]string{"__name__": "up"},
			},
		},
		{
			"whitespace-label",
			Input{
				Name:   "a",
				URL:    "http://exporter.example.com:9100/metrics",
				Labels: map[string]string{" project_id ": "other"},
			},
		},
		{
			"invalid-label-char",
			Input{
				Name:   "a",
				URL:    "http://exporter.example.com:9100/metrics",
				Labels: map[string]string{"env-name": "prod"},
			},
		},
		{
			"nanosecond-interval",
			Input{Name: "a", URL: "http://exporter.example.com:9100/metrics", Interval: "2000000000ns"},
		},
		{"signed-interval", Input{Name: "a", URL: "http://exporter.example.com:9100/metrics", Interval: "+10s"}},
		{"fractional-interval", Input{Name: "a", URL: "http://exporter.example.com:9100/metrics", Interval: "1.5s"}},
		{"excessive-interval", Input{Name: "a", URL: "http://exporter.example.com:9100/metrics", Interval: "2h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := service.validate(tc.input, false); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("input %#v accepted or wrong error: err=%v", tc.input, err)
			}
		})
	}
}

func TestWriteConfigSetsFollowRedirectsFalseLimitsAndSkipsCorruptRow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, _ = store.CreateProject(ctx, "One", "p1")
	_, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "p1", Name: "corrupt", URL: "not-a-valid-url", Interval: "15s",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "p1", Name: "healthy", URL: "https://exporter.example.com:9443/metrics", Interval: "1s",
		Labels: map[string]string{"env": "prod", "__address__": "127.0.0.1:18428"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scrape.yml")
	if err := WriteConfig(ctx, store, path); err != nil {
		t.Fatalf("corrupt row must not block WriteConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(data)
	for _, want := range []string{
		"job_name: " + JobName(valid),
		"follow_redirects: false",
		"scrape_timeout: 1s",
		"sample_limit:",
		"label_limit:",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("config missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "__address__") {
		t.Errorf("meta label leaked into scrape.yml:\n%s", rendered)
	}
}

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

func TestServiceQuotaRollbackConcurrentPatchAndDNSBlocking(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, _ = store.CreateProject(ctx, "One", "p1")

	resolver := staticResolver{
		"internal.example.com": {netip.MustParseAddr("169.254.169.254")},
		"public.example.com":   {netip.MustParseAddr("93.184.216.34")},
	}
	var failReload bool
	cfgPath := filepath.Join(t.TempDir(), "scrape.yml")
	service := NewService(Config{
		Store:                store,
		ConfigPath:           cfgPath,
		MaxTargetsPerProject: 2,
		Resolver:             resolver,
		OutboundPolicy:       outbound.Policy{},
		Reload: func() error {
			if failReload {
				return errors.New("reload failed")
			}
			return nil
		},
	})

	if _, err := service.Create(ctx, "p1", Input{
		Name: "dns-ssrf", URL: "http://internal.example.com:9100/metrics", Interval: "15s",
	}); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("DNS resolving to metadata IP should be rejected, got err=%v", err)
	}

	failReload = true
	if _, err := service.Create(ctx, "p1", Input{
		Name: "rolled-back", URL: "http://public.example.com:9100/metrics", Interval: "15s",
	}); err == nil {
		t.Fatal("expected create to fail when reload fails")
	}
	failReload = false
	if list, err := service.List(ctx, "p1"); err != nil || len(list) != 0 {
		t.Fatalf("failed reload should roll back DB insert, got list=%+v err=%v", list, err)
	}

	first, err := service.Create(ctx, "p1", Input{
		Name: "t1", URL: "http://public.example.com:9100/metrics", Interval: "15s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, "p1", Input{
		Name: "t2", URL: "http://public.example.com:9101/metrics", Interval: "15s",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, "p1", Input{
		Name: "t3", URL: "http://public.example.com:9102/metrics", Interval: "15s",
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("3rd target err=%v want ErrQuotaExceeded", err)
	}

	// Concurrent PATCHes modifying disjoint fields must not clobber each other.
	disabled := false
	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() {
		defer wg.Done()
		_, errA = service.Update(ctx, "p1", first.ID, Input{Name: "t1-renamed"})
	}()
	go func() {
		defer wg.Done()
		_, errB = service.Update(ctx, "p1", first.ID, Input{Enabled: &disabled})
	}()
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("concurrent PATCH errors: errA=%v errB=%v", errA, errB)
	}
	got, err := service.Get(ctx, "p1", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "t1-renamed" || got.Enabled {
		t.Fatalf("lost update under concurrent PATCH: got=%+v want name=t1-renamed enabled=false", got)
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

func TestRenderPureFunctionDeterministicAndFiltersDisabledOrCorrupt(t *testing.T) {
	input := []meta.ScrapeTargetView{
		{
			ID:       20,
			Project:  meta.ProjectInfo{ID: 2, Slug: "beta"},
			Name:     "b",
			URL:      "https://beta.example.com:9443/custom?module=http",
			Interval: "1m",
			Labels:   map[string]string{"env": "staging", "__address__": "127.0.0.1:18428", "project_id": "spoof"},
			Enabled:  true,
		},
		{
			ID:       10,
			Project:  meta.ProjectInfo{ID: 1, Slug: "alpha"},
			Name:     "a",
			URL:      "http://alpha.example.com:9100/metrics",
			Interval: "15s",
			Labels:   map[string]string{"role": "db"},
			Enabled:  true,
		},
		{
			ID:       15,
			Project:  meta.ProjectInfo{ID: 1, Slug: "alpha"},
			Name:     "disabled",
			URL:      "http://disabled.example.com:9100/metrics",
			Interval: "15s",
			Enabled:  false,
		},
		{
			ID:       16,
			Project:  meta.ProjectInfo{ID: 1, Slug: "alpha"},
			Name:     "corrupt-url",
			URL:      "://bad",
			Interval: "15s",
			Enabled:  true,
		},
		{
			ID:       17,
			Project:  meta.ProjectInfo{ID: 1, Slug: ""},
			Name:     "missing-project",
			URL:      "http://orphan.example.com:9100/metrics",
			Interval: "15s",
			Enabled:  true,
		},
	}

	first, err := Render(input)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Verify caller slice was not mutated and output is deterministic.
	if input[0].ID != 20 {
		t.Fatalf("Render mutated input slice order: first ID=%d want=20", input[0].ID)
	}
	second, err := Render([]meta.ScrapeTargetView{input[1], input[0], input[2], input[3], input[4]})
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("Render is non-deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	rendered := string(first)
	idxA := strings.Index(rendered, "job_name: scrape-10")
	idxB := strings.Index(rendered, "job_name: scrape-20")
	if idxA < 0 || idxB < 0 || idxA >= idxB {
		t.Fatalf("expected deterministic ID ordering scrape-10 before scrape-20:\n%s", rendered)
	}
	for _, want := range []string{
		"follow_redirects: false",
		"project_id: alpha",
		"project_id: beta",
		"env: staging",
		"role: db",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("Render output missing %q:\n%s", want, rendered)
		}
	}
	for _, forbidden := range []string{"disabled.example.com", "orphan.example.com", "__address__", "spoof"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("Render output leaked %q:\n%s", forbidden, rendered)
		}
	}

	empty, err := Render(nil)
	if err != nil {
		t.Fatalf("Render(nil): %v", err)
	}
	if string(empty) != emptyScrapeConfigYAML {
		t.Fatalf("Render(nil)=%q want=%q", empty, emptyScrapeConfigYAML)
	}
}

func TestReconcilerEventDrivenWakeupBackoffRetryAndClosedLoopStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := openTestStore(t)
	if _, err := store.CreateProject(ctx, "Alpha", "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "alpha", Name: "node", URL: "http://exporter.example.com:9100/metrics", Interval: "15s",
	}); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "scrape.yml")
	var attempts atomic.Int32
	var failUntil atomic.Int32
	failUntil.Store(2) // First two reload attempts fail; 3rd succeeds.
	fixedNow := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	svc := NewService(Config{
		Store:          store,
		ConfigPath:     cfgPath,
		RetryBaseDelay: 10 * time.Millisecond,
		RetryMaxDelay:  40 * time.Millisecond,
		Now:            func() time.Time { return fixedNow },
		Reload: func() error {
			n := attempts.Add(1)
			if n <= failUntil.Load() {
				return errors.New("vmagent /-/reload status=503")
			}
			return nil
		},
	})

	initial := svc.Status()
	if !initial.Leader || initial.DesiredRevision != 0 || initial.AppliedRevision != 0 || !initial.InSync ||
		initial.LastSyncAt != nil || initial.LastSyncError != "" {
		t.Fatalf("unexpected initial status: %+v", initial)
	}

	// Burst of NotifyChange calls must be non-blocking and advance desiredRevision.
	for range 5 {
		svc.NotifyChange()
	}
	pending := svc.Status()
	if pending.DesiredRevision != 5 || pending.AppliedRevision != 0 || pending.InSync {
		t.Fatalf("unexpected pending status: %+v", pending)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunReconciler(ctx, nil, 0)
	}()

	// Wait until backoff retry succeeds on attempt 3 and closes the loop.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := svc.Status()
		if st.InSync && st.AppliedRevision == 5 && st.LastSyncError == "" && st.LastSyncAt != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := svc.Status()
	if !st.InSync || st.AppliedRevision != 5 || st.DesiredRevision != 5 || st.LastSyncError != "" ||
		st.LastSyncAt == nil || !st.LastSyncAt.Equal(fixedNow) {
		t.Fatalf("reconciler did not converge after backoff retry: status=%+v attempts=%d", st, attempts.Load())
	}
	if attempts.Load() != 3 {
		t.Fatalf("reload attempts=%d want=3", attempts.Load())
	}

	// Deleting a project triggers NotifyProjectDeleted -> even if reload fails transiently,
	// reconciler retries and clears the deleted project's target from scrape.yml.
	p2, err := store.CreateProject(ctx, "Beta", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: "beta", Name: "beta-node", URL: "http://beta.example.com:9100/metrics", Interval: "15s",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Sync(ctx); err != nil {
		t.Fatalf("sync beta target: %v", err)
	}
	if err := store.DeleteProject(ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	failUntil.Store(attempts.Load() + 1) // Fail the immediate sync once, succeed on retry.
	if err := svc.NotifyProjectDeleted(ctx); err == nil {
		t.Fatal("expected immediate NotifyProjectDeleted reload failure")
	}
	if failed := svc.Status(); failed.InSync || failed.LastSyncError == "" {
		t.Fatalf("expected out-of-sync error state after failed reload: %+v", failed)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cur := svc.Status(); cur.InSync && cur.LastSyncError == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cur := svc.Status(); !cur.InSync || cur.LastSyncError != "" {
		t.Fatalf("reconciler did not recover after project delete failure: %+v", cur)
	}
	rendered, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "beta") {
		t.Fatalf("deleted project target remained in scrape.yml:\n%s", rendered)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunReconciler did not stop on context cancellation")
	}
}
