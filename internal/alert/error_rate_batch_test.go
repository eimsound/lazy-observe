package alert

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func spikeOccurrences(projectID int, start time.Time, n int) []errortrack.ErrorOccurrence {
	out := make([]errortrack.ErrorOccurrence, n)
	for i := range out {
		out[i] = errortrack.ErrorOccurrence{ProjectID: projectID, OccurredAt: start.Add(time.Duration(i) * time.Second)}
	}
	return out
}

// A whole inbox batch recorded through RecordErrorEvents must enqueue the
// same outbox rows, in the same order and with the same annotations, as the
// equivalent sequence of per-event RecordErrorEvent calls (issue #375).
func TestRecordErrorEventsBatchMatchesPerEventOutbox(t *testing.T) {
	ctx := context.Background()
	seq := newSpikeHarness(t, "spike-seq")
	batch := newSpikeHarness(t, "spike-batch")
	seqSvc := seq.service("seq")
	batchSvc := batch.service("batch")

	apply := func(at time.Time, start time.Time, n int) {
		t.Helper()
		seq.set(at)
		for _, occ := range spikeOccurrences(seq.project.ID, start, n) {
			if err := seqSvc.RecordErrorEvent(ctx, occ.ProjectID, occ.OccurredAt); err != nil {
				t.Fatal(err)
			}
		}
		batch.set(at)
		if err := batchSvc.RecordErrorEvents(ctx, spikeOccurrences(batch.project.ID, start, n)); err != nil {
			t.Fatal(err)
		}
	}

	// Eight errors in one batch cross the 5/5min limit once: one firing row.
	apply(seq.base.Add(10*time.Second), seq.base, 8)
	batch.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	batch.waitDelivered("firing", "1")
	seq.waitDelivered("firing", "1")

	// The window slid past the spike before the recovery loop ran: the first
	// event of the next batch resolves cycle 1 and the sixth re-fires cycle 2.
	late := seq.base.Add(6 * time.Minute)
	apply(late, late.Add(-10*time.Second), 7)
	rows := batch.expectRows(
		PlatformStateRef{Cycle: 1, Status: "firing"},
		PlatformStateRef{Cycle: 1, Status: "resolved"},
		PlatformStateRef{Cycle: 2, Status: "firing"},
	)
	seqRows := seq.expectRows(
		PlatformStateRef{Cycle: 1, Status: "firing"},
		PlatformStateRef{Cycle: 1, Status: "resolved"},
		PlatformStateRef{Cycle: 2, Status: "firing"},
	)
	for i := range rows {
		got, want := rows[i].Notification.Alerts[0], seqRows[i].Notification.Alerts[0]
		if got.Annotations["summary"] != want.Annotations["summary"] || !got.StartsAt.Equal(want.StartsAt) ||
			!got.EndsAt.Equal(want.EndsAt) || rows[i].Priority != seqRows[i].Priority {
			t.Fatalf("row %d batch=%#v sequential=%#v", i, rows[i], seqRows[i])
		}
	}
	if got := rows[1].Notification.Alerts[0].Annotations["summary"]; got != "Error rate spike recovered: 0.2 events/min" {
		t.Fatalf("resolved summary=%q", got)
	}
	batch.waitDelivered("resolved", "1")
	batch.waitDelivered("firing", "2")
}

// A project whose config cannot be loaded is reported in the joined error
// without blocking accounting for the rest of the batch.
func TestRecordErrorEventsReportsFailedProjectAndRecordsOthers(t *testing.T) {
	ctx := context.Background()
	h := newSpikeHarness(t, "spike-partial")
	svc := h.service("partial")
	missing := h.project.ID + 1000
	h.set(h.base.Add(10 * time.Second))
	occurrences := append(spikeOccurrences(missing, h.base, 6), spikeOccurrences(h.project.ID, h.base, 6)...)
	err := svc.RecordErrorEvents(ctx, occurrences)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("project %d", missing)) {
		t.Fatalf("err=%v want failure naming project %d", err, missing)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	h.waitDelivered("firing", "1")

	if err := svc.RecordErrorEvents(ctx, nil); err != nil {
		t.Fatalf("empty batch err=%v", err)
	}
}
