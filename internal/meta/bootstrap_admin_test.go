package meta

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingHandler keeps every structured log record so tests can assert on
// keys and values instead of grepping formatted text.
type recordingHandler struct {
	mu      *sync.Mutex
	records *[]recordedLog
	attrs   []slog.Attr
}

type recordedLog struct {
	message string
	attrs   map[string]string
}

func newRecordingLogger() (*slog.Logger, func() []recordedLog) {
	handler := &recordingHandler{mu: &sync.Mutex{}, records: &[]recordedLog{}}
	return slog.New(handler), func() []recordedLog {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return append([]recordedLog(nil), (*handler.records)...)
	}
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	entry := recordedLog{message: record.Message, attrs: map[string]string{}}
	for _, attr := range h.attrs {
		entry.attrs[attr.Key] = attr.Value.String()
	}
	record.Attrs(func(attr slog.Attr) bool {
		entry.attrs[attr.Key] = attr.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, entry)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func openBootstrapTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertLogsOmit(t *testing.T, records []recordedLog, secret string) {
	t.Helper()
	if secret == "" {
		t.Fatal("secret under test is empty")
	}
	for _, record := range records {
		if strings.Contains(record.message, secret) {
			t.Fatalf("log message %q contains the admin password", record.message)
		}
		for key, value := range record.attrs {
			if key == "password" {
				t.Fatalf("log %q carries a password attribute", record.message)
			}
			if strings.Contains(value, secret) {
				t.Fatalf("log %q attribute %q contains the admin password", record.message, key)
			}
		}
	}
}

func findLog(records []recordedLog, message string) (recordedLog, bool) {
	for _, record := range records {
		if record.message == message {
			return record, true
		}
	}
	return recordedLog{}, false
}

func TestBootstrapWritesGeneratedAdminPasswordFileWithoutLoggingIt(t *testing.T) {
	ctx := context.Background()
	store := openBootstrapTestStore(t)
	path := filepath.Join(t.TempDir(), "data", InitialAdminPasswordFileName)
	logger, records := newRecordingLogger()
	if _, err := store.Bootstrap(ctx, logger, BootstrapAdmin{GeneratedPasswordFile: path}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("initial admin password file mode = %o, want 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(raw))
	if len(password) < minBootstrapAdminPasswordLength {
		t.Fatalf("generated password length = %d", len(password))
	}
	admin, err := store.AuthenticateCredentials(ctx, "admin", password)
	if err != nil {
		t.Fatalf("generated password from the file does not authenticate admin: %v", err)
	}
	if !admin.MustChangePassword {
		t.Fatal("generated bootstrap admin must be forced to change the password")
	}
	if err := store.ChangePassword(ctx, admin.ID, password, "operator-chosen-password"); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.GetUser(ctx, admin.ID); err != nil || changed.MustChangePassword {
		t.Fatalf("own password change must clear the flag: user=%#v err=%v", changed, err)
	}

	logs := records()
	assertLogsOmit(t, logs, password)
	written, ok := findLog(logs, "initial admin password written")
	if !ok || written.attrs["path"] != path || written.attrs["host"] == "" {
		t.Fatalf("missing initial admin password path log: %#v", logs)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("atomic write left extra files: %v err=%v", entries, err)
	}
}

func TestBootstrapUsesProvidedAdminPasswordFile(t *testing.T) {
	ctx := context.Background()
	store := openBootstrapTestStore(t)
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "admin-password")
	const provided = "deployer chosen password"
	if err := os.WriteFile(passwordFile, []byte("  "+provided+"\r\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(dir, InitialAdminPasswordFileName)
	logger, records := newRecordingLogger()
	if _, err := store.Bootstrap(ctx, logger, BootstrapAdmin{PasswordFile: passwordFile}); err != nil {
		t.Fatal(err)
	}
	admin, err := store.AuthenticateCredentials(ctx, "admin", provided)
	if err != nil {
		t.Fatalf("provided password does not authenticate admin: %v", err)
	}
	if admin.MustChangePassword {
		t.Fatal("a deployer-provided admin password must not be forced to change")
	}
	if _, err := os.Stat(generated); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provided password must not produce a generated file: %v", err)
	}
	logs := records()
	assertLogsOmit(t, logs, provided)
	created, ok := findLog(logs, "created bootstrap admin")
	if !ok || created.attrs["password_source"] != "admin_password_file" {
		t.Fatalf("missing provided-password bootstrap log: %#v", logs)
	}
}

func TestBootstrapRejectsUnusableAdminPasswordFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"empty":            "",
		"whitespace only":  " \n\t\r\n",
		"shorter than min": "short7\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openBootstrapTestStore(t)
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			logger, _ := newRecordingLogger()
			if _, err := store.Bootstrap(ctx, logger, BootstrapAdmin{PasswordFile: path}); err == nil {
				t.Fatal("bootstrap accepted an unusable admin password file")
			}
			users, err := store.ListUsers(ctx)
			if err != nil || len(users) != 0 {
				t.Fatalf("failed bootstrap created users=%v err=%v", users, err)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		store := openBootstrapTestStore(t)
		logger, _ := newRecordingLogger()
		admin := BootstrapAdmin{PasswordFile: filepath.Join(dir, "absent")}
		if _, err := store.Bootstrap(context.Background(), logger, admin); err == nil {
			t.Fatal("bootstrap accepted a missing admin password file")
		}
	})
	t.Run("no source", func(t *testing.T) {
		store := openBootstrapTestStore(t)
		logger, _ := newRecordingLogger()
		if _, err := store.Bootstrap(context.Background(), logger, BootstrapAdmin{}); err == nil {
			t.Fatal("bootstrap accepted no admin password source")
		}
	})
}

func TestBootstrapKeepsExistingAdmin(t *testing.T) {
	ctx := context.Background()
	store := openBootstrapTestStore(t)
	const existing = "existing-admin-password"
	if _, err := store.CreateUser(ctx, "admin", existing, "admin"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "admin-password")
	if err := os.WriteFile(passwordFile, []byte("replacement-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(dir, InitialAdminPasswordFileName)
	for _, admin := range []BootstrapAdmin{{PasswordFile: passwordFile}, {GeneratedPasswordFile: generated}} {
		logger, records := newRecordingLogger()
		if _, err := store.Bootstrap(ctx, logger, admin); err != nil {
			t.Fatal(err)
		}
		if _, ok := findLog(records(), "created bootstrap admin"); ok {
			t.Fatalf("bootstrap recreated an existing admin with %#v", admin)
		}
	}
	if _, err := store.AuthenticateCredentials(ctx, "admin", existing); err != nil {
		t.Fatalf("existing admin password was overwritten: %v", err)
	}
	if _, err := os.Stat(generated); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing admin must not produce a generated file: %v", err)
	}
}
