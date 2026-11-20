package replay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestCleanerDeletesOnlyExpiredReplay(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	); err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertReplaySession(
		ctx,
		project.ID,
		"expired",
		now.Add(-73*time.Hour),
		now.Add(-73*time.Hour),
		1,
		3,
		false,
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertReplaySession(
		ctx,
		project.ID,
		"active",
		now.Add(-71*time.Hour),
		now.Add(-71*time.Hour),
		1,
		3,
		false,
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, "replay/default/expired/0.json.gz", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, "replay/default/active/0.json.gz", strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}

	cleaner := &Cleaner{Metadata: store, Blobs: blobs}
	deleted, err := cleaner.Clean(ctx, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}
	if _, err := store.GetReplaySession(ctx, project.ID, "expired"); !meta.IsNotFound(err) {
		t.Fatalf("expired metadata error=%v", err)
	}
	if _, err := blobs.Get(ctx, "replay/default/expired/0.json.gz"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("expired blob error=%v", err)
	}
	if _, err := store.GetReplaySession(ctx, project.ID, "active"); err != nil {
		t.Fatalf("active metadata: %v", err)
	}
	r, err := blobs.Get(ctx, "replay/default/active/0.json.gz")
	if err != nil {
		t.Fatalf("active blob: %v", err)
	}
	r.Close()
}

type failingPrefixBlobStore struct {
	blob.Store
	failPrefix string
}

func (f *failingPrefixBlobStore) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == f.failPrefix {
		return errors.New("simulated s3 failure")
	}
	return f.Store.DeletePrefix(ctx, prefix)
}

func TestCleanerContinuesPastSingleRowError(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	); err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"bad-sess", "good-sess"} {
		if err := store.UpsertReplaySession(
			ctx, project.ID, id, now.Add(-73*time.Hour), now.Add(-73*time.Hour), 1, 3, false, "", "",
		); err != nil {
			t.Fatal(err)
		}
	}
	localBlobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := localBlobs.Put(ctx, "replay/default/bad-sess/0.json.gz", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if err := localBlobs.Put(ctx, "replay/default/good-sess/0.json.gz", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}

	cleaner := &Cleaner{
		Metadata: store,
		Blobs:    &failingPrefixBlobStore{Store: localBlobs, failPrefix: "replay/default/bad-sess/"},
	}
	deleted, err := cleaner.Clean(ctx, now.Add(-72*time.Hour))
	if err == nil {
		t.Fatal("expected aggregated error for failing row")
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d want=1", deleted)
	}
	if _, err := store.GetReplaySession(ctx, project.ID, "good-sess"); !meta.IsNotFound(err) {
		t.Fatalf("good-sess should have been deleted despite earlier failure, err=%v", err)
	}
	if _, err := store.GetReplaySession(ctx, project.ID, "bad-sess"); err != nil {
		t.Fatalf("bad-sess metadata should remain for retry, err=%v", err)
	}
}

func TestCleanerSkipsSessionWithRecentSegmentAndPreservesRefreshedMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	); err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.UpsertReplaySession(
		ctx, project.ID, "recent-blob-sess", now.Add(-73*time.Hour), now.Add(-73*time.Hour), 1, 3, false, "", "",
	); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, "replay/default/recent-blob-sess/1.json.gz", strings.NewReader("fresh")); err != nil {
		t.Fatal(err)
	}

	// Within grace period: newly arrived segment protects the session from cleanup.
	cleaner := &Cleaner{Metadata: store, Blobs: blobs, GracePeriod: DefaultGracePeriod}
	deleted, err := cleaner.Clean(ctx, now.Add(-72*time.Hour))
	if err != nil || deleted != 0 {
		t.Fatalf("grace-period cleanup deleted=%d err=%v want 0", deleted, err)
	}
	if _, err := blobs.Get(ctx, "replay/default/recent-blob-sess/1.json.gz"); err != nil {
		t.Fatalf("recent segment blob was unexpectedly deleted: %v", err)
	}
	if _, err := store.GetReplaySession(ctx, project.ID, "recent-blob-sess"); err != nil {
		t.Fatalf("recent session metadata was unexpectedly deleted: %v", err)
	}

	// Conditional metadata delete: if last_seen_at refreshed after candidate listing,
	// DeleteExpiredReplaySession refuses to delete the active session row.
	if err := store.UpsertReplaySession(
		ctx, project.ID, "recent-blob-sess", now.Add(-73*time.Hour), now, 2, 10, false, "", "",
	); err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetReplaySession(ctx, project.ID, "recent-blob-sess")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.DeleteExpiredReplaySession(ctx, sess.ID, now.Add(-72*time.Hour))
	if err != nil || removed {
		t.Fatalf("DeleteExpiredReplaySession removed=%v err=%v want false", removed, err)
	}
	if _, err := store.GetReplaySession(ctx, project.ID, "recent-blob-sess"); err != nil {
		t.Fatalf("refreshed session was deleted: %v", err)
	}
}

func TestCleanerSweepsOrphanReplayBlobsAndPreservesLegacySessions(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	); err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	// Committed session with replay_segments row for seq 0.
	if _, err := store.UpsertReplaySegment(ctx, meta.ReplaySegmentInput{
		ProjectID:     project.ID,
		SessionID:     "committed-sess",
		Seq:           0,
		StartedAt:     now.Add(-time.Hour),
		LastSeenAt:    now.Add(-time.Hour),
		EventCount:    1,
		SizeBytes:     4,
		PayloadSHA256: strings.Repeat("a", 64),
		MarkersJSON:   "[]",
	}); err != nil {
		t.Fatal(err)
	}
	// Legacy session without replay_segments rows.
	if err := store.UpsertReplaySession(
		ctx, project.ID, "legacy-sess", now.Add(-time.Hour), now.Add(-time.Hour), 1, 4, false, "", "",
	); err != nil {
		t.Fatal(err)
	}

	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	committedKey := "replay/default/committed-sess/0.json.gz"
	uncommittedSeqKey := "replay/default/committed-sess/99.json.gz"
	orphanSessionKey := "replay/default/ghost-sess/0.json.gz"
	legacyKey := "replay/default/legacy-sess/0.json.gz"
	for _, key := range []string{committedKey, uncommittedSeqKey, orphanSessionKey, legacyKey} {
		if err := blobs.Put(ctx, key, strings.NewReader("blob")); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := now.Add(-72 * time.Hour)
	// Within grace period: nothing swept.
	if cleaned, err := (&Cleaner{Metadata: store, Blobs: blobs}).Clean(ctx, cutoff); err != nil || cleaned != 0 {
		t.Fatalf("within-grace cleaned=%d err=%v want 0", cleaned, err)
	}
	// Past grace period: orphan session blob and uncommitted seq blob are swept;
	// committed segment and legacy session blob remain intact.
	cleaned, err := (&Cleaner{
		Metadata: store,
		Blobs:    blobs,
		Now:      func() time.Time { return time.Now().UTC().Add(DefaultGracePeriod + time.Minute) },
	}).Clean(ctx, cutoff)
	if err != nil || cleaned != 2 {
		t.Fatalf("past-grace cleaned=%d err=%v want 2", cleaned, err)
	}
	for _, keep := range []string{committedKey, legacyKey} {
		if _, err := blobs.Get(ctx, keep); err != nil {
			t.Fatalf("expected blob %s to remain: %v", keep, err)
		}
	}
	for _, gone := range []string{uncommittedSeqKey, orphanSessionKey} {
		if _, err := blobs.Get(ctx, gone); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("orphan blob %s still exists: err=%v", gone, err)
		}
	}
}
