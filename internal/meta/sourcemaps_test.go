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

func TestListSourceMapReleasesSQLPushdownAggregationAndLimit(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		p1, err := store.CreateProject(ctx, "ReleasesOne", "releases-one")
		if err != nil {
			t.Fatal(err)
		}
		p2, err := store.CreateProject(ctx, "ReleasesTwo", "releases-two")
		if err != nil {
			t.Fatal(err)
		}
		base := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
		// Release v1: 2 files, newest at base+1m, total size 300.
		if err := store.UpsertSourceMapArtifact(
			ctx, p1.ID, "v1", "a.js", 100, strings.Repeat("1", 64), base,
		); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertSourceMapArtifact(
			ctx, p1.ID, "v1", "b.js", 200, strings.Repeat("2", 64), base.Add(time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		// Release v2: 1 file at base+3m, size 500.
		if err := store.UpsertSourceMapArtifact(
			ctx, p1.ID, "v2", "app.js", 500, strings.Repeat("3", 64), base.Add(3*time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		// Release v3: 3 files, newest at base+2m, total size 60.
		for idx := range 3 {
			if err := store.UpsertSourceMapArtifact(
				ctx,
				p1.ID,
				"v3",
				fmt.Sprintf("chunk-%d.js", idx),
				20,
				strings.Repeat("4", 64),
				base.Add(2*time.Minute),
			); err != nil {
				t.Fatal(err)
			}
		}
		// Other project release must remain isolated.
		if err := store.UpsertSourceMapArtifact(
			ctx, p2.ID, "other-rel", "x.js", 999, strings.Repeat("5", 64), base.Add(10*time.Minute),
		); err != nil {
			t.Fatal(err)
		}

		all, err := store.ListSourceMapReleases(ctx, p1.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 3 {
			t.Fatalf("len(all)=%d want 3: %#v", len(all), all)
		}
		if all[0].Release != "v2" || all[0].Files != 1 || all[0].Size != 500 ||
			!all[0].CreatedAt.Equal(base.Add(3*time.Minute)) {
			t.Fatalf("all[0]=%#v", all[0])
		}
		if all[1].Release != "v3" || all[1].Files != 3 || all[1].Size != 60 ||
			!all[1].CreatedAt.Equal(base.Add(2*time.Minute)) {
			t.Fatalf("all[1]=%#v", all[1])
		}
		if all[2].Release != "v1" || all[2].Files != 2 || all[2].Size != 300 ||
			!all[2].CreatedAt.Equal(base.Add(time.Minute)) {
			t.Fatalf("all[2]=%#v", all[2])
		}

		limited, err := store.ListSourceMapReleases(ctx, p1.ID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(limited) != 2 || limited[0].Release != "v2" || limited[1].Release != "v3" {
			t.Fatalf("limited=%#v", limited)
		}
	})
}
