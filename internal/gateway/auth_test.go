package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/eimsound/lazy-observe/internal/symbolicate"
	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"
)

type failingMarkLoginMetadata struct{ *meta.Store }

func (f failingMarkLoginMetadata) MarkLogin(context.Context, int) error {
	return errors.New("mark login unavailable")
}

func testStore(t *testing.T) *meta.Store {
	t.Helper()
	store, err := meta.OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func noLoginFailureDelay() time.Duration { return 0 }

func testAccount(store *meta.Store, options account.Options) *account.Manager {
	return account.New(store, account.NewMetadataSessions(store), options)
}

// newTestGateway builds a gateway around fakes. Unless a test supplies its own
// Account, any "lou_" bearer authenticates as a viewer (queryTestAccount), and
// a fakeTokens-backed gateway resolves every X-LO-Project slug to the fake
// token's project. Tests therefore use "lou_valid": ingest routes accept it via
// fakeTokens and query routes via the user-token path, as in production where
// project tokens are ingest-only.
func newTestGateway(cfg Config) *Gateway {
	wireMetadataCapabilities(&cfg)
	if cfg.Account == nil {
		cfg.Account = queryTestAccount{}
	}
	if tokens, ok := cfg.Tokens.(*fakeTokens); ok && cfg.Store == nil {
		cfg.Store = fakeProjectStore{tokens: tokens}
	}
	return New(cfg)
}

type queryTestAccount struct{ unavailableTestAccount }

func (queryTestAccount) AuthenticateUserToken(_ context.Context, token string) (account.User, error) {
	if strings.HasPrefix(token, "lou_") {
		return account.User{ID: 1, Username: "query-test", Role: "viewer"}, nil
	}
	return account.User{}, &account.Error{Code: account.CodeInvalidCredentials}
}

type fakeProjectStore struct{ tokens *fakeTokens }

func (s fakeProjectStore) ProjectBySlug(context.Context, string) (meta.ProjectInfo, error) {
	s.tokens.mu.Lock()
	defer s.tokens.mu.Unlock()
	return s.tokens.project, s.tokens.err
}

func (fakeProjectStore) GetProject(context.Context, int) (*meta.Project, error) {
	return nil, &meta.NotFoundError{}
}

func (fakeProjectStore) RecordAudit(context.Context, meta.AuditRecord) error { return nil }

func (fakeProjectStore) ListAudit(context.Context, meta.AuditListOptions) ([]meta.AuditRecord, error) {
	return nil, nil
}

// userQueryToken issues a real user API token for metadata-backed tests.
func userQueryToken(t *testing.T, store *meta.Store, username string) string {
	t.Helper()
	u, err := store.CreateUser(t.Context(), username, "password", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateUserAPIToken(t.Context(), u.ID, "test-query")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// wireMetadataCapabilities is the tests' composition root: like production
// startup, it injects the metadata Store into every capability explicitly
// instead of the gateway inferring them from the management Store.
func wireMetadataCapabilities(cfg *Config) {
	store, ok := cfg.Store.(*meta.Store)
	if !ok {
		return
	}
	if cfg.Issues == nil {
		cfg.Issues = store
	}
	if cfg.SourceMaps == nil {
		cfg.SourceMaps = store
	}
	if cfg.Profiles == nil {
		cfg.Profiles = store
	}
	if cfg.Notifications == nil {
		cfg.Notifications = store
	}
	if cfg.UploadTokens == nil {
		cfg.UploadTokens = store
	}
}

func newMetadataTestGateway(store *meta.Store, cfg Config) *Gateway {
	wireMetadataCapabilities(&cfg)
	if cfg.Account == nil {
		cfg.Account = account.New(store, account.NewMetadataSessions(store), account.Options{
			Now: cfg.Now, LoginFailureDelay: noLoginFailureDelay,
		})
	}
	gw := New(cfg)
	if cfg.ErrorTracker == nil {
		inbox, err := errortrack.NewInbox(
			store,
			errortrack.NewVictoriaLogsWriter(gw.storage, gw.client),
			errortrack.InboxOptions{
				Now: gw.now, Symbolicator: symbolicate.New(cfg.Blobs, 128), OnChange: gw.HandleIssueChange,
				OnEvent: gw.HandleErrorEvent,
			},
		)
		if err != nil {
			panic("test gateway: initialize error tracking: " + err.Error())
		}
		if err := gw.AttachErrorTracker(inbox); err != nil {
			panic(err)
		}
	}
	return gw
}

type unavailableTestAccount struct{}

func (unavailableTestAccount) Login(context.Context, account.LoginInput) (account.LoginResult, error) {
	return account.LoginResult{}, unavailableTestAccountError()
}

func (unavailableTestAccount) AuthenticateSession(context.Context, string) (account.User, error) {
	return account.User{}, unavailableTestAccountError()
}

func (unavailableTestAccount) AuthenticateUserToken(context.Context, string) (account.User, error) {
	return account.User{}, unavailableTestAccountError()
}

func (unavailableTestAccount) Logout(context.Context, string, account.User) (account.AuditFacts, error) {
	return account.AuditFacts{}, unavailableTestAccountError()
}

func (unavailableTestAccount) CurrentUser(context.Context, int) (account.User, error) {
	return account.User{}, unavailableTestAccountError()
}

func (unavailableTestAccount) ChangePassword(
	context.Context,
	account.User,
	string,
	string,
	string,
) (account.AuditFacts, error) {
	return account.AuditFacts{}, unavailableTestAccountError()
}

func (unavailableTestAccount) UpdateProfile(
	context.Context,
	account.User,
	account.ProfileInput,
) (account.User, account.AuditFacts, error) {
	return account.User{}, account.AuditFacts{}, unavailableTestAccountError()
}

func (unavailableTestAccount) ListSessions(context.Context, int, string) ([]account.SessionRef, error) {
	return nil, unavailableTestAccountError()
}

func (unavailableTestAccount) RevokeSession(
	context.Context,
	account.User,
	string,
) (account.AuditFacts, error) {
	return account.AuditFacts{}, unavailableTestAccountError()
}

func (unavailableTestAccount) ListUserTokens(context.Context, int) ([]account.Token, error) {
	return nil, unavailableTestAccountError()
}

func (unavailableTestAccount) IssueUserToken(
	context.Context,
	account.User,
	string,
) (account.IssuedToken, error) {
	return account.IssuedToken{}, unavailableTestAccountError()
}

func (unavailableTestAccount) RevokeUserToken(
	context.Context,
	account.User,
	int,
) (account.AuditFacts, error) {
	return account.AuditFacts{}, unavailableTestAccountError()
}

func unavailableTestAccountError() error {
	return &account.Error{Code: account.CodeUnavailable, Stage: "test_account"}
}

func sessionCookie(t *testing.T, store *meta.Store, username, password, role string) *http.Cookie {
	t.Helper()
	user, err := store.CreateUser(context.Background(), username, password, role)
	if err != nil {
		t.Fatal(err)
	}
	token := metadataSession(t, store, user.ID, time.Now(), time.Now().Add(time.Hour))
	return &http.Cookie{Name: "lo_session", Value: token}
}

func metadataSession(
	t *testing.T,
	store *meta.Store,
	userID int,
	createdAt, expiresAt time.Time,
) string {
	t.Helper()
	token, err := account.NewMetadataSessions(store).Create(t.Context(), userID, createdAt, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRBACViewerForbiddenAdminAllowed(t *testing.T) {
	store := testStore(t)
	viewer := sessionCookie(t, store, "view", "password", "viewer")
	admin := sessionCookie(t, store, "root", "password", "admin")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()
	body := []byte(`{"name":"Project Two","slug":"p2"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(viewer)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create: status=%d err=%v", resp.StatusCode, err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("admin create: status=%d err=%v", resp.StatusCode, err)
	}
}

func TestAlertWriteRBAC(t *testing.T) {
	store := testStore(t)
	viewer := sessionCookie(t, store, "alert-viewer", "password", "viewer")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store}).App()
	body := bytes.NewReader([]byte(`{"name":"Always","expr":"vector(1)"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/alert-rules", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous rule write: status=%d err=%v", resp.StatusCode, err)
	}
	req = httptest.NewRequest(
		http.MethodPost,
		"/api/alert-rules",
		bytes.NewReader([]byte(`{"name":"Always","expr":"vector(1)"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(viewer)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer rule write: status=%d err=%v", resp.StatusCode, err)
	}
}

func TestQueryAuthenticationBothChannels(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(context.Background(), "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "reader", "password", "viewer")
	projects := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		projects <- r.Header.Get("ProjectID")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL}).App()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+userQueryToken(t, store, "token-reader"))
	req.Header.Set("X-LO-Project", "alpha")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent || <-projects != strconvI(project.ID) {
		t.Fatalf("user token channel: status=%d err=%v", resp.StatusCode, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project token channel: status=%d err=%v", resp.StatusCode, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("X-LO-Project", "alpha")
	req.AddCookie(cookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent || <-projects != strconvI(project.ID) {
		t.Fatalf("session channel: status=%d err=%v", resp.StatusCode, err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	store := testStore(t)
	app := newTestGateway(Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()
	for i := 1; i <= 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusUnauthorized
		if i == 11 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("attempt %d: status=%d want=%d body=%s", i, resp.StatusCode, want, body)
		}
	}
}

func TestLoginAccountRateLimitAcrossSourceIPs(t *testing.T) {
	store := testStore(t)
	app := newTestGateway(Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()
	payload, _ := json.Marshal(map[string]string{"username": "target", "password": "wrong"})
	for i := 1; i <= 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", i)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusUnauthorized
		if i > 5 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("attempt %d: status=%d want=%d body=%s", i, resp.StatusCode, want, body)
		}
		if want == http.StatusTooManyRequests && resp.Header.Get(fiber.HeaderRetryAfter) == "" {
			t.Fatalf("attempt %d: lockout response is missing Retry-After", i)
		}
	}
}

func TestLoginFailureResponseAndDelayDoNotRevealUsername(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateUser(context.Background(), "known", "secret", "viewer"); err != nil {
		t.Fatal(err)
	}
	const delay = 20 * time.Millisecond
	app := newTestGateway(Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: func() time.Duration { return delay }}),
	}).App()

	type response struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	request := func(username string) response {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"username": username, "password": "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		started := time.Now()
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed < delay {
			t.Fatalf("%s response delay = %s, want at least %s", username, elapsed, delay)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want %d", username, resp.StatusCode, http.StatusUnauthorized)
		}
		var body response
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	known := request("known")
	missing := request("missing")
	if known != missing || known.Code != "invalid_credentials" || known.Error != "invalid username or password" {
		t.Fatalf("credential responses differ: known=%+v missing=%+v", known, missing)
	}
}

func TestLoginReportsAuthenticationServiceFailure(t *testing.T) {
	app := newTestGateway(Config{}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		bytes.NewReader([]byte(`{"username":"admin","password":"secret"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusServiceUnavailable, body)
	}
}

func TestSharedSessionLoginFailureDoesNotLeaveSession(t *testing.T) {
	store := testStore(t)
	user, err := store.CreateUser(t.Context(), "partial", "secret", "member")
	if err != nil {
		t.Fatal(err)
	}
	sessions := account.NewStateSessions(state.NewMemorySessionStore())
	accounts := account.New(
		failingMarkLoginMetadata{Store: store},
		sessions,
		account.Options{LoginFailureDelay: noLoginFailureDelay},
	)
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store, Account: accounts}).App()
	payload, _ := json.Marshal(map[string]string{"username": "partial", "password": "secret"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	listed, err := accounts.ListSessions(t.Context(), user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("failed login left sessions: %#v", listed)
	}
	events := auditList(t, store, "auth.login")
	if len(events) != 1 || events[0].Success || events[0].FailureReason != "service_unavailable" {
		t.Fatalf("login failure audit=%#v", events)
	}
}

func TestSharedSessionUsesCurrentUserRole(t *testing.T) {
	store := testStore(t)
	user, err := store.CreateUser(t.Context(), "changing-role", "secret", "member")
	if err != nil {
		t.Fatal(err)
	}
	shared := state.NewMemorySessionStore()
	accounts := account.New(
		store,
		account.NewStateSessions(shared),
		account.Options{LoginFailureDelay: noLoginFailureDelay},
	)
	app := newTestGateway(Config{
		Store: store, Tokens: store, Account: accounts, Management: management.New(store),
	}).App()

	payload, _ := json.Marshal(map[string]string{"username": "changing-role", "password": "secret"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d err=%v", resp.StatusCode, err)
	}
	cookie := resp.Cookies()[0]

	if _, err := store.UpdateUser(t.Context(), user.ID, "", "", "viewer"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(
		http.MethodPost,
		"/api/projects",
		bytes.NewReader([]byte(`{"name":"Denied","slug":"denied"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stale member role status=%d err=%v", resp.StatusCode, err)
	}

	if err := store.DeleteUser(t.Context(), user.ID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("deleted user session status=%d err=%v", resp.StatusCode, err)
	}
}

func TestLoginCookieAndMe(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateUser(context.Background(), "admin", "secret", "admin"); err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store}).App()
	payload, _ := json.Marshal(map[string]string{"username": "admin", "password": "secret"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 {
		t.Fatalf("login: status=%d cookies=%v err=%v", resp.StatusCode, resp.Cookies(), err)
	}
	cookie := resp.Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie flags: %#v", cookie)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("me: status=%d err=%v", resp.StatusCode, err)
	}
}

func TestRedisSessionAndLoginLimitSharedAcrossGatewayReplicas(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateUser(context.Background(), "shared-admin", "secret", "admin"); err != nil {
		t.Fatal(err)
	}
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	sharedA := state.NewRedis(clientA, "gateway-test")
	sharedB := state.NewRedis(clientB, "gateway-test")
	appA := newTestGateway(Config{
		Store: store, Tokens: store, Limits: sharedA,
		Account: account.New(store, account.NewStateSessions(sharedA), account.Options{
			Limits: sharedA, LoginGate: sharedA, LoginFailureDelay: noLoginFailureDelay,
		}),
	}).App()
	appB := newTestGateway(Config{
		Store: store, Tokens: store, Limits: sharedB,
		Account: account.New(store, account.NewStateSessions(sharedB), account.Options{
			Limits: sharedB, LoginGate: sharedB, LoginFailureDelay: noLoginFailureDelay,
		}),
	}).App()

	payload, _ := json.Marshal(map[string]string{"username": "shared-admin", "password": "secret"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := appA.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 {
		t.Fatalf("replica A login: status=%d cookies=%v err=%v", resp.StatusCode, resp.Cookies(), err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(resp.Cookies()[0])
	resp, err = appB.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("replica B session: status=%d err=%v", resp.StatusCode, err)
	}

	for i := 1; i <= 11; i++ {
		app := appA
		if i%2 == 0 {
			app = appB
		}
		req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, err = app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusUnauthorized
		if i == 10 || i == 11 { // The successful login above consumed one shared slot.
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("shared attempt %d: status=%d want=%d", i, resp.StatusCode, want)
		}
	}
}

func strconvI(v int) string { return fmt.Sprint(v) }
