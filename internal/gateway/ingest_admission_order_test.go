package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
)

// Endpoint anti-abuse quotas are admitted from the narrowest key to the
// broadest (client IP, then replay session, then project), so a request
// rejected by a narrower quota never consumes the shared project quota (#385).

const (
	admissionOrderFloodIP = "203.0.113.10"
	admissionOrderOtherIP = "203.0.113.20"
	admissionOrderFloods  = 5
)

type admissionOrderFixture struct {
	store *meta.Store
	blobs blob.Store
	token string
	now   time.Time
}

func newAdmissionOrderFixture(t *testing.T) admissionOrderFixture {
	t.Helper()
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	token, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return admissionOrderFixture{
		store: store,
		blobs: blobs,
		token: token,
		now:   time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
}

func (f admissionOrderFixture) gateway(profiles bool) *Gateway {
	cfg := Config{
		Tokens: f.store, Store: f.store, Blobs: f.blobs,
		// Trust the Fiber app.Test peer so X-Forwarded-For selects the client IP.
		TrustedProxies: []string{"0.0.0.0/32", "127.0.0.0/8"},
		Now:            func() time.Time { return f.now },
	}
	if profiles {
		cfg.Profiles = f.store
	} else {
		cfg.Replays = f.store
	}
	return newMetadataTestGateway(f.store, cfg)
}

func postReplayFrom(
	t *testing.T,
	app httpTestApp,
	token, clientIP, sessionID string,
	seq int,
	at time.Time,
) *http.Response {
	t.Helper()
	segment := []byte(fmt.Sprintf(
		`[{"type":4,"data":{"href":"https://example.test/checkout"},"timestamp":%d}]`,
		at.Add(-time.Minute).UnixMilli(),
	))
	target := "/api/replay/segments?" + url.Values{"session_id": {sessionID}, "seq": {strconv.Itoa(seq)}}.Encode()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(segment))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://app.example.test")
	req.Header.Set("X-LO-Replay-Compression", "identity")
	req.Header.Set("X-Forwarded-For", clientIP)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postProfileFrom(t *testing.T, app httpTestApp, token, clientIP string, data []byte) *http.Response {
	t.Helper()
	uploadID, err := randomProfileID()
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("profile", "profile.pb.gz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = w.WriteField("type", "cpu")
	_ = w.WriteField("service", "svc")
	_ = w.WriteField("start", "2026-07-17T11:59:00Z")
	_ = w.WriteField("end", "2026-07-17T11:59:10Z")
	_ = w.WriteField("upload_id", strings.ReplaceAll(uploadID, "-", ""))
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/profiles", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Forwarded-For", clientIP)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// expectIngestRejected asserts the 429 contract shared by every endpoint
// anti-abuse quota: positive Retry-After and {"error":"<message>"}.
func expectIngestRejected(t *testing.T, resp *http.Response, message, label string) {
	t.Helper()
	body := readBody(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("%s status=%d want=429 body=%s", label, resp.StatusCode, body)
	}
	if retry, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || retry < 1 {
		t.Fatalf("%s Retry-After=%q want positive integer", label, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(string(body), message) {
		t.Fatalf("%s body=%s want error %q", label, body, message)
	}
}

func expectIngestStatus(t *testing.T, resp *http.Response, want int, label string) {
	t.Helper()
	body := readBody(resp)
	if resp.StatusCode != want {
		t.Fatalf("%s status=%d want=%d body=%s", label, resp.StatusCode, want, body)
	}
}

func TestReplayIngestIPFloodDoesNotExhaustProjectQuota(t *testing.T) {
	f := newAdmissionOrderFixture(t)
	gw := f.gateway(false)
	gw.replayGuard.project = newProjectIngestWindow(2, gw.now)
	gw.replayGuard.ip = newLocalIngestWindow(1, gw.now)
	app := gw.App()

	resp := postReplayFrom(t, app, f.token, admissionOrderFloodIP, "flood-ip", 0, f.now)
	expectIngestStatus(t, resp, http.StatusAccepted, "flooding IP first segment")
	for seq := 1; seq <= admissionOrderFloods; seq++ {
		resp = postReplayFrom(t, app, f.token, admissionOrderFloodIP, "flood-ip", seq, f.now)
		expectIngestRejected(t, resp, "replay ingest rate limit exceeded", "flooding IP over quota")
	}
	resp = postReplayFrom(t, app, f.token, admissionOrderOtherIP, "other-ip", 0, f.now)
	expectIngestStatus(t, resp, http.StatusAccepted, "other IP after flood")
}

func TestReplayIngestSessionFloodDoesNotExhaustProjectQuota(t *testing.T) {
	f := newAdmissionOrderFixture(t)
	gw := f.gateway(false)
	gw.replayGuard.project = newProjectIngestWindow(2, gw.now)
	gw.replaySessionLimit = newLocalIngestWindow(1, gw.now)
	app := gw.App()

	resp := postReplayFrom(t, app, f.token, admissionOrderFloodIP, "flood-session", 0, f.now)
	expectIngestStatus(t, resp, http.StatusAccepted, "flooding session first segment")
	for seq := 1; seq <= admissionOrderFloods; seq++ {
		resp = postReplayFrom(t, app, f.token, admissionOrderFloodIP, "flood-session", seq, f.now)
		expectIngestRejected(t, resp, "replay session rate limit exceeded", "flooding session over quota")
	}
	// Malformed metadata is rejected before the project quota is charged too.
	resp = postReplayFrom(t, app, f.token, admissionOrderFloodIP, "bad session", 0, f.now)
	expectIngestStatus(t, resp, http.StatusBadRequest, "invalid session id")
	resp = postReplayFrom(t, app, f.token, admissionOrderFloodIP, "too-far", maxReplaySegmentsPerSession, f.now)
	expectIngestStatus(t, resp, http.StatusRequestEntityTooLarge, "seq beyond session quota")

	resp = postReplayFrom(t, app, f.token, admissionOrderOtherIP, "other-session", 0, f.now)
	expectIngestStatus(t, resp, http.StatusAccepted, "other session after flood")
	resp = postReplayFrom(t, app, f.token, admissionOrderOtherIP, "third-session", 0, f.now)
	expectIngestRejected(t, resp, "replay ingest rate limit exceeded", "project quota still enforced")
}

func TestProfileIngestIPFloodDoesNotExhaustProjectQuota(t *testing.T) {
	f := newAdmissionOrderFixture(t)
	gw := f.gateway(true)
	gw.profileGuard.project = newProjectIngestWindow(2, gw.now)
	gw.profileGuard.ip = newLocalIngestWindow(1, gw.now)
	app := gw.App()
	data := writeProfile(t, syntheticProfile(11, "main.work", "main.main"))

	resp := postProfileFrom(t, app, f.token, admissionOrderFloodIP, data)
	expectIngestStatus(t, resp, http.StatusCreated, "flooding IP first profile")
	for i := 0; i < admissionOrderFloods; i++ {
		resp = postProfileFrom(t, app, f.token, admissionOrderFloodIP, data)
		expectIngestRejected(t, resp, "profile ingest rate limit exceeded", "flooding IP over quota")
	}
	resp = postProfileFrom(t, app, f.token, admissionOrderOtherIP, data)
	expectIngestStatus(t, resp, http.StatusCreated, "other IP after flood")
}
