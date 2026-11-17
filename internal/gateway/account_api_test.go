package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v2"
)

type revokeOtherSessionsFailure struct{ account.Sessions }

func (revokeOtherSessionsFailure) RevokeOthers(context.Context, int, string) error {
	return errors.New("session backend unavailable")
}

func mustUserID(t *testing.T, store *meta.Store, username string) int {
	t.Helper()
	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.Username == username {
			return user.ID
		}
	}
	t.Fatalf("user %q not found", username)
	return 0
}

func auditList(t *testing.T, store *meta.Store, action string) []meta.AuditRecord {
	t.Helper()
	events, err := store.ListAudit(context.Background(), meta.AuditListOptions{Action: action, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestChangePasswordEndpoint(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "changer", "password", "member")
	app := newTestGateway(Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()

	post := func(body string, cookie *http.Cookie, csrf string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/password", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		if csrf != "" {
			req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
			req.Header.Set(csrfHeaderName, csrf)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := post(
		`{"current_password":"wrong","new_password":"new-password"}`,
		cookie,
		"",
	); resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("wrong current password: status=%d body=%s", resp.StatusCode, body)
	}
	if resp := post(
		`{"current_password":"password","new_password":"short"}`,
		cookie,
		"",
	); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short new password: status=%d", resp.StatusCode)
	}
	if resp := post(
		`{"current_password":"password","new_password":"brand-new-password"}`,
		cookie,
		"",
	); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: status=%d", resp.StatusCode)
	}
	if _, err := store.AuthenticateCredentials(t.Context(), "changer", "brand-new-password"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	if _, err := store.AuthenticateCredentials(t.Context(), "changer", "password"); err == nil {
		t.Fatal("old password still accepted")
	}
	// Audit trail persisted both failure and success.
	events := auditList(t, store, "auth.password_change")
	if len(events) < 3 {
		t.Fatalf("password_change audit events = %d, want >= 3", len(events))
	}
	foundSuccess := false
	for _, event := range events {
		if event.Success {
			foundSuccess = true
		}
	}
	if !foundSuccess {
		t.Fatal("no successful password_change audit event")
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "sweeper", "password", "member")
	userID := mustUserID(t, store, "sweeper")
	metadataSession(t, store, userID, time.Now(), time.Now().Add(time.Hour))
	app := newTestGateway(Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}),
	}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/password",
		bytes.NewReader([]byte(`{"current_password":"password","new_password":"new-password"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: status=%d", resp.StatusCode)
	}
	sessions, err := account.NewMetadataSessions(store).List(t.Context(), userID, cookie.Value, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions after change = %d, want 1", len(sessions))
	}
	if !sessions[0].Current {
		t.Fatal("remaining session should be the current one")
	}
}

func TestChangePasswordReportsSessionRevocationFailure(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "revoke-failure", "password", "member")
	sessions := revokeOtherSessionsFailure{Sessions: account.NewMetadataSessions(store)}
	accounts := account.New(store, sessions, account.Options{LoginFailureDelay: noLoginFailureDelay})
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store, Account: accounts}).App()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/password",
		bytes.NewReader([]byte(`{"current_password":"password","new_password":"new-password"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	events := auditList(t, store, "auth.password_change")
	if len(events) != 1 || events[0].Success || events[0].FailureReason != "session_revoke_failed" {
		t.Fatalf("audit events=%#v", events)
	}
}

func TestUpdateMeProfile(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "profile", "password", "member")
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store}).App()

	put := func(body string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/auth/me", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := put(`{"display_name":"Ada","email":"ada@example.com"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("update profile: status=%d body=%s", resp.StatusCode, body)
	}
	var updated struct {
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Ada" || updated.Email != "ada@example.com" {
		t.Fatalf("profile response wrong: %#v", updated)
	}
	if resp := put(`{"email":"not-an-email"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid email: status=%d", resp.StatusCode)
	}
	if resp := put(`{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty update: status=%d", resp.StatusCode)
	}
	resp = put(`{"display_name":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear display name: status=%d", resp.StatusCode)
	}
	me := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	me.AddCookie(cookie)
	resp, err := app.Test(me)
	if err != nil {
		t.Fatal(err)
	}
	var meBody struct {
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meBody); err != nil {
		t.Fatal(err)
	}
	if meBody.DisplayName != "" || meBody.Email != "ada@example.com" {
		t.Fatalf("me after clear: %#v", meBody)
	}
}

func TestSessionManagementEndpoints(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "sessions", "password", "member")
	userID := mustUserID(t, store, "sessions")
	metadataSession(t, store, userID, time.Now(), time.Now().Add(time.Hour))
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store}).App()

	list := func() []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	} {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/sessions", nil)
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("list sessions: status=%d body=%s", resp.StatusCode, body)
		}
		var sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
			t.Fatal(err)
		}
		return sessions
	}
	sessions := list()
	if len(sessions) != 2 {
		t.Fatalf("session count = %d, want 2", len(sessions))
	}
	var otherID string
	currentCount := 0
	for _, session := range sessions {
		if session.Current {
			currentCount++
		} else {
			otherID = session.ID
		}
	}
	if currentCount != 1 || otherID == "" {
		t.Fatalf("current flags wrong: %#v", sessions)
	}
	del := httptest.NewRequest(http.MethodDelete, "/api/auth/sessions/"+otherID, nil)
	del.AddCookie(cookie)
	resp, err := app.Test(del)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete session: status=%d err=%v", resp.StatusCode, err)
	}
	if len(list()) != 1 {
		t.Fatal("session not revoked")
	}
	// Cross-user deletion of an existing session must 404.
	otherCookie := sessionCookie(t, store, "intruder", "password", "member")
	req := httptest.NewRequest(http.MethodDelete, "/api/auth/sessions/"+otherID, nil)
	req.AddCookie(otherCookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user delete: status=%d err=%v", resp.StatusCode, err)
	}
	_ = userID
}

func TestAuditEndpointAdminOnlyWithFilters(t *testing.T) {
	store := testStore(t)
	adminCookie := sessionCookie(t, store, "auditor", "password", "admin")
	memberCookie := sessionCookie(t, store, "pleb", "password", "member")
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store}).App()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		bytes.NewReader([]byte(`{"username":"auditor","password":"wrong"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	_, _ = app.Test(req)

	get := func(cookie *http.Cookie, query string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/audit"+query, nil)
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := get(memberCookie, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member audit: status=%d", resp.StatusCode)
	}
	resp := get(adminCookie, "?action=auth.login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin audit: status=%d", resp.StatusCode)
	}
	var events []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events) < 1 {
		t.Fatalf("audit events = %d, want >= 1", len(events))
	}
	for _, event := range events {
		if event["action"] != "auth.login" {
			t.Fatalf("action filter leaked: %#v", event)
		}
	}
}

func TestCSRFEnforcementAndCookies(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateUser(t.Context(), "csrf", "password", "admin"); err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Store: store, Tokens: store, Management: management.New(store),
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}), RequireCSRF: true,
	}).App()

	loginReq := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		bytes.NewReader([]byte(`{"username":"csrf","password":"password"}`)),
	)
	loginReq.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(loginReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login: status=%d err=%v", resp.StatusCode, err)
	}
	var sessionCookie *http.Cookie
	var csrfValue string
	for _, cookie := range resp.Cookies() {
		switch cookie.Name {
		case "lo_session":
			sessionCookie = cookie
		case csrfCookieName:
			csrfValue = cookie.Value
		}
	}
	if sessionCookie == nil || csrfValue == "" {
		t.Fatalf("login cookies missing: session=%v csrf=%q", sessionCookie, csrfValue)
	}
	mutate := func(withCSRF bool) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader([]byte(`{"name":"X","slug":"x"}`)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookie)
		if withCSRF {
			req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfValue})
			req.Header.Set(csrfHeaderName, csrfValue)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := mutate(false); resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mutation without CSRF: status=%d body=%s", resp.StatusCode, body)
	}
	if resp := mutate(true); resp.StatusCode != http.StatusOK {
		t.Fatalf("mutation with CSRF: status=%d", resp.StatusCode)
	}
	// GET requests are exempt.
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(sessionCookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with session: status=%d err=%v", resp.StatusCode, err)
	}
}

func TestSecureSessionCookieFlag(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateUser(t.Context(), "secure", "password", "member"); err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Store: store, Tokens: store,
		Account: testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay}), SecureSessionCookie: true,
	}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		bytes.NewReader([]byte(`{"username":"secure","password":"password"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login: status=%d err=%v", resp.StatusCode, err)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "lo_session" && !cookie.Secure {
			t.Fatal("session cookie is not marked Secure")
		}
	}
}

func TestLoginLockoutLiftsAfterWindow(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateUser(t.Context(), "locker", "password", "member"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	app := newTestGateway(
		Config{
			Store: store, Tokens: store, Now: func() time.Time { return now },
			Account: testAccount(store, account.Options{
				Now: func() time.Time { return now }, LoginFailureDelay: noLoginFailureDelay,
			}),
		},
	).App()
	attempt := func(password string) *http.Response {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"username": "locker", "password": password})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for i := 0; i < 5; i++ {
		if resp := attempt("wrong"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: status=%d", i+1, resp.StatusCode)
		}
	}
	// Locked: even the correct password is rejected with 429 + Retry-After.
	resp := attempt("password")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked login: status=%d", resp.StatusCode)
	}
	if resp.Header.Get(fiber.HeaderRetryAfter) == "" {
		t.Fatal("locked login missing Retry-After")
	}
	// Advancing past the lockout window lifts the lock.
	now = now.Add(15*time.Minute + time.Second)
	if resp := attempt("password"); resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login after lockout: status=%d body=%s", resp.StatusCode, body)
	}
}
