package meta

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
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

func TestReplaySegmentAtomicUpsertQuotasConcurrencyAndDeleteAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "ReplaySeg", "replay-seg")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

		// 1. Atomic insert + idempotent duplicate + conflict rejection.
		res, err := store.UpsertReplaySegment(ctx, ReplaySegmentInput{
			ProjectID:     project.ID,
			SessionID:     "sess-atomic",
			Seq:           0,
			StartedAt:     now,
			LastSeenAt:    now.Add(time.Second),
			EventCount:    5,
			SizeBytes:     120,
			PayloadSHA256: strings.Repeat("a", 64),
			MarkersJSON:   `[{"type":"http","time":1000}]`,
			HasError:      false,
			Release:       "1.0.0",
			PageURL:       "https://example.test/start",
			MaxSegments:   3,
			MaxBytes:      300,
		})
		if err != nil || res.Duplicate {
			t.Fatalf("first upsert res=%+v err=%v", res, err)
		}
		dupRes, err := store.UpsertReplaySegment(ctx, ReplaySegmentInput{
			ProjectID:     project.ID,
			SessionID:     "sess-atomic",
			Seq:           0,
			StartedAt:     now,
			LastSeenAt:    now.Add(time.Second),
			EventCount:    5,
			SizeBytes:     120,
			PayloadSHA256: strings.Repeat("a", 64),
			MarkersJSON:   `[{"type":"http","time":1000}]`,
			MaxSegments:   3,
			MaxBytes:      300,
		})
		if err != nil || !dupRes.Duplicate {
			t.Fatalf("duplicate upsert res=%+v err=%v want Duplicate=true", dupRes, err)
		}
		if _, err := store.UpsertReplaySegment(ctx, ReplaySegmentInput{
			ProjectID:     project.ID,
			SessionID:     "sess-atomic",
			Seq:           0,
			StartedAt:     now,
			LastSeenAt:    now.Add(time.Second),
			EventCount:    5,
			SizeBytes:     120,
			PayloadSHA256: strings.Repeat("b", 64),
			MarkersJSON:   `[]`,
			MaxSegments:   3,
			MaxBytes:      300,
		}); !errors.Is(err, ErrReplaySegmentConflict) {
			t.Fatalf("conflict upsert err=%v want ErrReplaySegmentConflict", err)
		}

		// 2. Byte quota enforced atomically in DB.
		if _, err := store.UpsertReplaySegment(ctx, ReplaySegmentInput{
			ProjectID:     project.ID,
			SessionID:     "sess-atomic",
			Seq:           1,
			StartedAt:     now.Add(2 * time.Second),
			LastSeenAt:    now.Add(3 * time.Second),
			EventCount:    2,
			SizeBytes:     200, // 120 + 200 > 300
			PayloadSHA256: strings.Repeat("c", 64),
			MarkersJSON:   `[]`,
			MaxSegments:   3,
			MaxBytes:      300,
		}); !errors.Is(err, ErrReplayQuotaExceeded) {
			t.Fatalf("byte quota err=%v want ErrReplayQuotaExceeded", err)
		}

		// 3. Concurrent inserts across segments must produce exact totals without regression.
		const workers = 6
		var wg sync.WaitGroup
		errCh := make(chan error, workers)
		for seq := range workers {
			wg.Add(1)
			go func(seq int) {
				defer wg.Done()
				_, upErr := store.UpsertReplaySegment(ctx, ReplaySegmentInput{
					ProjectID:     project.ID,
					SessionID:     "sess-concurrent",
					Seq:           seq,
					StartedAt:     now.Add(time.Duration(seq) * time.Second),
					LastSeenAt:    now.Add(time.Duration(seq+1) * time.Second),
					EventCount:    seq + 1,
					SizeBytes:     100,
					PayloadSHA256: strings.Repeat(string(rune('a'+seq)), 64),
					MarkersJSON:   `[]`,
					HasError:      seq == 3,
					MaxSegments:   20,
					MaxBytes:      10_000,
				})
				if upErr != nil {
					errCh <- upErr
				}
			}(seq)
		}
		wg.Wait()
		close(errCh)
		for upErr := range errCh {
			t.Fatalf("concurrent UpsertReplaySegment failed: %v", upErr)
		}
		sess, err := store.GetReplaySession(ctx, project.ID, "sess-concurrent")
		if err != nil {
			t.Fatal(err)
		}
		if sess.SegmentCount != workers || sess.TotalBytes != workers*100 || !sess.HasError {
			t.Fatalf("concurrent session summary=%+v want count=%d bytes=%d has_error=true",
				sess, workers, workers*100)
		}
		segs, err := store.ListReplaySegments(ctx, project.ID, "sess-concurrent", 50)
		if err != nil || len(segs) != workers {
			t.Fatalf("listed segments=%d err=%v want=%d", len(segs), err, workers)
		}
		for i, seg := range segs {
			if seg.Seq != i {
				t.Fatalf("segment order[%d].Seq=%d want=%d", i, seg.Seq, i)
			}
		}

		// 4. DeleteExpiredReplaySession removes both replay_sessions and replay_segments.
		removed, err := store.DeleteExpiredReplaySession(ctx, sess.ID, now.Add(time.Hour))
		if err != nil || !removed {
			t.Fatalf("DeleteExpiredReplaySession removed=%v err=%v", removed, err)
		}
		remaining, err := store.ListReplaySegments(ctx, project.ID, "sess-concurrent", 50)
		if err != nil || len(remaining) != 0 {
			t.Fatalf("replay_segments left after session delete: %d err=%v", len(remaining), err)
		}
	})
}
