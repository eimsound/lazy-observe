package meta

import (
	"context"
	"strings"
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
