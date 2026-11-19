package lazyobserve

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProfilingDefaults(t *testing.T) {
	cfg, err := normalizedConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profiling == nil || !*cfg.Profiling || cfg.ProfilingInterval != 60*time.Second {
		t.Fatalf("profiling defaults: enabled=%v interval=%s", cfg.Profiling, cfg.ProfilingInterval)
	}
}

func TestProfilingDisabledMakesNoProfileRequests(t *testing.T) {
	var profileRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/profiles" {
			profileRequests.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	disabled := false
	client, err := newClient(context.Background(), Config{
		DSN: server.URL + "/p/default@token", Metrics: true, Profiling: &disabled, ExportInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := profileRequests.Load(); got != 0 {
		t.Fatalf("profile requests=%d want=0", got)
	}
}

func TestEnsureGzip(t *testing.T) {
	compressed, err := ensureGzip([]byte("pprof payload"))
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) < 2 || compressed[0] != 0x1f || compressed[1] != 0x8b {
		t.Fatalf("not gzip: %x", compressed[:min(len(compressed), 4)])
	}
	second, err := ensureGzip(compressed)
	if err != nil || string(second) != string(compressed) {
		t.Fatalf("gzip changed on second pass: err=%v", err)
	}
}

func TestUploadProfileIncludesStableUploadID(t *testing.T) {
	var uploadID, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		uploadID = r.FormValue("upload_id")
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	client := &Client{
		cfg:          Config{ServiceName: "checkout"},
		profileURL:   server.URL + "/api/profiles",
		profileToken: "secret",
	}
	profile := collectedProfile{
		uploadID: newEventID(),
		typeName: "heap",
		start:    time.Now().UTC(),
		end:      time.Now().UTC().Add(time.Second),
		data:     []byte{0x1f, 0x8b, 0x00},
	}
	if err := client.uploadProfile(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	if uploadID != profile.uploadID || len(uploadID) != 32 || authorization != "Bearer secret" {
		t.Fatalf("upload_id=%q authorization=%q", uploadID, authorization)
	}
}

func TestCollectHeapAndGoroutineSnapshots(t *testing.T) {
	for _, profileType := range []string{"heap", "goroutine"} {
		collected, ok := collectSnapshotProfile(profileType, profileType)
		if !ok {
			t.Fatalf("collect %s profile failed", profileType)
		}
		if len(collected.uploadID) != 32 || collected.typeName != profileType || collected.start.IsZero() ||
			collected.end.Before(collected.start) {
			t.Fatalf("invalid %s metadata: %+v", profileType, collected)
		}
		zr, err := gzip.NewReader(bytes.NewReader(collected.data))
		if err != nil {
			t.Fatalf("open %s gzip: %v", profileType, err)
		}
		decoded, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read %s gzip: %v", profileType, err)
		}
		if err := zr.Close(); err != nil {
			t.Fatalf("close %s gzip: %v", profileType, err)
		}
		if len(decoded) == 0 {
			t.Fatalf("empty %s pprof payload", profileType)
		}
	}
}

// fakeCPUProfiler reports a CPU window of exactly elapsed: the first clock read
// (window start) returns base, every later read returns base+elapsed. It never
// touches the real pprof CPU profiler, so tests neither sleep nor collide with
// profiling loops of other clients in the same process.
func fakeCPUProfiler(elapsed time.Duration) *cpuProfiler {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var reads atomic.Int32
	return &cpuProfiler{
		now: func() time.Time {
			if reads.Add(1) == 1 {
				return base
			}
			return base.Add(elapsed)
		},
		start: func(w io.Writer) error {
			_, err := w.Write([]byte("fake cpu pprof"))
			return err
		},
		stop:      func() {},
		minWindow: profilingMinCancelledWindow,
	}
}

func profileTypes(profiles []collectedProfile) []string {
	types := make([]string, 0, len(profiles))
	for _, p := range profiles {
		types = append(types, p.typeName)
	}
	return types
}

func TestCollectProfilesCancelledCPUWindowNeedsMinimumDuration(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		wantCPU bool
	}{
		{"zero", 0, false},
		{"just under 5s", 4999 * time.Millisecond, false},
		{"exactly 5s", 5 * time.Second, true},
		{"7.5s", 7500 * time.Millisecond, true},
		{"9.9s", 9900 * time.Millisecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // shutdown arrives while the CPU window is open
			p := fakeCPUProfiler(tc.elapsed)
			var stops atomic.Int32
			p.stop = func() { stops.Add(1) }
			profiles := collectProfiles(ctx, profilingCPUWindow, p)
			// The CPU profiler must be stopped on the discard path too, or the
			// next window's StartCPUProfile fails with "already in use".
			if got := stops.Load(); got != 1 {
				t.Fatalf("stop calls=%d want=1", got)
			}
			want := []string{"heap", "goroutine"}
			if tc.wantCPU {
				want = []string{"cpu", "heap", "goroutine"}
			}
			if got := profileTypes(profiles); !slices.Equal(got, want) {
				t.Fatalf("profile types=%v want=%v", got, want)
			}
			if tc.wantCPU && profiles[0].end.Sub(profiles[0].start) != tc.elapsed {
				t.Fatalf("cpu window=%s want=%s", profiles[0].end.Sub(profiles[0].start), tc.elapsed)
			}
		})
	}
}

func TestCollectProfilesFullWindowIsNotSubjectToMinimum(t *testing.T) {
	// The window ran to completion (timer fired, ctx live): upload regardless
	// of length, as before.
	profiles := collectProfiles(context.Background(), time.Millisecond, fakeCPUProfiler(time.Millisecond))
	if got, want := profileTypes(profiles), []string{"cpu", "heap", "goroutine"}; !slices.Equal(got, want) {
		t.Fatalf("profile types=%v want=%v", got, want)
	}
}

func TestProfilingLoopShutdownShortWindowIsDiscardedWithoutDropCount(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		want    []string
	}{
		{"4.999s discarded", 4999 * time.Millisecond, []string{"goroutine", "heap"}},
		{"5s uploaded", 5 * time.Second, []string{"cpu", "goroutine", "heap"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var uploaded []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse multipart form: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				uploaded = append(uploaded, r.FormValue("type"))
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)

			cfg, err := normalizedConfig(Config{ServiceName: "checkout"})
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{
				cfg:          cfg,
				profileURL:   server.URL + "/api/profiles",
				profileToken: "secret",
				profileDone:  make(chan struct{}),
				cpuProfiler:  fakeCPUProfiler(tc.elapsed),
			}
			client.profileDelivery = newProfileDelivery(client)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			go client.profilingLoop(ctx)
			select {
			case <-client.profileDone:
			case <-time.After(5 * time.Second):
				t.Fatal("profilingLoop did not stop after cancel")
			}
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if err := client.profileDelivery.close(closeCtx); err != nil {
				t.Fatalf("close profile delivery: %v", err)
			}

			mu.Lock()
			got := slices.Clone(uploaded)
			mu.Unlock()
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("uploaded=%v want=%v", got, tc.want)
			}
			if dropped := client.Diagnostics().DroppedTelemetry; dropped != 0 {
				t.Fatalf("DroppedTelemetry=%d want=0: discarding a short window is not a delivery failure", dropped)
			}
		})
	}
}
