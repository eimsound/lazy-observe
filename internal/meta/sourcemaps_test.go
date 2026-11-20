package meta

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func TestReferencedSourceMapDigestsIncludesArtifactsAndPendingSnapshots(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Maps", "maps")
		if err != nil {
			t.Fatal(err)
		}
		other, err := store.CreateProject(ctx, "Other", "other")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		live, pinned, foreign := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
		for _, artifact := range []struct {
			projectID       int
			release, digest string
		}{{project.ID, "live", live}, {project.ID, "pinned", pinned}, {other.ID, "live", foreign}} {
			if err := store.UpsertSourceMapArtifact(ctx, artifact.projectID, artifact.release, "app.js", 1,
				artifact.digest, now); err != nil {
				t.Fatal(err)
			}
		}
		payload := []byte(`{"event_id":"pinned"}`)
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
			ProjectID: project.ID, EventID: "pinned", Release: "pinned", CanonicalDigest: "pinned",
			Payload: payload, PayloadBytes: int64(len(payload)), OccurredAt: now, AcceptedAt: now,
		}}, errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSourceMapArtifacts(ctx, project.ID, "pinned"); err != nil {
			t.Fatal(err)
		}
		referenced, err := store.ReferencedSourceMapDigests(ctx, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !referenced[live] || !referenced[pinned] || referenced[foreign] || len(referenced) != 2 {
			t.Fatalf("referenced=%v", referenced)
		}
	})
}

func TestUpsertSourceMapArtifactsAtomicQuotaAndConcurrentUpsert(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "ConcurrentMaps", "concurrent-maps")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		// Concurrent upserts of the same filename must not fail with unique violation.
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				digest := strings.Repeat(fmt.Sprintf("%x", i), 64)
				errs <- store.UpsertSourceMapArtifacts(ctx, project.ID, "r-same", []SourceMapArtifactInput{{
					Filename: "app.min.js", Size: 100 + int64(i), SHA256: digest,
				}}, 10, 10000, now)
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent same-filename upsert failed: %v", err)
			}
		}

		// Concurrent quota race: 6 goroutines each uploading 1 file against maxFiles=3.
		quotaErrs := make(chan error, 6)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				quotaErrs <- store.UpsertSourceMapArtifacts(ctx, project.ID, "r-quota", []SourceMapArtifactInput{{
					Filename: fmt.Sprintf("chunk-%d.js", i), Size: 50, SHA256: strings.Repeat("a", 64),
				}}, 3, 10000, now)
			}(i)
		}
		wg.Wait()
		close(quotaErrs)
		okCount, exceededCount := 0, 0
		for err := range quotaErrs {
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrSourceMapQuotaExceeded):
				exceededCount++
			default:
				t.Fatalf("unexpected quota error: %v", err)
			}
		}
		if okCount != 3 || exceededCount != 3 {
			t.Fatalf("concurrent quota ok=%d exceeded=%d want 3 and 3", okCount, exceededCount)
		}
	})
}
