package meta

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/eimsound/lazy-observe/internal/embeddedpg"
)

var (
	postgresAdminURL string
	postgresDBSeq    atomic.Uint64
)

func TestMain(m *testing.M) {
	if os.Getenv("LO_TEST_PG") != "1" {
		os.Exit(m.Run())
	}
	temp, err := os.MkdirTemp("", "lazy-observe-postgres-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create embedded postgres directory:", err)
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "allocate embedded postgres port:", err)
		os.Exit(1)
	}
	port := uint32(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	// The downloaded archive goes to LO_TEST_PG_CACHE when it is set so that repeated runs and
	// CI can reuse it; runtime and data stay in the per-run temporary directory.
	cachePath := embeddedpg.CachePath(filepath.Join(temp, "cache"))
	// Prepare downloads with retries and a mandatory checksum; a failure here is final and
	// never skips the PostgreSQL tests.
	if err := embeddedpg.Prepare(embeddedpostgres.V18, cachePath); err != nil {
		fmt.Fprintf(os.Stderr, "prepare embedded postgres archive: %v\n", err)
		_ = os.RemoveAll(temp)
		os.Exit(1)
	}
	config := embeddedpostgres.DefaultConfig().
		Port(port).
		CachePath(cachePath).
		RuntimePath(filepath.Join(temp, "runtime")).
		DataPath(filepath.Join(temp, "data")).
		StartTimeout(2 * time.Minute).
		Logger(io.Discard)
	postgres := embeddedpostgres.NewDatabase(config)
	if err := postgres.Start(); err != nil {
		fmt.Fprintf(
			os.Stderr,
			"start embedded postgres: %v (%s)\n",
			err,
			embeddedpg.DownloadProbe(embeddedpostgres.V18),
		)
		_ = os.RemoveAll(temp)
		os.Exit(1)
	}
	postgresAdminURL = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	code := m.Run()
	if err := postgres.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop embedded postgres:", err)
		code = 1
	}
	_ = os.RemoveAll(temp)
	os.Exit(code)
}

func forEachDialect(t *testing.T, test func(*testing.T, *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("LO_TEST_PG") != "1" {
			t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
		}
		store := newPostgresTestStore(t)
		test(t, store)
	})
}

func newPostgresTestStore(t *testing.T) *Store {
	t.Helper()
	dbURL := newPostgresTestDatabase(t)
	store, err := OpenStore(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newPostgresTestDatabase(t *testing.T) string {
	t.Helper()
	database := fmt.Sprintf("lazy_observe_test_%d", postgresDBSeq.Add(1))
	admin, err := sql.Open("pgx", postgresAdminURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE "`+database+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create postgres test database: %v", err)
	}
	dbURL := postgresAdminURL[:len(postgresAdminURL)-len("postgres?sslmode=disable")] + database + "?sslmode=disable"
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE "`+database+`" WITH (FORCE)`)
		_ = admin.Close()
	})
	return dbURL
}

func TestPostgresMigrationLockTimeoutAndCancellation(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	dbURL := newPostgresTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	holder, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	lockKey, err := postgresMigrationLockKey(context.Background(), holder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = holder.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) })

	if _, err := OpenStoreWithOptions(
		context.Background(),
		dbURL,
		OpenOptions{MigrationLockTimeout: 100 * time.Millisecond},
	); err == nil ||
		!strings.Contains(err.Error(), "timed out acquiring") {
		t.Fatalf("lock timeout error=%v", err)
	}
	assertPostgresSchemaVersionAbsent(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := OpenStoreWithOptions(
		ctx,
		dbURL,
		OpenOptions{MigrationLockTimeout: time.Second},
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("lock cancellation error=%v, want context.Canceled", err)
	}
	assertPostgresSchemaVersionAbsent(t, db)
}

func TestPostgresConcurrentBaselineStartup(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	dbURL := newPostgresTestDatabase(t)
	start := make(chan struct{})
	stores := make(chan *Store, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := OpenStoreWithOptions(
				context.Background(),
				dbURL,
				OpenOptions{MigrationLockTimeout: 5 * time.Second},
			)
			if err != nil {
				errs <- err
				return
			}
			stores <- store
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(stores)
	for err := range errs {
		t.Fatalf("concurrent OpenStore: %v", err)
	}
	opened := 0
	for store := range stores {
		opened++
		_ = store.Close()
	}
	if opened != 2 {
		t.Fatalf("opened stores=%d, want 2", opened)
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entries, err := migrationEntries(migrationFiles, "migrations/postgres")
	if err != nil {
		t.Fatal(err)
	}
	var count, distinct int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_version`).
		Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != len(entries) || distinct != len(entries) {
		t.Fatalf("migration rows count=%d distinct=%d want=%d", count, distinct, len(entries))
	}
}

func TestPostgresMigrationFailureReleasesLock(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	dbURL := newPostgresTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE projects (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStoreWithOptions(
		context.Background(),
		dbURL,
		OpenOptions{MigrationLockTimeout: time.Second},
	); err == nil ||
		!strings.Contains(err.Error(), "apply migration 1") {
		t.Fatalf("migration failure error=%v", err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	lockKey, err := postgresMigrationLockKey(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	if err := conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, lockKey).
		Scan(&acquired); err != nil ||
		!acquired {
		t.Fatalf("acquire after failed migration: acquired=%v err=%v", acquired, err)
	}
	_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
}

func TestPostgresConcurrentBootstrap(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	dbURL := newPostgresTestDatabase(t)
	storeA, err := OpenStore(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := OpenStore(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	type result struct {
		selfProject ProjectInfo
		selfToken   string
		logs        string
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	// Each replica has its own data_dir; only the bootstrap winner may write
	// the generated password file into it.
	passwordFiles := []string{
		filepath.Join(t.TempDir(), InitialAdminPasswordFileName),
		filepath.Join(t.TempDir(), InitialAdminPasswordFileName),
	}
	for index, store := range []*Store{storeA, storeB} {
		go func(store *Store, passwordFile string) {
			<-start
			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&output, nil))
			admin := BootstrapAdmin{GeneratedPasswordFile: passwordFile}
			if _, err := store.Bootstrap(context.Background(), logger, admin); err != nil {
				results <- result{err: err}
				return
			}
			project, token, err := store.BootstrapSelfMonitoring(
				context.Background(),
				"lo_test-self-monitoring-derived-token-value",
			)
			results <- result{selfProject: project, selfToken: token, logs: output.String(), err: err}
		}(store, passwordFiles[index])
	}
	close(start)
	first, second := <-results, <-results
	for _, result := range []result{first, second} {
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	if first.selfProject.ID != second.selfProject.ID || first.selfToken == "" || first.selfToken != second.selfToken {
		t.Fatalf(
			"self-monitoring bootstrap differs: first=(%#v,%q) second=(%#v,%q)",
			first.selfProject,
			first.selfToken,
			second.selfProject,
			second.selfToken,
		)
	}
	combinedLogs := first.logs + second.logs
	if strings.Count(combinedLogs, "created bootstrap admin") != 1 ||
		strings.Count(combinedLogs, "initial admin password written") != 1 ||
		strings.Count(combinedLogs, "created default project") != 1 {
		t.Fatalf("bootstrap creation logs were not single-winner:\n%s", combinedLogs)
	}
	written := 0
	for _, passwordFile := range passwordFiles {
		password, err := os.ReadFile(passwordFile)
		switch {
		case err == nil:
			written++
			if strings.Contains(combinedLogs, strings.TrimSpace(string(password))) {
				t.Fatal("bootstrap logs contain the generated admin password")
			}
		case !errors.Is(err, os.ErrNotExist):
			t.Fatal(err)
		}
	}
	if written != 1 {
		t.Fatalf("generated admin password files = %d, want exactly the bootstrap winner's", written)
	}

	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM users WHERE username = 'admin'`:                                                                                  1,
		`SELECT COUNT(*) FROM projects WHERE slug IN ('default', 'lazy-observe')`:                                                              2,
		`SELECT COUNT(*) FROM project_api_tokens t JOIN projects p ON p.id = t.project_id WHERE p.slug = 'default'`:                            1,
		`SELECT COUNT(*) FROM project_api_tokens t JOIN projects p ON p.id = t.project_id WHERE p.slug = 'lazy-observe' AND t.revoked = FALSE`: 1,
	} {
		var count int
		if err := storeA.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatalf("bootstrap invariant query=%q count=%d want=%d err=%v", query, count, want, err)
		}
	}
}

func TestPostgresMigrationConnectionLossReleasesLock(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	dbURL := newPostgresTestDatabase(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	lockKey, err := acquirePostgresMigrationLock(context.Background(), conn, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var backendPID int
	if err := conn.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	killer, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer killer.Close()
	var terminated bool
	if err := killer.QueryRow(`SELECT pg_terminate_backend($1)`, backendPID).
		Scan(&terminated); err != nil ||
		!terminated {
		t.Fatalf("terminate migration connection: terminated=%v err=%v", terminated, err)
	}
	if err := releasePostgresMigrationLock(conn, lockKey); err == nil {
		t.Fatal("release on terminated migration connection unexpectedly succeeded")
	}

	replacement, err := killer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	var acquired bool
	if err := replacement.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, lockKey).
		Scan(&acquired); err != nil ||
		!acquired {
		t.Fatalf("acquire after connection loss: acquired=%v err=%v", acquired, err)
	}
	_, _ = replacement.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
}

func assertPostgresSchemaVersionAbsent(t *testing.T, db *sql.DB) {
	t.Helper()
	var relation sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('public.schema_version')`).Scan(&relation); err != nil {
		t.Fatal(err)
	}
	if relation.Valid {
		t.Fatalf("schema_version unexpectedly exists as %q", relation.String)
	}
}
