package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/redis/go-redis/v9"
)

func TestAdminCreateUserRollsBackOnProfileConflict(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "atomic-create-admin", "password", "admin")
	existing, err := store.CreateUser(context.Background(), "email-owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	email := "occupied@example.com"
	if _, err := store.UpdateUserProfile(context.Background(), existing.ID, nil, &email); err != nil {
		t.Fatal(err)
	}

	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/users",
		strings.NewReader(
			`{"username":"must-roll-back","password":"password","role":"member","email":"occupied@example.com"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusConflict, body)
	}

	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.Username == "must-roll-back" {
			t.Fatalf("failed create left user behind: %#v", user)
		}
	}
}

func TestAdminUpdateUserRollsBackOnProfileConflict(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "atomic-update-admin", "password", "admin")
	target, err := store.CreateUser(context.Background(), "before-update", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	originalDisplayName, originalEmail := "Before Update", "before@example.com"
	if _, err := store.UpdateUserProfile(
		context.Background(),
		target.ID,
		&originalDisplayName,
		&originalEmail,
	); err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateUser(context.Background(), "update-email-owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	occupiedEmail := "update-occupied@example.com"
	if _, err := store.UpdateUserProfile(context.Background(), owner.ID, nil, &occupiedEmail); err != nil {
		t.Fatal(err)
	}

	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()
	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/users/"+strconvI(target.ID),
		strings.NewReader(
			`{"username":"after-update","role":"viewer","display_name":"After Update","email":"update-occupied@example.com"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusConflict, body)
	}

	updated, err := store.GetUser(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Username != "before-update" || updated.Role != "member" ||
		updated.DisplayName != originalDisplayName || updated.Email != originalEmail {
		t.Fatalf("failed update changed user: %#v", updated)
	}
}

func TestAdminCreateUserPersistsDisplayName(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "display-name-admin", "password", "admin")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/users",
		strings.NewReader(
			`{"username":"alice-ops","password":"password-123","role":"member","display_name":"Alice Ops","email":"alice@example.com"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user status=%d want=201 body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"display_name":"Alice Ops"`) {
		t.Fatalf("create user response missing display_name: %s", body)
	}

	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range users {
		if u.Username == "alice-ops" {
			found = true
			if u.DisplayName != "Alice Ops" {
				t.Fatalf("persisted display_name=%q want %q", u.DisplayName, "Alice Ops")
			}
		}
	}
	if !found {
		t.Fatal("created user alice-ops not found in store")
	}
}

func TestAdminUserMutationsRevokeRedisSessionsAndProtectLastAdminViaHTTP(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	redisState := state.NewRedis(client, "mgmt-session-revoke")
	sessions := account.NewStateSessions(redisState)
	accounts := account.New(store, sessions, account.Options{
		Limits: redisState, LoginGate: redisState, LoginFailureDelay: noLoginFailureDelay,
	})
	mgr := management.New(store).WithSessions(sessions)

	admin, _, err := mgr.CreateUser(ctx, management.CreateUserInput{
		Username: "sole-http-admin", Password: "password-123", Role: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := mgr.CreateUser(ctx, management.CreateUserInput{
		Username: "target-member", Password: "password-123", Role: "member",
	})
	if err != nil {
		t.Fatal(err)
	}

	gw := newMetadataTestGateway(store, Config{
		Store:      store,
		Tokens:     store,
		Account:    accounts,
		Management: mgr,
		Limits:     redisState,
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	login := func(username, password string) *http.Cookie {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		resp, reqErr := app.Test(req)
		if reqErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("login %s status=%d err=%v", username, resp.StatusCode, reqErr)
		}
		defer resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == "lo_session" {
				return c
			}
		}
		t.Fatalf("login %s did not return session cookie", username)
		return nil
	}

	meStatus := func(cookie *http.Cookie) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		req.AddCookie(cookie)
		resp, reqErr := app.Test(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	adminCookie := login("sole-http-admin", "password-123")

	// 1. Sole admin cannot be demoted or deleted via HTTP, and session stays active.
	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/users/"+strconvI(admin.ID),
		strings.NewReader(`{"role":"member"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demote sole admin status=%d want=403 err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	req = httptest.NewRequest(http.MethodDelete, "/api/users/"+strconvI(admin.ID), nil)
	req.AddCookie(adminCookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete sole admin status=%d want=403 err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if status := meStatus(adminCookie); status != http.StatusOK {
		t.Fatalf("protected sole admin session lost: status=%d", status)
	}

	// 2. Password reset via HTTP revokes target's Redis session.
	targetCookie := login("target-member", "password-123")
	if status := meStatus(targetCookie); status != http.StatusOK {
		t.Fatalf("target initial session status=%d want=200", status)
	}
	req = httptest.NewRequest(
		http.MethodPatch,
		"/api/users/"+strconvI(target.ID),
		strings.NewReader(`{"password":"new-password-456"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("password reset status=%d want=200 err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if status := meStatus(targetCookie); status != http.StatusUnauthorized {
		t.Fatalf("target Redis session survived password reset: status=%d want=401", status)
	}

	// 3. Role downgrade via HTTP revokes target's Redis session.
	targetCookie = login("target-member", "new-password-456")
	req = httptest.NewRequest(
		http.MethodPut,
		"/api/users/"+strconvI(target.ID),
		strings.NewReader(`{"role":"viewer"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("role downgrade status=%d want=200 err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if status := meStatus(targetCookie); status != http.StatusUnauthorized {
		t.Fatalf("target Redis session survived role downgrade: status=%d want=401", status)
	}

	// 4. User deletion via HTTP revokes target's Redis session.
	targetCookie = login("target-member", "new-password-456")
	req = httptest.NewRequest(http.MethodDelete, "/api/users/"+strconvI(target.ID), nil)
	req.AddCookie(adminCookie)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete user status=%d want=204 err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if status := meStatus(targetCookie); status != http.StatusUnauthorized {
		t.Fatalf("target Redis session survived user deletion: status=%d want=401", status)
	}
}
