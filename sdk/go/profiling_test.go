package lazyobserve

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
