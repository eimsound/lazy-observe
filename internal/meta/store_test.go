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

func TestSQLiteDSNEnforcesConnectionPragmasAndUTCTime(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Force database/sql to discard the initial connection and open a fresh one
	// so DSN-level PRAGMAs are exercised across reconnects.
	store.db.SetMaxIdleConns(0)
	var fk, busyTimeout int
	if err := store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if fk != 1 || busyTimeout != 5000 {
		t.Fatalf("sqlite connection pragmas after reconnect: foreign_keys=%d busy_timeout=%d", fk, busyTimeout)
	}

	// Verify Ent default timestamps and non-UTC inputs compare consistently in UTC.
	u, err := store.CreateUser(ctx, "tz-user", "password123", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if u.CreatedAt.Location() != time.UTC {
		t.Fatalf("user created_at location=%v want UTC", u.CreatedAt.Location())
	}
	cst := time.FixedZone("CST", 8*3600)
	expiredAt := time.Date(2026, 7, 17, 10, 0, 0, 0, cst) // 02:00 UTC
	activeAt := time.Date(2026, 7, 17, 14, 0, 0, 0, cst)  // 06:00 UTC
	cutoffUTC := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	if err := store.CreateAccountSession(ctx, AccountSession{
		Token: "tok-expired", UserID: u.ID, CreatedAt: expiredAt.Add(-time.Hour), ExpiresAt: expiredAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccountSession(ctx, AccountSession{
		Token: "tok-active", UserID: u.ID, CreatedAt: activeAt.Add(-time.Hour), ExpiresAt: activeAt,
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteExpiredSessions(ctx, cutoffUTC)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpiredSessions deleted=%d err=%v want 1", deleted, err)
	}
	remaining, err := store.ListAccountSessions(ctx, u.ID, "", cutoffUTC)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("remaining sessions=%d err=%v want 1", len(remaining), err)
	}
}

func TestDeletedProjectSlugCannotBeReusedAndTracksBlobCleanup(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		p, err := store.CreateProject(ctx, "Disposable", "reused-slug")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteProject(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		pending, err := store.ListPendingDeletedProjectBlobs(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 || pending[0] != "reused-slug" {
			t.Fatalf("pending deleted project blobs=%v want [reused-slug]", pending)
		}
		if _, err := store.CreateProject(ctx, "Reincarnated", "reused-slug"); err != ErrProjectSlugReserved {
			t.Fatalf("CreateProject with deleted slug err=%v want ErrProjectSlugReserved", err)
		}
		if err := store.MarkDeletedProjectBlobsCleaned(ctx, "reused-slug", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		pending, err = store.ListPendingDeletedProjectBlobs(ctx, 10)
		if err != nil || len(pending) != 0 {
			t.Fatalf("pending after mark cleaned=%v err=%v", pending, err)
		}
	})
}

func TestBatchedRetentionDeletesAcrossBatchesAndNewTables(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		p, err := store.CreateProject(ctx, "Retain", "retain-batch")
		if err != nil {
			t.Fatal(err)
		}
		u, err := store.CreateUser(ctx, "retain-user", "password123", "viewer")
		if err != nil {
			t.Fatal(err)
		}
		ch, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "webhook", Name: "retain-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
		old := now.Add(-48 * time.Hour)
		fresh := now.Add(-1 * time.Hour)
		cutoff := now.Add(-24 * time.Hour)

		// Seed > retentionDeleteBatchSize expired audit rows to exercise multi-batch deletion.
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		insertAudit := bind(
			store.dialect,
			`INSERT INTO audit_events(action, actor_username, created_at) VALUES ('test', 'bot', ?)`,
		)
		for i := range retentionDeleteBatchSize + 37 {
			ts := old
			if i == retentionDeleteBatchSize+36 {
				ts = fresh
			}
			if _, err := tx.ExecContext(ctx, insertAudit, ts); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		deletedAudit, err := store.DeleteAuditBefore(ctx, cutoff)
		if err != nil || deletedAudit != retentionDeleteBatchSize+36 {
			t.Fatalf("DeleteAuditBefore deleted=%d err=%v want %d", deletedAudit, err, retentionDeleteBatchSize+36)
		}

		// Seed expired + active sessions.
		if err := store.CreateAccountSession(ctx, AccountSession{
			Token: "sess-old", UserID: u.ID, CreatedAt: old, ExpiresAt: old,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateAccountSession(ctx, AccountSession{
			Token: "sess-fresh", UserID: u.ID, CreatedAt: fresh, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := store.DeleteExpiredSessions(ctx, cutoff); err != nil || n != 1 {
			t.Fatalf("DeleteExpiredSessions n=%d err=%v want 1", n, err)
		}

		// Seed expired + fresh notification_deliveries.
		for _, ts := range []time.Time{old, fresh} {
			if _, err := store.db.ExecContext(
				ctx,
				bind(
					store.dialect,
					`INSERT INTO notification_deliveries(channel_id, status, error, attempts, created_at) VALUES (?, 'sent', '', 1, ?)`,
				),
				ch.ID,
				ts,
			); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := store.DeleteExpiredNotificationDeliveries(ctx, cutoff); err != nil || n != 1 {
			t.Fatalf("DeleteExpiredNotificationDeliveries n=%d err=%v want 1", n, err)
		}

		// Seed notification_outbox: old delivered & dead should be deleted; old pending & fresh delivered kept.
		for _, item := range []struct {
			key, status string
			ts          time.Time
		}{
			{"old-delivered", "delivered", old},
			{"old-dead", "dead", old},
			{"old-pending", "pending", old},
			{"fresh-delivered", "delivered", fresh},
		} {
			if _, err := store.db.ExecContext(
				ctx,
				bind(
					store.dialect,
					`INSERT INTO notification_outbox(idempotency_key, project_id, channel_id, payload, status, available_at, created_at, updated_at)
					 VALUES (?, ?, ?, '{}', ?, ?, ?, ?)`,
				),
				item.key,
				p.ID,
				ch.ID,
				item.status,
				item.ts,
				item.ts,
				item.ts,
			); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := store.DeleteExpiredNotificationOutbox(ctx, cutoff); err != nil || n != 2 {
			t.Fatalf("DeleteExpiredNotificationOutbox n=%d err=%v want 2", n, err)
		}
		var outboxRemaining int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_outbox`).
			Scan(&outboxRemaining); err != nil ||
			outboxRemaining != 2 {
			t.Fatalf("remaining outbox=%d err=%v want 2", outboxRemaining, err)
		}
	})
}
