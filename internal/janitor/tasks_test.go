package janitor

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
	"github.com/eimsound/lazy-observe/internal/db/ent/issue"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func openTestStore(t *testing.T) (*meta.Store, meta.ProjectInfo) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
	return store, project
}

func TestProfilesDeleteExpiredBlobAndMetadata(t *testing.T) {
	ctx := context.Background()
	store, project := openTestStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		end  time.Time
	}{
		{"expired", now.Add(-8 * 24 * time.Hour)},
		{"active", now.Add(-6 * 24 * time.Hour)},
	} {
		key := "profiles/default/service/cpu/" + item.name + ".pb.gz"
		if err := blobs.Put(ctx, key, strings.NewReader(item.name)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateProfileMeta(
			ctx,
			project.ID,
			"service",
			meta.ProfileTypeCPU,
			item.end.Add(-10*time.Second),
			item.end,
			int64(len(item.name)),
			strings.Repeat(item.name[:1], 32),
			strings.Repeat("a", 64),
			key,
			map[string]string{},
		); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := (&Profiles{Metadata: store, Blobs: blobs}).Clean(ctx, now.Add(-7*24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if _, err := blobs.Get(ctx, "profiles/default/service/cpu/expired.pb.gz"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("expired blob error=%v", err)
	}
	if _, err := store.GetProfile(ctx, project.ID, 1); !meta.IsNotFound(err) {
		t.Fatalf("expired metadata error=%v", err)
	}
	if _, err := blobs.Get(ctx, "profiles/default/service/cpu/active.pb.gz"); err != nil {
		t.Fatalf("active blob error=%v", err)
	}
}

type failingKeyBlobStore struct {
	blob.Store
	failKey string
}

func (f *failingKeyBlobStore) Delete(ctx context.Context, key string) error {
	if key == f.failKey {
		return errors.New("simulated delete failure")
	}
	return f.Store.Delete(ctx, key)
}

func TestProfilesCleanHandlesMissingBlobAndSingleRowError(t *testing.T) {
	ctx := context.Background()
	store, project := openTestStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	localBlobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	end := now.Add(-8 * 24 * time.Hour)
	for i, item := range []struct {
		key      string
		putBlob  bool
		uploadID string
	}{
		{"profiles/default/svc/cpu/fail.pb.gz", true, strings.Repeat("1", 32)},
		{"profiles/default/svc/cpu/missing.pb.gz", false, strings.Repeat("2", 32)},
		{"profiles/default/svc/cpu/good.pb.gz", true, strings.Repeat("3", 32)},
	} {
		if item.putBlob {
			if err := localBlobs.Put(ctx, item.key, strings.NewReader("data")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.CreateProfileMeta(
			ctx, project.ID, "svc", meta.ProfileTypeCPU,
			end.Add(-time.Duration(i+1)*time.Second), end, 4,
			item.uploadID, strings.Repeat("a", 64), item.key, map[string]string{},
		); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := (&Profiles{
		Metadata: store,
		Blobs:    &failingKeyBlobStore{Store: localBlobs, failKey: "profiles/default/svc/cpu/fail.pb.gz"},
	}).Clean(ctx, now.Add(-7*24*time.Hour))
	if err == nil {
		t.Fatal("expected error for failing blob")
	}
	if deleted != 2 {
		t.Fatalf("deleted=%d want=2 (missing + good)", deleted)
	}
}

func revisionKey(digest string) string { return "sourcemaps/default/revisions/" + digest + ".map" }

func digestOf(c byte) string { return strings.Repeat(string(c), 64) }

// Source map bodies are content-addressed under sourcemaps/<slug>/revisions/
// and may be shared by several releases. Retention removes a revision only when
// no remaining release and no pending Inbox snapshot (ADR 0007) references it.
func TestSourceMapsDeleteOnlyExpiredWholeReleases(t *testing.T) {
	ctx := context.Background()
	store, project := openTestStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-91*24*time.Hour), now.Add(-89*24*time.Hour)
	exclusive, shared, pinned, activeOnly, otherRelease := digestOf('a'), digestOf('b'), digestOf('c'), digestOf('d'),
		digestOf('e')
	for _, artifact := range []struct {
		release, filename, digest string
		created                   time.Time
	}{
		{"expired", "only.js", exclusive, old},
		{"expired", "shared.js", shared, old},
		{"active", "shared.js", shared, old},
		{"active", "new.js", activeOnly, recent},
		{"pinned", "pinned.js", pinned, old},
		// A release literally named "revisions" must not select the whole
		// revision namespace of the project.
		{"revisions", "x.js", otherRelease, old},
	} {
		if err := store.UpsertSourceMapArtifact(
			ctx, project.ID, artifact.release, artifact.filename, 3, artifact.digest, artifact.created,
		); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte(`{"event_id":"pending-1"}`)
	if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
		ProjectID: project.ID, EventID: "pending-1", Release: "pinned", CanonicalDigest: "pending-1",
		Payload: payload, PayloadBytes: int64(len(payload)), OccurredAt: now, AcceptedAt: now,
	}}, errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// strayRevision is left behind by an earlier manual delete: no metadata at all.
	strayRevision := digestOf('f')
	for _, digest := range []string{exclusive, shared, pinned, activeOnly, otherRelease, strayRevision} {
		if err := blobs.Put(ctx, revisionKey(digest), strings.NewReader("map")); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := (&SourceMaps{Metadata: store, Blobs: blobs}).Clean(ctx, now.Add(-90*24*time.Hour))
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	for digest, wantPresent := range map[string]bool{
		exclusive: false, otherRelease: false, strayRevision: false, shared: true, activeOnly: true, pinned: true,
	} {
		_, err := blobs.Get(ctx, revisionKey(digest))
		if present := err == nil; present != wantPresent {
			t.Errorf("revision %s present=%v want %v (err=%v)", digest[:1], present, wantPresent, err)
		}
	}
	for release, want := range map[string]int{"expired": 0, "revisions": 0, "pinned": 0, "active": 2} {
		if rows, err := store.SourceMapArtifacts(ctx, project.ID, release); err != nil || len(rows) != want {
			t.Errorf("%s metadata count=%d want %d err=%v", release, len(rows), want, err)
		}
	}
}

func TestAlertEventsDeleteOnlyExpired(t *testing.T) {
	ctx := context.Background()
	store, project := openTestStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug,
		Name:        "retention",
		Signal:      "metrics",
		Expression:  "up == 0",
		For:         "0s",
		Severity:    "warning",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		fingerprint string
		updated     time.Time
	}{{"expired", now.Add(-31 * 24 * time.Hour)}, {"active", now.Add(-29 * 24 * time.Hour)}} {
		_, err := store.CreateAlertEvent(ctx, meta.AlertEventCreate{
			RuleID: rule.ID, Fingerprint: event.fingerprint, Status: "resolved", Labels: map[string]string{},
			Annotations: map[string]string{}, StartsAt: event.updated, UpdatedAt: &event.updated,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := (AlertEvents{Metadata: store}).Clean(ctx, now.Add(-30*24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	events, err := store.ListAlertEvents(ctx, project.Slug, "", nil)
	if err != nil || len(events) != 1 || events[0].Fingerprint != "active" {
		t.Fatalf("retained events=%+v err=%v", events, err)
	}
}

func TestClosedIssuesDeleteOnlyExpiredResolvedOrIgnored(t *testing.T) {
	ctx := context.Background()
	store, project := openTestStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	retained := make(map[string]int)
	for _, item := range []struct {
		fingerprint string
		status      issue.Status
		seen        time.Time
	}{
		{"expired-resolved", issue.StatusResolved, now.Add(-91 * 24 * time.Hour)},
		{"expired-ignored", issue.StatusIgnored, now.Add(-91 * 24 * time.Hour)},
		{"old-open", issue.StatusOpen, now.Add(-91 * 24 * time.Hour)},
		{"active-resolved", issue.StatusResolved, now.Add(-89 * 24 * time.Hour)},
	} {
		changes, err := store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{{
			ProjectID: project.ID, Fingerprint: item.fingerprint, Title: item.fingerprint, Level: "error",
			Count: 7, SessionBits: 42, FirstSeen: item.seen, LastSeen: item.seen, LastEventID: "event",
			AcceptedAt: item.seen, AppliedAt: item.seen,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 1 {
			t.Fatalf("issue changes=%+v", changes)
		}
		if _, err := store.SetIssueStatus(ctx, project.ID, changes[0].IssueID, string(item.status), now); err != nil {
			t.Fatal(err)
		}
		retained[item.fingerprint] = changes[0].IssueID
	}
	deleted, err := (ClosedIssues{Metadata: store}).Clean(ctx, now.Add(-90*24*time.Hour))
	if err != nil || deleted != 2 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	for _, fingerprint := range []string{"old-open", "active-resolved"} {
		row, err := store.GetIssue(ctx, project.ID, retained[fingerprint])
		if err != nil || row.Fingerprint != fingerprint {
			t.Fatalf("retained %s row=%+v err=%v", fingerprint, row, err)
		}
	}
}
