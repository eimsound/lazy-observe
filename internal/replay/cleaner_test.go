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
