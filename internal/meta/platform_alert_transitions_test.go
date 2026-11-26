package meta

import (
	"context"
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

func ackAllTransitions(t *testing.T, store *Store, projectID int) {
	t.Helper()
	rows, err := store.PendingPlatformAlertTransitions(context.Background(), projectID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := store.AckPlatformAlertTransition(context.Background(), row.ID); err != nil {
			t.Fatal(err)
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
		ackAllTransitions(t, store, project.ID)

		recoveries, err := store.CheckPlatformErrorRateRecoveries(ctx, late.Add(6*time.Minute), window)
		if err != nil || len(recoveries) != 1 {
			t.Fatalf("recoveries=%#v err=%v", recoveries, err)
		}
		if got := pendingTransitions(t, store, project.ID); !equalTransitions(got, []loggedTransition{
			{Status: "resolved", Cycle: 2, Count: 0, Threshold: 0},
		}) {
			t.Fatalf("after recovery=%#v", got)
		}
		ackAllTransitions(t, store, project.ID)

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
		// Acknowledging twice is harmless, and project deletion drops the log.
		rows, err := store.PendingPlatformAlertTransitions(ctx, project.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AckPlatformAlertTransition(ctx, rows[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := store.AckPlatformAlertTransition(ctx, rows[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteProject(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := store.PendingPlatformAlertTransitions(ctx, 0, 0); err != nil || len(got) != 0 {
			t.Fatalf("transitions after project delete=%#v err=%v", got, err)
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
