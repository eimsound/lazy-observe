package errortrack_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
)

type testLogsBackend struct {
	baseURL    string
	ingestURL  string
	headerName string
}

func (b *testLogsBackend) LogsBase() string { return b.baseURL }

func (b *testLogsBackend) LogsIngestBase() string { return b.ingestURL }

func (b *testLogsBackend) SetTenantHeaders(h http.Header, projectID int) {
	name := b.headerName
	if name == "" {
		name = "AccountID"
	}
	h.Set(name, strconv.Itoa(projectID))
}

type recordedVLogsRequest struct {
	accountID string
	path      string
	bytesLen  int
	lines     []map[string]string
}

func TestVictoriaLogsWriterBatchGroupsByProjectAndBoundsChunks(t *testing.T) {
	var mu sync.Mutex
	var requests []recordedVLogsRequest
	var newConns atomic.Int64

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
		scanner.Buffer(make([]byte, 64<<10), 10<<20)
		var lines []map[string]string
		for scanner.Scan() {
			var rec map[string]string
			if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
				t.Errorf("invalid NDJSON line: %v", err)
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			lines = append(lines, rec)
		}
		mu.Lock()
		requests = append(requests, recordedVLogsRequest{
			accountID: r.Header.Get("AccountID"),
			path:      r.URL.Path,
			bytesLen:  buf.Len(),
			lines:     lines,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		// Return a non-empty 2xx body to verify keep-alive body draining.
		_, _ = w.Write([]byte(strings.Repeat("ok", 1024)))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	writer := errortrack.NewVictoriaLogsWriter(
		&testLogsBackend{baseURL: srv.URL},
		errortrack.NewVictoriaLogsHTTPClient(),
	)

	now := time.Date(2026, 11, 23, 1, 2, 3, 0, time.UTC)
	mkPrepared := func(projectID int, slug, eventID string, rawStack *errortrack.Stacktrace, fp string, msgSize int) errortrack.PreparedEvent {
		msg := "err"
		if msgSize > 0 {
			msg = strings.Repeat("x", msgSize)
		}
		ev := errortrack.Event{
			EventID:   eventID,
			Timestamp: now,
			Platform:  "javascript",
			Level:     "error",
			Message:   msg,
			Exception: errortrack.Exception{
				Type:  "TypeError",
				Value: "boom",
				Stacktrace: errortrack.Stacktrace{
					Frames: []errortrack.Frame{{Function: "symFn", Filename: "src/app.ts", InApp: true}},
				},
			},
		}
		raw, _ := json.Marshal(ev)
		return errortrack.PreparedEvent{
			Project:       errortrack.Project{ID: projectID, Slug: slug},
			Raw:           raw,
			Event:         ev,
			RawStacktrace: rawStack,
			Fingerprint:   fp,
		}
	}

	rawOrig := errortrack.Stacktrace{
		Frames: []errortrack.Frame{{Function: "minFn", Filename: "app.min.js", InApp: true}},
	}
	batch := make([]errortrack.PreparedEvent, 0, 23)
	// 18 events for project 1 -> must split into 16 + 2 lines.
	for i := range 18 {
		var rs *errortrack.Stacktrace
		fp := "precomputed-fp-1"
		if i == 0 {
			rs = &rawOrig
		}
		batch = append(batch, mkPrepared(1, "p1", "p1-"+strconv.Itoa(i), rs, fp, 0))
	}
	// 2 interleaved events for project 2 -> must never mix with project 1.
	batch = append(batch, mkPrepared(2, "p2", "p2-0", nil, "precomputed-fp-2", 0))
	batch = append(batch, mkPrepared(2, "p2", "p2-1", nil, "precomputed-fp-2", 0))
	// 3 large events for project 3 (~1.8 MiB message -> >3.6 MiB encoded each) -> 2 events exceed 4 MiB so 3 requests or 2+1.
	for i := range 3 {
		batch = append(batch, mkPrepared(3, "p3", "p3-"+strconv.Itoa(i), nil, "precomputed-fp-3", 900<<10))
	}

	errs := writer.WriteErrorEvents(context.Background(), batch)
	for idx, err := range errs {
		if err != nil {
			t.Fatalf("WriteErrorEvents[%d] unexpected error: %v", idx, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 5 {
		t.Fatalf("expected at least 5 grouped/bounded requests, got %d", len(requests))
	}
	var p1Counts, p2Counts, p3Counts []int
	for _, req := range requests {
		if req.path != "/insert/jsonline" {
			t.Fatalf("unexpected path %q", req.path)
		}
		if len(req.lines) > 16 {
			t.Fatalf("batch exceeded 16 lines: %d", len(req.lines))
		}
		if len(req.lines) > 1 && req.bytesLen > 4<<20 {
			t.Fatalf("multi-line batch exceeded 4 MiB: %d", req.bytesLen)
		}
		for _, line := range req.lines {
			if !strings.HasPrefix(line["event_id"], "p"+req.accountID+"-") {
				t.Fatalf("cross-tenant leak: account=%s event_id=%s", req.accountID, line["event_id"])
			}
		}
		switch req.accountID {
		case "1":
			p1Counts = append(p1Counts, len(req.lines))
			if req.lines[0]["event_id"] == "p1-0" {
				if !strings.Contains(req.lines[0]["raw_stacktrace"], "minFn") ||
					!strings.Contains(req.lines[0]["symbolicated_stacktrace"], "symFn") {
					t.Fatalf("expected distinct raw vs symbolicated stacktrace, got %#v", req.lines[0])
				}
				if req.lines[0]["fingerprint"] != "precomputed-fp-1" {
					t.Fatalf("fingerprint=%q want precomputed-fp-1", req.lines[0]["fingerprint"])
				}
			}
		case "2":
			p2Counts = append(p2Counts, len(req.lines))
		case "3":
			p3Counts = append(p3Counts, len(req.lines))
		default:
			t.Fatalf("unexpected tenant header %q", req.accountID)
		}
	}
	if len(p1Counts) != 2 || p1Counts[0] != 16 || p1Counts[1] != 2 {
		t.Fatalf("project 1 chunk line counts=%v want [16 2]", p1Counts)
	}
	if len(p2Counts) != 1 || p2Counts[0] != 2 {
		t.Fatalf("project 2 chunk line counts=%v want [2]", p2Counts)
	}
	totalP3 := 0
	for _, n := range p3Counts {
		totalP3 += n
	}
	if len(p3Counts) < 2 || totalP3 != 3 {
		t.Fatalf("project 3 should split by maxBatchBytes: counts=%v", p3Counts)
	}
	if gotConns := newConns.Load(); gotConns != 1 {
		t.Fatalf("expected 2xx response drain to keep 1 persistent TCP connection, got %d", gotConns)
	}
}

func TestVictoriaLogsWriterBatch4xxFallbackIsolatesBadEvent(t *testing.T) {
	var requestCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		if strings.Contains(buf.String(), `"bad-ev"`) {
			http.Error(w, "invalid line", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	writer := errortrack.NewVictoriaLogsWriter(&testLogsBackend{baseURL: srv.URL}, srv.Client())
	now := time.Now().UTC()
	makeEvent := func(id string) errortrack.PreparedEvent {
		ev := errortrack.Event{EventID: id, Timestamp: now, Platform: "go", Level: "error", Message: id}
		raw, _ := json.Marshal(ev)
		return errortrack.PreparedEvent{
			Project:     errortrack.Project{ID: 7, Slug: "alpha"},
			Raw:         raw,
			Event:       ev,
			Fingerprint: "fp-" + id,
		}
	}

	errs := writer.WriteErrorEvents(context.Background(), []errortrack.PreparedEvent{
		makeEvent("good-1"),
		makeEvent("bad-ev"),
		makeEvent("good-2"),
		makeEvent("good-3"),
	})
	if len(errs) != 4 {
		t.Fatalf("len(errs)=%d want 4", len(errs))
	}
	if errs[0] != nil || errs[2] != nil || errs[3] != nil {
		t.Fatalf("expected healthy events to succeed on 4xx fallback, got errs=%v", errs)
	}
	if errs[1] == nil || !strings.Contains(errs[1].Error(), "400") {
		t.Fatalf("expected 400 error on bad event, got %v", errs[1])
	}
	if got := requestCount.Load(); got != 5 {
		t.Fatalf("requestCount=%d want 5 (1 failed batch + 4 fallback singles)", got)
	}
}

func TestVictoriaLogsWriterBatch5xxPreservesRetryWithoutFanout(t *testing.T) {
	var requestCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		http.Error(w, "storage overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	writer := errortrack.NewVictoriaLogsWriter(&testLogsBackend{baseURL: srv.URL}, srv.Client())
	now := time.Now().UTC()
	batch := make([]errortrack.PreparedEvent, 4)
	for i := range batch {
		id := "ev-" + strconv.Itoa(i)
		ev := errortrack.Event{EventID: id, Timestamp: now, Platform: "go", Level: "error", Message: id}
		raw, _ := json.Marshal(ev)
		batch[i] = errortrack.PreparedEvent{
			Project:     errortrack.Project{ID: 7, Slug: "alpha"},
			Raw:         raw,
			Event:       ev,
			Fingerprint: "fp-" + id,
		}
	}

	errs := writer.WriteErrorEvents(context.Background(), batch)
	if len(errs) != 4 {
		t.Fatalf("len(errs)=%d want 4", len(errs))
	}
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("errs[%d]=%v want 503", i, err)
		}
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("expected 5xx to avoid per-event fanout, got %d requests", got)
	}
}

func TestVictoriaLogsHTTPClientUsesDedicatedPooledTransport(t *testing.T) {
	client := errortrack.NewVictoriaLogsHTTPClient()
	if client == nil || client == http.DefaultClient {
		t.Fatal("expected dedicated *http.Client, not nil or http.DefaultClient")
	}
	if client.Timeout <= 0 {
		t.Fatalf("expected positive Timeout, got %s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport {
		t.Fatalf("expected cloned *http.Transport, got %T", client.Transport)
	}
	if transport.MaxIdleConnsPerHost < 16 {
		t.Fatalf("MaxIdleConnsPerHost=%d want >= 16", transport.MaxIdleConnsPerHost)
	}
	if client.CheckRedirect == nil || !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("expected CheckRedirect to disable redirect following")
	}
}
