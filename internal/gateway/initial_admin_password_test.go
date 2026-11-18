package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func warningLogger() (*slog.Logger, func() string) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return logger, logs.String
}

func writeInitialAdminPasswordFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)
	if err := os.WriteFile(path, []byte("generated-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func changeOwnPassword(t *testing.T, cfg Config, store *meta.Store, cookie *http.Cookie) *http.Response {
	t.Helper()
	cfg.Store, cfg.Tokens = store, store
	if cfg.Account == nil {
		cfg.Account = testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay})
	}
	app := newMetadataTestGateway(store, cfg).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/password",
		bytes.NewReader([]byte(`{"current_password":"password","new_password":"brand-new-password"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The generated first-admin password is useless once the admin replaced it, so
// the successful change removes the file from disk.
func TestChangePasswordRemovesGeneratedInitialPasswordFile(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "admin", "password", "admin")
	path := writeInitialAdminPasswordFile(t)

	resp := changeOwnPassword(t, Config{InitialAdminPasswordFile: path}, store, cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("initial admin password file still exists: %v", err)
	}
}

// Every other account keeps the file in place: only the bootstrap admin's own
// password change makes it obsolete.
func TestChangePasswordKeepsInitialPasswordFileForOtherUsers(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "member", "password", "member")
	path := writeInitialAdminPasswordFile(t)

	resp := changeOwnPassword(t, Config{InitialAdminPasswordFile: path}, store, cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("another user's password change removed the file: %v", err)
	}
}

// An empty InitialAdminPasswordFile disables removal: cmd leaves it empty when
// security.admin_password_file names the generated file itself, so that
// deployer-owned file is never removed.
func TestChangePasswordKeepsDeployerProvidedPasswordFile(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "admin", "password", "admin")
	path := writeInitialAdminPasswordFile(t)

	resp := changeOwnPassword(t, Config{}, store, cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("deployer-provided file was removed: %v", err)
	}
}

// A generated file that is already gone is the normal repeat password change:
// the removal succeeds silently instead of warning on every later change.
func TestChangePasswordSilentWhenInitialPasswordFileAlreadyGone(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "admin", "password", "admin")
	missing := filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)
	logger, logs := warningLogger()

	resp := changeOwnPassword(t, Config{InitialAdminPasswordFile: missing, Logger: logger}, store, cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("missing file failed the password change: status=%d", resp.StatusCode)
	}
	if _, err := store.AuthenticateCredentials(context.Background(), "admin", "brand-new-password"); err != nil {
		t.Fatalf("password was not changed: %v", err)
	}
	if strings.Contains(logs(), "could not remove initial admin password file") {
		t.Fatalf("an absent file still warned: %s", logs())
	}
}

// A real removal failure is logged, but never turns into a failed password
// change.
func TestChangePasswordWarnsWhenInitialPasswordFileCannotBeRemoved(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "admin", "password", "admin")
	path := filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)
	if err := os.MkdirAll(filepath.Join(path, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	logger, logs := warningLogger()

	resp := changeOwnPassword(t, Config{InitialAdminPasswordFile: path, Logger: logger}, store, cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unremovable file failed the password change: status=%d", resp.StatusCode)
	}
	if !strings.Contains(logs(), "could not remove initial admin password file") {
		t.Fatalf("missing removal warning: %s", logs())
	}
}
