package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

type loggedTransition struct {
	Status    string
	Cycle     int64
	Count     int // -1 when no window count was logged
	Threshold int
}

func pendingTransitions(t *testing.T, store *Store, projectID int) []loggedTransition {
	t.Helper()
	rows, err := store.PendingPlatformAlertTransitions(context.Background(), projectID, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]loggedTransition, 0, len(rows))
	for i, row := range rows {
		if i > 0 && rows[i-1].ID >= row.ID && rows[i-1].ProjectID == row.ProjectID {
			t.Fatalf("pending transitions out of id order: %#v", rows)
		}
		if row.Kind != platformAlertKindErrorRateSpike || row.ProjectSlug == "" || row.RecordedAt.IsZero() {
			t.Fatalf("transition row=%#v", row)
		}
		count := -1
		if row.WindowCount != nil {
			count = *row.WindowCount
		}
		out = append(out, loggedTransition{Status: row.Status, Cycle: row.Cycle, Count: count,
			Threshold: row.ThresholdPerMin})
	}
	return out
}

func commitAllTransitions(t *testing.T, store *Store, projectID int) {
	t.Helper()
	ctx := context.Background()
	for pass := 0; ; pass++ {
		claimed, err := store.ClaimPlatformAlertTransitions(
			ctx,
			projectID,
			fmt.Sprintf("test-drain-%d", pass),
			time.Minute,
			100,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) == 0 {
			return
		}
		for _, row := range claimed {
			if _, err := store.CommitPlatformAlertTransition(ctx, row.ID, row.ClaimToken, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func equalTransitions(got, want []loggedTransition) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Issue #386: every platform_alert_states transition is logged in the same
// transaction, including the intermediate resolved(C) of an intra-batch
// resolved(C) -> firing(C+1) sequence whose state row only keeps cycle C+1.
func TestPlatformAlertTransitionsLogEveryStateChange(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		window := 5 * time.Minute
		t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		project, err := store.CreateProject(ctx, "Transition Log", "transition-log")
		if err != nil {
			t.Fatal(err)
		}
		spike := func(start time.Time, n int) []time.Time {
			out := make([]time.Time, n)
			for i := range out {
				out[i] = start.Add(time.Duration(i) * time.Second)
			}
			return out
		}
		if _, err := store.RecordPlatformErrorRates(ctx, t0.Add(10*time.Second), window, []PlatformErrorRateBatch{
			{ProjectID: project.ID, ThresholdPerMin: 1, OccurredAt: spike(t0, 6)},
		}); err != nil {
			t.Fatal(err)
		}
		if got := pendingTransitions(t, store, project.ID); !equalTransitions(got, []loggedTransition{
			{Status: "firing", Cycle: 1, Count: 6, Threshold: 1},
		}) {
			t.Fatalf("after first spike=%#v", got)
		}

		late := t0.Add(6 * time.Minute)
		results, err := store.RecordPlatformErrorRates(ctx, late, window, []PlatformErrorRateBatch{
			{ProjectID: project.ID, ThresholdPerMin: 1, OccurredAt: spike(late.Add(-20*time.Second), 7)},
		})
		if err != nil || len(results) != 1 || len(results[0].Transitions) != 2 {
			t.Fatalf("intra-batch results=%#v err=%v", results, err)
		}
		if state := platformSpikeStateRow(t, store, project.ID); state == nil || state.Status != "firing" ||
			state.Cycle != 2 {
			t.Fatalf("final state=%#v want firing cycle 2", state)
		}
		if got := pendingTransitions(t, store, project.ID); !equalTransitions(got, []loggedTransition{
			{Status: "firing", Cycle: 1, Count: 6, Threshold: 1},
			{Status: "resolved", Cycle: 1, Count: 1, Threshold: 1},
			{Status: "firing", Cycle: 2, Count: 6, Threshold: 1},
		}) {
			t.Fatalf("after intra-batch resolve and re-fire=%#v", got)
		}
		commitAllTransitions(t, store, project.ID)

		recoveries, err := store.CheckPlatformErrorRateRecoveries(ctx, late.Add(6*time.Minute), window)
		if err != nil || len(recoveries) != 1 {
			t.Fatalf("recoveries=%#v err=%v", recoveries, err)
		}
		if got := pendingTransitions(t, store, project.ID); !equalTransitions(got, []loggedTransition{
			{Status: "resolved", Cycle: 2, Count: 0, Threshold: 0},
		}) {
			t.Fatalf("after recovery=%#v", got)
		}
		commitAllTransitions(t, store, project.ID)

		if _, err := store.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigUpdate{
			ErrorRateThresholdPerMinute: 3,
		}); err != nil {
			t.Fatal(err)
		}
		third := late.Add(20 * time.Minute)
		if _, err := store.RecordPlatformErrorRates(ctx, third.Add(time.Minute), window, []PlatformErrorRateBatch{
			{ProjectID: project.ID, ThresholdPerMin: 3, OccurredAt: spike(third, 16)},
		}); err != nil {
			t.Fatal(err)
		}
		if tr, err := store.ClearPlatformErrorRateState(ctx, project.ID, third.Add(2*time.Minute)); err != nil ||
			tr.Transition != "resolved" || tr.Cycle != 3 {
			t.Fatalf("clear=%#v err=%v", tr, err)
		}
		if got := pendingTransitions(t, store, project.ID); !equalTransitions(got, []loggedTransition{
			{Status: "firing", Cycle: 3, Count: 16, Threshold: 3},
			{Status: "resolved", Cycle: 3, Count: -1, Threshold: 3},
		}) {
			t.Fatalf("after reset=%#v", got)
		}
		// Clearing an already resolved state logs nothing.
		if tr, err := store.ClearPlatformErrorRateState(ctx, project.ID, third.Add(3*time.Minute)); err != nil ||
			tr.Transition != "" {
			t.Fatalf("second clear=%#v err=%v", tr, err)
		}
		if got := pendingTransitions(t, store, project.ID); len(got) != 2 {
			t.Fatalf("second clear logged a transition: %#v", got)
		}
		// Duplicate fenced commit returns ErrStalePlatformTransitionClaim, and project deletion drops remaining rows.
		claimed, err := store.ClaimPlatformAlertTransitions(ctx, project.ID, "log-test", time.Minute, 10)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claimed=%#v err=%v", claimed, err)
		}
		if _, err := store.CommitPlatformAlertTransition(ctx, claimed[0].ID, claimed[0].ClaimToken, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitPlatformAlertTransition(ctx, claimed[0].ID, claimed[0].ClaimToken, nil); !errors.Is(
			err,
			ErrStalePlatformTransitionClaim,
		) {
			t.Fatalf("duplicate commit err=%v want ErrStalePlatformTransitionClaim", err)
		}
		if err := store.DeleteProject(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := store.PendingPlatformAlertTransitions(ctx, 0, 0); err != nil || len(got) != 0 {
			t.Fatalf("transitions after project delete=%#v err=%v", got, err)
		}
	})
}

// Exclusive head claiming yields at most one transition (the oldest) per
// (project, kind) stream, keeps successors blocked until the head commits, and
// leaves ClaimToken populated only on leased rows.
func TestPlatformAlertTransitionsExclusiveHeadClaim(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		window := 5 * time.Minute
		t0 := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
		p1, err := store.CreateProject(ctx, "Claim P1", "claim-p1")
		if err != nil {
			t.Fatal(err)
		}
		p2, err := store.CreateProject(ctx, "Claim P2", "claim-p2")
		if err != nil {
			t.Fatal(err)
		}
		ch, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type:   "webhook",
			Name:   "claim-hook",
			Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		spike := func(start time.Time, n int) []time.Time {
			out := make([]time.Time, n)
			for i := range out {
				out[i] = start.Add(time.Duration(i) * time.Second)
			}
			return out
		}
		if _, err := store.RecordPlatformErrorRates(ctx, t0.Add(10*time.Second), window, []PlatformErrorRateBatch{
			{ProjectID: p1.ID, ThresholdPerMin: 1, OccurredAt: spike(t0, 6)},
			{ProjectID: p2.ID, ThresholdPerMin: 1, OccurredAt: spike(t0, 6)},
		}); err != nil {
			t.Fatal(err)
		}
		late := t0.Add(6 * time.Minute)
		if _, err := store.RecordPlatformErrorRates(ctx, late, window, []PlatformErrorRateBatch{
			{ProjectID: p1.ID, ThresholdPerMin: 1, OccurredAt: spike(late.Add(-20*time.Second), 7)},
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := store.ClaimPlatformAlertTransitions(ctx, 0, "", time.Minute, 10); err == nil {
			t.Fatal("expected error for empty claim token")
		}
		if _, err := store.ClaimPlatformAlertTransitions(ctx, 0, "tok", 0, 10); err == nil {
			t.Fatal("expected error for zero TTL")
		}
		if _, err := store.ClaimPlatformAlertTransitions(ctx, 0, "tok", -time.Second, 10); err == nil {
			t.Fatal("expected error for negative TTL")
		}

		claimedP1, err := store.ClaimPlatformAlertTransitions(
			ctx,
			p1.ID,
			"tok-a",
			30*time.Second,
			10,
		)
		if err != nil || len(claimedP1) != 1 {
			t.Fatalf("claimedP1=%#v err=%v", claimedP1, err)
		}
		if claimedP1[0].ProjectID != p1.ID || claimedP1[0].ProjectSlug != "claim-p1" ||
			claimedP1[0].Status != "firing" || claimedP1[0].Cycle != 1 || claimedP1[0].ClaimToken != "tok-a" {
			t.Fatalf("unexpected claimedP1[0]=%#v", claimedP1[0])
		}

		// Concurrent claim across all projects skips leased p1 (and keeps its successors
		// blocked), claiming only p2's head row.
		claimedAll, err := store.ClaimPlatformAlertTransitions(
			ctx,
			0,
			"tok-b",
			30*time.Second,
			10,
		)
		if err != nil || len(claimedAll) != 1 || claimedAll[0].ProjectID != p2.ID ||
			claimedAll[0].ClaimToken != "tok-b" {
			t.Fatalf("claimedAll=%#v err=%v want only p2", claimedAll, err)
		}
		if none, err := store.ClaimPlatformAlertTransitions(
			ctx,
			0,
			"tok-c",
			30*time.Second,
			10,
		); err != nil ||
			len(none) != 0 {
			t.Fatalf("expected empty claim while both heads leased, got=%#v err=%v", none, err)
		}

		// Invalid entry must fail validation before deleting the transition row.
		if _, err := store.CommitPlatformAlertTransition(ctx, claimedP1[0].ID, "tok-a", []NotificationOutboxEnqueue{{
			ProjectID: p1.ID,
			ChannelID: ch.ID,
			Payload:   json.RawMessage(`{"status":"firing"}`),
		}}); err == nil {
			t.Fatal("expected commit validation error for empty idempotency key")
		}

		inserted, err := store.CommitPlatformAlertTransition(
			ctx,
			claimedP1[0].ID,
			"tok-a",
			[]NotificationOutboxEnqueue{{
				IdempotencyKey: "claim-p1-firing-1",
				ProjectID:      p1.ID,
				ChannelID:      ch.ID,
				Payload:        json.RawMessage(`{"status":"firing","cycle":1}`),
				OrderingKey:    "platform-state:claim-p1:error_rate_spike",
			}},
		)
		if err != nil || inserted != 1 {
			t.Fatalf("commit p1 head inserted=%d err=%v", inserted, err)
		}

		// Committing p1's firing(1) releases resolved(1) as the new head.
		claimedR1, err := store.ClaimPlatformAlertTransitions(
			ctx,
			p1.ID,
			"tok-d",
			30*time.Second,
			10,
		)
		if err != nil || len(claimedR1) != 1 || claimedR1[0].Status != "resolved" || claimedR1[0].Cycle != 1 {
			t.Fatalf("claimedR1=%#v err=%v", claimedR1, err)
		}
		// Zero-entry commit (e.g. silenced/suppressed transition) still deletes the transition row.
		inserted, err = store.CommitPlatformAlertTransition(ctx, claimedR1[0].ID, "tok-d", nil)
		if err != nil || inserted != 0 {
			t.Fatalf("commit zero-entry resolved inserted=%d err=%v", inserted, err)
		}

		claimedF2, err := store.ClaimPlatformAlertTransitions(
			ctx,
			p1.ID,
			"tok-e",
			30*time.Second,
			10,
		)
		if err != nil || len(claimedF2) != 1 || claimedF2[0].Status != "firing" || claimedF2[0].Cycle != 2 {
			t.Fatalf("claimedF2=%#v err=%v", claimedF2, err)
		}
		if _, err := store.CommitPlatformAlertTransition(ctx, claimedF2[0].ID, "tok-e", nil); err != nil {
			t.Fatal(err)
		}
		if done, err := store.ClaimPlatformAlertTransitions(
			ctx,
			p1.ID,
			"tok-f",
			30*time.Second,
			10,
		); err != nil ||
			len(done) != 0 {
			t.Fatalf("expected p1 drained, got=%#v err=%v", done, err)
		}
	})
}

// Expired leases are taken over by a new claim token, and any delayed commit
// using the superseded token is fenced out with ErrStalePlatformTransitionClaim
// and writes zero outbox rows.
func TestPlatformAlertTransitionsExpiredLeaseTakeoverAndFencing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 13, 30, 0, 0, time.UTC)
		project, err := store.CreateProject(ctx, "Fence Project", "fence-project")
		if err != nil {
			t.Fatal(err)
		}
		ch, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type:   "webhook",
			Name:   "fence-hook",
			Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		events := make([]time.Time, 6)
		for i := range events {
			events[i] = now.Add(-time.Duration(i+1) * time.Second)
		}
		if _, err := store.RecordPlatformErrorRates(ctx, now, 5*time.Minute, []PlatformErrorRateBatch{
			{ProjectID: project.ID, ThresholdPerMin: 1, OccurredAt: events},
		}); err != nil {
			t.Fatal(err)
		}

		active, err := store.ClaimPlatformAlertTransitions(ctx, project.ID, "worker-active", 30*time.Second, 10)
		if err != nil || len(active) != 1 {
			t.Fatalf("active claim=%#v err=%v", active, err)
		}
		if beforeExpiry, err := store.ClaimPlatformAlertTransitions(
			ctx,
			project.ID,
			"worker-b",
			30*time.Second,
			10,
		); err != nil ||
			len(beforeExpiry) != 0 {
			t.Fatalf("expected lease active before expiry, got=%#v err=%v", beforeExpiry, err)
		}
		if err := store.ReleasePlatformAlertTransition(ctx, active[0].ID, "worker-active"); err != nil {
			t.Fatal(err)
		}

		const shortTTL = 25 * time.Millisecond
		first, err := store.ClaimPlatformAlertTransitions(ctx, project.ID, "worker-a", shortTTL, 10)
		if err != nil || len(first) != 1 {
			t.Fatalf("first claim=%#v err=%v", first, err)
		}
		var takeover []PlatformAlertTransition
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			takeover, err = store.ClaimPlatformAlertTransitions(ctx, project.ID, "worker-b", 30*time.Second, 10)
			if err != nil {
				t.Fatalf("takeover poll err=%v", err)
			}
			if len(takeover) > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if len(takeover) != 1 || takeover[0].ID != first[0].ID || takeover[0].ClaimToken != "worker-b" {
			t.Fatalf("takeover claim=%#v want row %d owned by worker-b", takeover, first[0].ID)
		}

		staleEntry := NotificationOutboxEnqueue{
			IdempotencyKey: "stale-worker-a-key",
			ProjectID:      project.ID,
			ChannelID:      ch.ID,
			Payload:        json.RawMessage(`{"from":"worker-a"}`),
			OrderingKey:    "platform-state:fence-project:error_rate_spike",
		}
		if inserted, err := store.CommitPlatformAlertTransition(
			ctx,
			first[0].ID,
			"worker-a",
			[]NotificationOutboxEnqueue{staleEntry},
		); !errors.Is(
			err,
			ErrStalePlatformTransitionClaim,
		) ||
			inserted != 0 {
			t.Fatalf("stale commit inserted=%d err=%v want ErrStalePlatformTransitionClaim", inserted, err)
		}
		if existing, err := store.NotificationOutboxExistingKeys(
			ctx,
			[]string{staleEntry.IdempotencyKey},
		); err != nil ||
			len(existing) != 0 {
			t.Fatalf("stale commit leaked outbox rows: existing=%#v err=%v", existing, err)
		}
		if pending, err := store.PendingPlatformAlertTransitions(ctx, project.ID, 0); err != nil || len(pending) != 1 ||
			pending[0].ClaimToken != "worker-b" {
			t.Fatalf("pending after stale commit=%#v err=%v", pending, err)
		}

		winnerEntry := NotificationOutboxEnqueue{
			IdempotencyKey: "winner-worker-b-key",
			ProjectID:      project.ID,
			ChannelID:      ch.ID,
			Payload:        json.RawMessage(`{"from":"worker-b"}`),
			OrderingKey:    "platform-state:fence-project:error_rate_spike",
		}
		inserted, err := store.CommitPlatformAlertTransition(
			ctx,
			takeover[0].ID,
			"worker-b",
			[]NotificationOutboxEnqueue{winnerEntry},
		)
		if err != nil || inserted != 1 {
			t.Fatalf("winner commit inserted=%d err=%v", inserted, err)
		}
		if existing, err := store.NotificationOutboxExistingKeys(
			ctx,
			[]string{staleEntry.IdempotencyKey, winnerEntry.IdempotencyKey},
		); err != nil ||
			!slices.Equal(sortedKeys(existing), []string{winnerEntry.IdempotencyKey}) {
			t.Fatalf("outbox keys=%#v err=%v", existing, err)
		}
		if _, err := store.CommitPlatformAlertTransition(ctx, takeover[0].ID, "worker-b", nil); !errors.Is(
			err,
			ErrStalePlatformTransitionClaim,
		) {
			t.Fatalf("duplicate commit err=%v want ErrStalePlatformTransitionClaim", err)
		}
	})
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ReleasePlatformAlertTransition clears a lease only when claimToken matches.
func TestPlatformAlertTransitionsReleaseRequiresMatchingToken(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 13, 45, 0, 0, time.UTC)
		project, err := store.CreateProject(ctx, "Release Project", "release-project")
		if err != nil {
			t.Fatal(err)
		}
		events := make([]time.Time, 6)
		for i := range events {
			events[i] = now.Add(-time.Duration(i+1) * time.Second)
		}
		if _, err := store.RecordPlatformErrorRates(ctx, now, 5*time.Minute, []PlatformErrorRateBatch{
			{ProjectID: project.ID, ThresholdPerMin: 1, OccurredAt: events},
		}); err != nil {
			t.Fatal(err)
		}

		claimed, err := store.ClaimPlatformAlertTransitions(ctx, project.ID, "owner-1", 30*time.Second, 10)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claimed=%#v err=%v", claimed, err)
		}
		if err := store.ReleasePlatformAlertTransition(ctx, claimed[0].ID, "wrong-owner"); err != nil {
			t.Fatal(err)
		}
		if blocked, err := store.ClaimPlatformAlertTransitions(
			ctx,
			project.ID,
			"owner-2",
			30*time.Second,
			10,
		); err != nil ||
			len(blocked) != 0 {
			t.Fatalf("wrong token released claim: %#v err=%v", blocked, err)
		}
		if err := store.ReleasePlatformAlertTransition(ctx, claimed[0].ID, "owner-1"); err != nil {
			t.Fatal(err)
		}
		reclaimed, err := store.ClaimPlatformAlertTransitions(
			ctx,
			project.ID,
			"owner-2",
			30*time.Second,
			10,
		)
		if err != nil || len(reclaimed) != 1 || reclaimed[0].ClaimToken != "owner-2" {
			t.Fatalf("reclaim after release=%#v err=%v", reclaimed, err)
		}
	})
}

// A project whose accounting is rolled back to its savepoint logs nothing,
// while the projects committed beside it keep their transitions.
func TestPlatformAlertTransitionsRollBackWithFailingProject(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
		ok, err := store.CreateProject(ctx, "Log OK", "log-ok")
		if err != nil {
			t.Fatal(err)
		}
		missing := ok.ID + 1000
		events := make([]time.Time, 6)
		for i := range events {
			events[i] = now.Add(-time.Duration(i+1) * time.Second)
		}
		if _, err := store.RecordPlatformErrorRates(ctx, now, 5*time.Minute, []PlatformErrorRateBatch{
			{ProjectID: ok.ID, ThresholdPerMin: 1, OccurredAt: events},
			{ProjectID: missing, ThresholdPerMin: 1, OccurredAt: events},
		}); err == nil {
			t.Fatal("expected the missing project to fail")
		}
		all, err := store.PendingPlatformAlertTransitions(ctx, 0, 0)
		if err != nil || len(all) != 1 || all[0].ProjectID != ok.ID || all[0].Status != "firing" {
			t.Fatalf("transitions=%#v err=%v want one firing for project %d", all, err, ok.ID)
		}
	})
}
