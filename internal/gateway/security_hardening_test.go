package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/secret"
)

func TestLoginRejectsOversizedBodyAndReducesRateLimitAuditNoise(t *testing.T) {
	store := testStore(t)
	app := newMetadataTestGateway(store, Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()

	oversizedBody := `{"username":"` + strings.Repeat("x", maxLoginBodyBytes+256) + `","password":"p"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(oversizedBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized login body status=%d want=400", resp.StatusCode)
	}
	if events := auditList(t, store, "auth.login"); len(events) != 0 {
		t.Fatalf("oversized login body wrote %d audit rows, want 0", len(events))
	}

	longUserPayload, _ := json.Marshal(map[string]string{
		"username": strings.Repeat("u", account.MaxUsernameLength+32),
		"password": "password-123",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(longUserPayload))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.10:1234"
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized username status=%d want=400", resp.StatusCode)
	}

	// Trigger IP rate limiting repeatedly and verify rate_limited audit rows are throttled.
	for i := range 16 {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.20:1234"
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if i >= 10 && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt %d status=%d want=429", i+1, resp.StatusCode)
		}
	}
	events := auditList(t, store, "auth.login")
	rateLimitedCount := 0
	for _, ev := range events {
		if ev.FailureReason == "rate_limited" {
			rateLimitedCount++
		}
	}
	if rateLimitedCount != 1 {
		t.Fatalf("rate_limited audit rows = %d, want throttled 1", rateLimitedCount)
	}
}

func TestLoginTrustProxyIsolatesClientIPsAndIgnoresUntrustedHeaders(t *testing.T) {
	store := testStore(t)

	// Without TrustedProxies, spoofed X-Forwarded-For is ignored.
	untrustedApp := newMetadataTestGateway(store, Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()
	for i := 1; i <= 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "203.0.113."+strconvI(i))
		resp, err := untrustedApp.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		want := http.StatusUnauthorized
		if i == 11 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("untrusted proxy attempt %d: status=%d want=%d", i, resp.StatusCode, want)
		}
	}

	// With TrustedProxies trusting the test peer (0.0.0.0 in Fiber app.Test),
	// distinct X-Forwarded-For client IPs receive independent rate-limit buckets.
	trustedApp := newMetadataTestGateway(store, Config{
		Store: store, Tokens: store,
		TrustedProxies: []string{"0.0.0.0/32", "127.0.0.0/8"},
		Account:        testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()
	for i := 1; i <= 15; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "203.0.113."+strconvI(i))
		resp, err := trustedApp.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("trusted proxy distinct client %d: status=%d want=401", i, resp.StatusCode)
		}
	}
}

type stubManagementStore struct {
	project meta.ProjectInfo
	err     error
}

func (s stubManagementStore) ProjectBySlug(context.Context, string) (meta.ProjectInfo, error) {
	return s.project, s.err
}
func (stubManagementStore) GetProject(context.Context, int) (*meta.Project, error) {
	return nil, &meta.NotFoundError{}
}
func (stubManagementStore) RecordAudit(context.Context, meta.AuditRecord) error {
	return errors.New("audit db down")
}
func (stubManagementStore) ListAudit(context.Context, meta.AuditListOptions) ([]meta.AuditRecord, error) {
	return nil, nil
}

func TestQueryRequiredAndLoadSessionProjectDistinguish503And404(t *testing.T) {
	// queryRequired with unavailable session backend -> 503 (not 401).
	app := newTestGateway(Config{
		Account: unavailableTestAccount{},
		Store:   stubManagementStore{project: meta.ProjectInfo{ID: 1, Slug: "default"}},
	}).App()
	req := httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	req.AddCookie(&http.Cookie{Name: "lo_session", Value: "lss_active"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("queryRequired with unavailable session status=%d want=503", resp.StatusCode)
	}

	// queryRequired without session cookie -> still 401.
	req = httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("queryRequired without session cookie status=%d want=401", resp.StatusCode)
	}

	// loadSessionProject with g.store == nil -> 503 (no panic).
	nilStoreApp := newTestGateway(Config{
		Account: queryTestAccount{},
		Store:   nil,
	}).App()
	req = httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err = nilStoreApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("loadSessionProject with nil store status=%d want=503", resp.StatusCode)
	}

	// Missing project -> 404 (not 401).
	notFoundApp := newTestGateway(Config{
		Account: queryTestAccount{},
		Store:   stubManagementStore{err: &meta.NotFoundError{Label: "project"}},
	}).App()
	req = httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	req.Header.Set("X-LO-Project", "missing")
	resp, err = notFoundApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("loadSessionProject missing project status=%d want=404", resp.StatusCode)
	}

	// Store transient failure during project lookup -> 503 (not 401).
	dbErrApp := newTestGateway(Config{
		Account: queryTestAccount{},
		Store:   stubManagementStore{err: errors.New("postgres timeout")},
	}).App()
	req = httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err = dbErrApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("loadSessionProject store error status=%d want=503", resp.StatusCode)
	}
}

func TestHealthzReadyzAndProxySanitizeInternalErrors(t *testing.T) {
	const rawSecretErr = "dial tcp 10.96.0.12:5432: connect: connection refused"
	g := newTestGateway(Config{
		Tokens:    &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Health:    staticHealth{"metadata": errors.New(rawSecretErr)},
		Readiness: staticHealth{"redis": errors.New(rawSecretErr)},
		Logs:      "http://127.0.0.1:1",
	})
	g.MarkReady()

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := g.App().Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d want=503", path, resp.StatusCode)
		}
		if strings.Contains(string(body), "10.96.0.12") || strings.Contains(string(body), "5432") ||
			strings.Contains(string(body), "connection refused") {
			t.Fatalf("%s leaked raw internal error: %s", path, body)
		}
		if !strings.Contains(string(body), `"unavailable"`) {
			t.Fatalf("%s missing sanitized status: %s", path, body)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := g.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy unreachable status=%d want=502", resp.StatusCode)
	}
	if strings.Contains(string(body), "127.0.0.1") || strings.Contains(string(body), "connection refused") {
		t.Fatalf("proxy 502 leaked raw internal error: %s", body)
	}
}

func TestAuditWarnOnPersistFailureAndNewAuditedRoutes(t *testing.T) {
	// 1. Audit persistence failure must log at WARN level (visible under default INFO logger).
	var warnOut bytes.Buffer
	warnLogger := slog.New(slog.NewJSONHandler(&warnOut, &slog.HandlerOptions{Level: slog.LevelInfo}))
	failingApp := newTestGateway(Config{
		Store:   stubManagementStore{project: meta.ProjectInfo{ID: 1, Slug: "default"}},
		Account: queryTestAccount{},
		Logger:  warnLogger,
	}).App()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	_, _ = failingApp.Test(req)
	if !strings.Contains(warnOut.String(), `"level":"WARN"`) ||
		!strings.Contains(warnOut.String(), "audit persistence failed") {
		t.Fatalf("expected WARN log for audit persistence failure, got: %s", warnOut.String())
	}

	// 2. PUT /api/ai/settings, /api/channels CRUD, and DELETE /api/releases/:release/sourcemaps emit audit rows.
	store := testStore(t)
	project, err := store.CreateProject(t.Context(), "Default", "default")
	if err != nil {
		t.Fatal(err)
	}
	adminCookie := sessionCookie(t, store, "audit-ops", "password-123", "admin")
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	localBlobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alertSvc := alert.NewService(alert.Config{Store: store})
	t.Cleanup(alertSvc.Close)
	app := newMetadataTestGateway(store, Config{
		Store:           store,
		Tokens:          store,
		Management:      management.New(store),
		Alerts:          alertSvc,
		Blobs:           localBlobs,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
	}).App()

	doAdmin := func(method, target, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.AddCookie(adminCookie)
		resp, reqErr := app.Test(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, target, resp.StatusCode, want, raw)
		}
		return raw
	}

	const aiKeySecret = "sk-super-secret-ai-key"
	doAdmin(
		http.MethodPut,
		"/api/ai/settings",
		`{"enabled":true,"provider":"openai","model":"gpt-4o-mini","api_key":"`+aiKeySecret+`"}`,
		http.StatusOK,
	)

	createdChannel := doAdmin(
		http.MethodPost,
		"/api/channels",
		`{"type":"webhook","name":"ops-hook","config":{"url":"https://hooks.example.com/secret-token"},"enabled":true}`,
		http.StatusCreated,
	)
	var ch struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createdChannel, &ch); err != nil {
		t.Fatal(err)
	}
	doAdmin(
		http.MethodPatch,
		"/api/channels/"+ch.ID,
		`{"name":"ops-hook-renamed"}`,
		http.StatusOK,
	)
	doAdmin(http.MethodDelete, "/api/channels/"+ch.ID, "", http.StatusNoContent)

	if err := store.UpsertSourceMapArtifact(
		t.Context(), project.ID, "v1.2.3", "app.js.map", 10, "deadbeef", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	doAdmin(http.MethodDelete, "/api/releases/v1.2.3/sourcemaps", "", http.StatusNoContent)

	persisted, err := store.ListAudit(t.Context(), meta.AuditListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(persisted)
	if strings.Contains(string(encoded), aiKeySecret) || strings.Contains(string(encoded), "secret-token") {
		t.Fatalf("persisted audit leaked credentials: %s", encoded)
	}
	seen := map[string]bool{}
	for _, ev := range persisted {
		if ev.Success {
			seen[ev.Action] = true
		}
	}
	for _, wantAction := range []string{
		"ai_settings.update",
		"notification_channel.create",
		"notification_channel.update",
		"notification_channel.delete",
		"source_map.delete",
	} {
		if !seen[wantAction] {
			t.Errorf("missing persisted audit event for %s", wantAction)
		}
	}
}
