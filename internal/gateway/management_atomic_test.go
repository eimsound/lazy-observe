package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eimsound/lazy-observe/internal/management"
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
