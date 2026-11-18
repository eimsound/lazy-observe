package meta

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPlatformKeyFingerprintRecordsFirstValueAndKeepsIt(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		first, created, err := store.EnsurePlatformKeyFingerprint(ctx, "fingerprint-a")
		if err != nil {
			t.Fatalf("first EnsurePlatformKeyFingerprint: %v", err)
		}
		if !created {
			t.Fatal("first EnsurePlatformKeyFingerprint did not report a write")
		}
		if first.Fingerprint != "fingerprint-a" {
			t.Fatalf("stored fingerprint = %q, want %q", first.Fingerprint, "fingerprint-a")
		}
		if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
			t.Fatalf("stored fingerprint timestamps = %#v, want both set", first)
		}

		// The same key on the next start reads the recorded value back
		// without rewriting it.
		same, created, err := store.EnsurePlatformKeyFingerprint(ctx, "fingerprint-a")
		if err != nil {
			t.Fatalf("second EnsurePlatformKeyFingerprint: %v", err)
		}
		if created {
			t.Fatal("second EnsurePlatformKeyFingerprint reported a write")
		}
		if !same.CreatedAt.Equal(first.CreatedAt) || !same.UpdatedAt.Equal(first.UpdatedAt) {
			t.Fatalf("matching start rewrote the row: %#v != %#v", same, first)
		}

		// A different key never overwrites the record: the caller must see the
		// mismatch and refuse to start.
		other, created, err := store.EnsurePlatformKeyFingerprint(ctx, "fingerprint-b")
		if err != nil {
			t.Fatalf("mismatched EnsurePlatformKeyFingerprint: %v", err)
		}
		if created {
			t.Fatal("mismatched EnsurePlatformKeyFingerprint overwrote the record")
		}
		if other.Fingerprint != "fingerprint-a" {
			t.Fatalf("recorded fingerprint = %q after a mismatch, want %q", other.Fingerprint, "fingerprint-a")
		}

		count, err := store.db.QueryContext(ctx, `SELECT COUNT(*) FROM platform_key_fingerprints`)
		if err != nil {
			t.Fatal(err)
		}
		defer count.Close()
		if !count.Next() {
			t.Fatal("count platform_key_fingerprints returned no row")
		}
		var rows int
		if err := count.Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("platform_key_fingerprints row count = %d, want 1", rows)
		}
	})
}

func TestPlatformKeyFingerprintReplaceAcceptsRotation(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		if _, _, err := store.EnsurePlatformKeyFingerprint(ctx, "fingerprint-a"); err != nil {
			t.Fatal(err)
		}
		replaced, err := store.ReplacePlatformKeyFingerprint(ctx, "fingerprint-b")
		if err != nil {
			t.Fatalf("ReplacePlatformKeyFingerprint: %v", err)
		}
		if replaced.Fingerprint != "fingerprint-b" {
			t.Fatalf("replaced fingerprint = %q, want %q", replaced.Fingerprint, "fingerprint-b")
		}
		ensured, created, err := store.EnsurePlatformKeyFingerprint(ctx, "fingerprint-b")
		if err != nil {
			t.Fatal(err)
		}
		if created || ensured.Fingerprint != "fingerprint-b" {
			t.Fatalf("Ensure after replace = %#v created=%v, want the rotated value", ensured, created)
		}
	})
}

// TestPlatformKeyFingerprintReplaceCreatesOnEmptyTable covers an operator who
// resets the fingerprint before the first server start.
func TestPlatformKeyFingerprintReplaceCreatesOnEmptyTable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		replaced, err := store.ReplacePlatformKeyFingerprint(context.Background(), "fingerprint-a")
		if err != nil {
			t.Fatalf("ReplacePlatformKeyFingerprint on empty table: %v", err)
		}
		if replaced.Fingerprint != "fingerprint-a" {
			t.Fatalf("replaced fingerprint = %q, want %q", replaced.Fingerprint, "fingerprint-a")
		}
	})
}

// TestPlatformKeyFingerprintConcurrentFirstWrite covers replicas that start at
// the same time against an empty row: exactly one fingerprint is recorded and
// every caller reads that one back, so the later starters compare instead of
// overwriting.
func TestPlatformKeyFingerprintConcurrentFirstWrite(t *testing.T) {
	race := func(t *testing.T, dbURL string) {
		ctx := context.Background()
		stores := openPlatformKeyStores(t, dbURL, 5)
		type outcome struct {
			record  PlatformKeyFingerprint
			created bool
			err     error
		}
		outcomes := make([]outcome, len(stores))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, store := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				record, created, err := store.EnsurePlatformKeyFingerprint(
					ctx, "fingerprint-"+string(rune('a'+i)),
				)
				outcomes[i] = outcome{record: record, created: created, err: err}
			}()
		}
		close(start)
		wg.Wait()

		winners := 0
		recorded := ""
		for i, got := range outcomes {
			if got.err != nil {
				t.Fatalf("concurrent EnsurePlatformKeyFingerprint[%d]: %v", i, got.err)
			}
			if got.created {
				winners++
			}
			if recorded == "" {
				recorded = got.record.Fingerprint
			}
			if got.record.Fingerprint != recorded {
				t.Fatalf(
					"concurrent starters read different fingerprints: %q vs %q", got.record.Fingerprint, recorded,
				)
			}
		}
		if winners != 1 {
			t.Fatalf("concurrent first writes that actually inserted = %d, want 1", winners)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		race(t, platformKeyDBURL(t, "sqlite"))
	})
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("LO_TEST_PG") != "1" {
			t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
		}
		race(t, newPostgresTestDatabase(t))
	})
}

func platformKeyDBURL(t *testing.T, dialectName string) string {
	t.Helper()
	if dialectName == "postgres" {
		return newPostgresTestDatabase(t)
	}
	return filepath.Join(t.TempDir(), "meta.db")
}

func openPlatformKeyStores(t *testing.T, dbURL string, count int) []*Store {
	t.Helper()
	stores := make([]*Store, 0, count)
	for range count {
		store, err := OpenStore(context.Background(), dbURL)
		if err != nil {
			t.Fatalf("OpenStore %s: %v", dbURL, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		stores = append(stores, store)
	}
	return stores
}
