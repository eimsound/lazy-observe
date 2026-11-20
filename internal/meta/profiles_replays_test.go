package meta

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func TestProfileAndReplayNonUTCTimeQueriesAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Timezone", "timezone")
		if err != nil {
			t.Fatal(err)
		}
		plus8 := time.FixedZone("UTC+8", 8*3600)
		minus5 := time.FixedZone("UTC-5", -5*3600)

		utcStart := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
		utcEnd := utcStart.Add(10 * time.Second)
		if _, err := store.CreateProfileMeta(
			ctx,
			project.ID,
			"svc",
			ProfileTypeCPU,
			utcStart,
			utcEnd,
			128,
			strings.Repeat("1", 32),
			strings.Repeat("a", 64),
			"profiles/timezone/svc/cpu/1.pb.gz",
			map[string]string{},
		); err != nil {
			t.Fatal(err)
		}

		qStart := utcStart.Add(-5 * time.Second).In(plus8)
		qEnd := utcEnd.Add(5 * time.Second).In(minus5)
		profiles, err := store.ListProfiles(ctx, project.ID, ProfileListOptions{
			Service: "svc",
			Type:    ProfileTypeCPU,
			Start:   &qStart,
			End:     &qEnd,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(profiles) != 1 {
			t.Fatalf("ListProfiles with non-UTC window returned %d rows, want 1", len(profiles))
		}

		expired, err := store.ListExpiredProfiles(ctx, utcEnd.Add(time.Second).In(plus8), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(expired) != 1 {
			t.Fatalf("ListExpiredProfiles with non-UTC cutoff returned %d rows, want 1", len(expired))
		}

		if err := store.UpsertReplaySession(
			ctx,
			project.ID,
			"sess-tz",
			utcStart,
			utcEnd,
			1,
			100,
			false,
			"v1",
			"https://example.test/home",
		); err != nil {
			t.Fatal(err)
		}

		sessions, err := store.ListReplaySessions(ctx, project.ID, ReplayListOptions{
			Start: &qStart,
			End:   &qEnd,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 1 {
			t.Fatalf("ListReplaySessions with non-UTC window returned %d rows, want 1", len(sessions))
		}

		expiredSessions, err := store.ListExpiredReplaySessions(ctx, utcEnd.Add(time.Second).In(plus8), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(expiredSessions) != 1 {
			t.Fatalf("ListExpiredReplaySessions with non-UTC cutoff returned %d rows, want 1", len(expiredSessions))
		}
	})
}

func TestReplayUpsertMonotonicStatsAndHugePageAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "ReplayStats", "replay-stats")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
		if err := store.UpsertReplaySession(
			ctx,
			project.ID,
			"sess-race",
			now,
			now.Add(5*time.Second),
			5,
			5000,
			true,
			"1.0.0",
			"https://example.test/a",
		); err != nil {
			t.Fatal(err)
		}
		// Simulate a slower HA replica writing a stale lower count/bytes.
		if err := store.UpsertReplaySession(
			ctx,
			project.ID,
			"sess-race",
			now.Add(time.Second),
			now.Add(2*time.Second),
			2,
			1200,
			false,
			"",
			"",
		); err != nil {
			t.Fatal(err)
		}
		row, err := store.GetReplaySession(ctx, project.ID, "sess-race")
		if err != nil {
			t.Fatal(err)
		}
		if row.SegmentCount != 5 || row.TotalBytes != 5000 {
			t.Fatalf(
				"stale replay upsert regressed stats: count=%d bytes=%d, want 5/5000",
				row.SegmentCount,
				row.TotalBytes,
			)
		}

		// Huge page must not overflow offset on either SQLite or PostgreSQL.
		rows, err := store.ListReplaySessions(ctx, project.ID, ReplayListOptions{
			Page:  math.MaxInt,
			Limit: 50,
		})
		if err != nil {
			t.Fatalf("huge page failed: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("huge page returned %d rows, want 0", len(rows))
		}
	})
}

func TestIssueIDsForErrorEventsBulkAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		p1, err := store.CreateProject(ctx, "Bulk1", "bulk-1")
		if err != nil {
			t.Fatal(err)
		}
		p2, err := store.CreateProject(ctx, "Bulk2", "bulk-2")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
		for _, tc := range []struct {
			projectID int
			eventID   string
			fp        string
		}{
			{p1.ID, "ev-1", "fp-1"},
			{p1.ID, "ev-2", "fp-2"},
			{p2.ID, "ev-other", "fp-other"},
		} {
			payload := []byte(`{"event_id":"` + tc.eventID + `"}`)
			if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
				ProjectID: tc.projectID, EventID: tc.eventID, CanonicalDigest: tc.eventID,
				Payload: payload, PayloadBytes: int64(len(payload)),
				OccurredAt: now, AcceptedAt: now,
			}}, errortrack.InboxCapacity{MaxItems: 20, MaxBytes: 1 << 20}); err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimErrorEvents(ctx, "bulk-worker", 1, now.Add(time.Second), time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim %s: %v", tc.eventID, err)
			}
			if _, err := store.CompleteErrorEvent(
				ctx,
				claimed[0].ID,
				"bulk-worker",
				claimed[0].ClaimEpoch,
				errortrack.IssueDelta{
					ProjectID: tc.projectID, Fingerprint: tc.fp, Title: tc.fp, Level: "error",
					Count: 1, FirstSeen: now, LastSeen: now, LastEventID: tc.eventID,
					AcceptedAt: now, AppliedAt: now.Add(2 * time.Second),
				},
				now.Add(2*time.Second),
			); err != nil {
				t.Fatal(err)
			}
		}

		got, err := store.IssueIDsForErrorEvents(ctx, p1.ID, []string{"ev-1", "ev-2", "ev-1", "ev-other", "missing"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got["ev-1"] <= 0 || got["ev-2"] <= 0 {
			t.Fatalf("bulk lookup=%v want ev-1 and ev-2 only", got)
		}
	})
}
