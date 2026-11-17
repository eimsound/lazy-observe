package meta

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrationEntriesIgnoreAppleDoubleFiles(t *testing.T) {
	files := fstest.MapFS{
		"migrations/001_baseline.sql":   {Data: []byte("SELECT 1;")},
		"migrations/._001_baseline.sql": {Data: []byte{0, 5, 22, 7}},
		"migrations/README.md":          {Data: []byte("ignored")},
	}

	entries, err := migrationEntries(files, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "001_baseline.sql" {
		t.Fatalf("unexpected migration entries: %v", entries)
	}
}

func TestArgon2IDPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatalf("valid password did not verify: %q", hash)
	}
	if VerifyPassword(hash, "wrong") || VerifyPassword("malformed", "wrong") {
		t.Fatal("invalid password/hash verified")
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		u, err := store.CreateUser(ctx, "expired", "password", "viewer")
		if err != nil {
			t.Fatal(err)
		}
		token := "los_expired"
		if err := store.CreateAccountSession(ctx, AccountSession{
			Token: token, UserID: u.ID, CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AuthenticateAccountSession(ctx, token, time.Now()); err == nil {
			t.Fatal("expired session authenticated")
		}
		count, err := store.client.Session.Query().Count(ctx)
		if err != nil || count != 0 {
			t.Fatalf("expired session cleanup: count=%d err=%v", count, err)
		}
	})
}

func TestBootstrapIdempotentOnSingleBaseline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.db")
	store, err := OpenStoreWithOptions(ctx, path, OpenOptions{ApplicationVersion: "baseline-test"})
	if err != nil {
		t.Fatalf("open baseline database: %v", err)
	}
	var count, version int
	var name, appVersion string
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(version), MAX(name), MAX(app_version) FROM schema_version`).
		Scan(&count, &version, &name, &appVersion); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != 1 || name != "001_baseline.sql" || appVersion != "baseline-test" {
		t.Fatalf("baseline history count=%d version=%d name=%q app_version=%q", count, version, name, appVersion)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	admin := BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), InitialAdminPasswordFileName)}
	if _, err := store.Bootstrap(ctx, logger, admin); err != nil {
		t.Fatal(err)
	}
	first := logs.String()
	if !strings.Contains(first, "created bootstrap admin") {
		t.Fatalf("first bootstrap did not log admin creation: %s", first)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if _, err := store.Bootstrap(ctx, logger, admin); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "created bootstrap admin") {
		t.Fatalf("second bootstrap repeated admin creation: %s", logs.String())
	}
	users, err := store.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("bootstrap users=%v err=%v", users, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStoreRejectsIncompatibleBaselineMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(
		ctx,
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO schema_version(version, applied_at) VALUES (999, ?)`,
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(ctx, path); err == nil || !strings.Contains(err.Error(), "recreate the database") {
		t.Fatalf("OpenStore error=%v, want incompatible-baseline rejection", err)
	}
}

func TestBootstrapSelfMonitoringIsIdempotentAndSecret(t *testing.T) {
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project1, token1, err := store.BootstrapSelfMonitoring(
		context.Background(),
		"lo_test-self-monitoring-derived-token-value",
	)
	if err != nil {
		t.Fatal(err)
	}
	project2, token2, err := store.BootstrapSelfMonitoring(
		context.Background(),
		"lo_test-self-monitoring-derived-token-value",
	)
	if err != nil {
		t.Fatal(err)
	}
	if project1.Slug != SelfMonitoringProjectSlug || project1.ID != project2.ID || token1 == "" || token1 != token2 {
		t.Fatalf("first=(%#v,%q) second=(%#v,%q)", project1, token1, project2, token2)
	}
	tokens, err := store.ListProjectAPITokens(context.Background(), project1.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("token metadata=%v err=%v", tokens, err)
	}
	if strings.Contains(fmt.Sprint(tokens), token1) {
		t.Fatal("internal token leaked through token metadata")
	}
}
