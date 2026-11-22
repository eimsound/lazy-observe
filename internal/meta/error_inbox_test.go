package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func TestErrorInboxCapacityIsAtomicAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Capacity", "capacity")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		capacity := errortrack.InboxCapacity{MaxItems: 3, MaxBytes: 1 << 20}
		errorsFound := make(chan error, 5)
		var wait sync.WaitGroup
		for index := 0; index < 5; index++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				payload := []byte(fmt.Sprintf(`{"event_id":"capacity-%d"}`, index))
				errorsFound <- store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
					ProjectID: project.ID, EventID: fmt.Sprintf("capacity-%d", index),
					CanonicalDigest: fmt.Sprintf("digest-%d", index), Payload: payload,
					PayloadBytes: int64(len(payload)), OccurredAt: now, AcceptedAt: now,
				}}, capacity)
			}(index)
		}
		wait.Wait()
		close(errorsFound)
		accepted, rejected := 0, 0
		for acceptErr := range errorsFound {
			switch {
			case acceptErr == nil:
				accepted++
			case errors.Is(acceptErr, errortrack.ErrCapacity):
				rejected++
			default:
				t.Fatalf("unexpected acceptance error: %v", acceptErr)
			}
		}
		diagnostics, err := store.ErrorInboxDiagnostics(ctx, 20)
		if err != nil || accepted != 3 || rejected != 2 || diagnostics.ActiveItems != 3 ||
			diagnostics.RejectionsTotal != 2 {
			t.Fatalf(
				"accepted=%d rejected=%d diagnostics=%#v err=%v",
				accepted,
				rejected,
				diagnostics,
				err,
			)
		}
		if err := store.DeleteProject(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
		diagnostics, err = store.ErrorInboxDiagnostics(ctx, 20)
		if err != nil || diagnostics.ActiveItems != 0 || diagnostics.ActiveBytes != 0 {
			t.Fatalf("post-delete diagnostics=%#v err=%v", diagnostics, err)
		}
	})
}

func TestErrorInboxCompletionReceiptIsAtomicAndDurable(t *testing.T) {
	forEachDialect(t, testErrorInboxCompletionReceiptIsAtomicAndDurable)
}

func testErrorInboxCompletionReceiptIsAtomicAndDurable(t *testing.T, store *Store) {
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Receipt", "receipt")
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt := time.Now().UTC().Truncate(time.Millisecond)
	occurredAt := acceptedAt.Add(-time.Minute)
	if err := store.UpsertSourceMapArtifact(
		ctx,
		project.ID,
		"receipt-release",
		"app.min.js",
		128,
		"receipt-map-digest",
		acceptedAt,
	); err != nil {
		t.Fatal(err)
	}
	entry := errortrack.InboxEntry{
		ProjectID: project.ID, EventID: "receipt-event", Release: "receipt-release",
		CanonicalDigest: "canonical-digest",
		Payload:         []byte(`{"event_id":"receipt-event"}`), PayloadBytes: 28,
		OccurredAt: occurredAt, AcceptedAt: acceptedAt,
	}
	entry.PayloadBytes = int64(len(entry.Payload))
	capacity := errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}
	if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{entry}, capacity); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimErrorEvents(ctx, "receipt-worker", 1, acceptedAt, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
	var snapshot map[string]string
	if err := json.Unmarshal(claimed[0].SourceMapSnapshot, &snapshot); err != nil ||
		snapshot["kind"] != "content-addressed-v1" ||
		snapshot["release"] != entry.Release ||
		snapshot["file:app.min.js"] != "receipt-map-digest" {
		t.Fatalf("source map snapshot=%s decoded=%#v err=%v", claimed[0].SourceMapSnapshot, snapshot, err)
	}
	delta := errortrack.IssueDelta{
		ProjectID: project.ID, Fingerprint: "receipt-fingerprint", Title: "ReceiptError: boom", Level: "error",
		Count: 1, FirstSeen: occurredAt, LastSeen: occurredAt, LastEventID: entry.EventID,
	}
	changes, err := store.CompleteErrorEvent(
		ctx,
		claimed[0].ID,
		"receipt-worker",
		claimed[0].ClaimEpoch,
		delta,
		acceptedAt.Add(time.Second),
	)
	if err != nil || len(changes) != 1 || changes[0].Kind != "new_issue" {
		t.Fatalf("changes=%#v err=%v", changes, err)
	}

	var status string
	var payload []byte
	var payloadBytes int64
	var digest, outcome string
	var storedOccurredAt, storedAcceptedAt, completedAt time.Time
	if err := store.db.QueryRowContext(ctx, bind(store.dialect, `SELECT status, payload, payload_bytes,
		canonical_digest, outcome, occurred_at, accepted_at, completed_at
		FROM telemetry_inbox WHERE project_id = ? AND event_id = ?`), project.ID, entry.EventID).
		Scan(
			&status,
			&payload,
			&payloadBytes,
			&digest,
			&outcome,
			&storedOccurredAt,
			&storedAcceptedAt,
			&completedAt,
		); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || payload != nil || payloadBytes != 0 || digest != entry.CanonicalDigest ||
		!storedOccurredAt.Equal(occurredAt) || !storedAcceptedAt.Equal(acceptedAt) || completedAt.IsZero() {
		t.Fatalf(
			"receipt status=%q payload=%q bytes=%d digest=%q occurred=%s accepted=%s completed=%s",
			status,
			payload,
			payloadBytes,
			digest,
			storedOccurredAt,
			storedAcceptedAt,
			completedAt,
		)
	}
	var recorded struct {
		Fingerprint   string                 `json:"fingerprint"`
		IssueID       int                    `json:"issue_id"`
		LifecycleKind string                 `json:"lifecycle_kind"`
		Lifecycle     errortrack.IssueChange `json:"lifecycle"`
	}
	if err := json.Unmarshal([]byte(outcome), &recorded); err != nil ||
		recorded.Fingerprint != delta.Fingerprint ||
		recorded.IssueID != changes[0].IssueID ||
		recorded.LifecycleKind != "new_issue" ||
		recorded.Lifecycle.Kind != "new_issue" {
		t.Fatalf("durable outcome=%q decoded=%#v err=%v", outcome, recorded, err)
	}

	if _, err := store.CompleteErrorEvent(
		ctx,
		claimed[0].ID,
		"receipt-worker",
		claimed[0].ClaimEpoch,
		delta,
		acceptedAt.Add(2*time.Second),
	); !errors.Is(err, errortrack.ErrClaimLost) {
		t.Fatalf("second completion error=%v", err)
	}
	issues, err := store.ListIssues(ctx, project.ID, IssueListOptions{})
	if err != nil || len(issues) != 1 || issues[0].Count != 1 {
		t.Fatalf("issues=%#v err=%v", issues, err)
	}
	deleted, err := store.DeleteCompletedErrorReceipts(ctx, completedAt.Add(-time.Second))
	if err != nil || deleted != 0 {
		t.Fatalf("early receipt cleanup deleted=%d err=%v", deleted, err)
	}
	deleted, err = store.DeleteCompletedErrorReceipts(ctx, completedAt.Add(time.Second))
	if err != nil || deleted != 1 {
		t.Fatalf("expired receipt cleanup deleted=%d err=%v", deleted, err)
	}
	entry.CanonicalDigest = "new-fact-after-receipt-expiry"
	if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{entry}, capacity); err != nil {
		t.Fatalf("reuse event ID after receipt expiry: %v", err)
	}
}

func TestErrorInboxFencingRejectsStaleOwnerAndEpochAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Fence", "fence")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		payload := []byte(`{"event_id":"fence-1"}`)
		capacity := errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
			ProjectID: project.ID, EventID: "fence-1", CanonicalDigest: "fence-digest",
			Payload: payload, PayloadBytes: int64(len(payload)), OccurredAt: now, AcceptedAt: now,
		}}, capacity); err != nil {
			t.Fatal(err)
		}
		first, err := store.ClaimErrorEvents(ctx, "owner-a", 1, now, time.Second)
		if err != nil || len(first) != 1 || first[0].ClaimEpoch != 1 {
			t.Fatalf("first claim=%#v err=%v", first, err)
		}
		// Lease expires; owner-a itself (or another owner) re-claims with epoch 2.
		second, err := store.ClaimErrorEvents(ctx, "owner-a", 1, now.Add(2*time.Second), time.Second)
		if err != nil || len(second) != 1 || second[0].ClaimEpoch != 2 {
			t.Fatalf("second claim=%#v err=%v", second, err)
		}
		delta := errortrack.IssueDelta{
			ProjectID: project.ID, Fingerprint: "fp-fence", Title: "FenceErr", Level: "error",
			Count: 1, FirstSeen: now, LastSeen: now, LastEventID: "fence-1",
		}
		if _, err := store.CompleteErrorEvent(
			ctx,
			first[0].ID,
			"owner-a",
			first[0].ClaimEpoch,
			delta,
			now.Add(3*time.Second),
		); !errors.Is(
			err,
			errortrack.ErrClaimLost,
		) {
			t.Fatalf("stale epoch CompleteErrorEvent err=%v want ErrClaimLost", err)
		}
		if err := store.RetryErrorEvent(
			ctx,
			first[0].ID,
			"owner-a",
			first[0].ClaimEpoch,
			now.Add(3*time.Second),
			now.Add(4*time.Second),
			"stale",
		); !errors.Is(
			err,
			errortrack.ErrClaimLost,
		) {
			t.Fatalf("stale epoch RetryErrorEvent err=%v want ErrClaimLost", err)
		}
		if err := store.ReleaseErrorEvent(
			ctx,
			first[0].ID,
			"owner-a",
			first[0].ClaimEpoch,
			now.Add(3*time.Second),
		); !errors.Is(
			err,
			errortrack.ErrClaimLost,
		) {
			t.Fatalf("stale epoch ReleaseErrorEvent err=%v want ErrClaimLost", err)
		}
		if err := store.TerminalErrorEvent(
			ctx,
			first[0].ID,
			"owner-a",
			first[0].ClaimEpoch,
			now.Add(3*time.Second),
			2,
			"stale",
		); !errors.Is(
			err,
			errortrack.ErrClaimLost,
		) {
			t.Fatalf("stale epoch TerminalErrorEvent err=%v want ErrClaimLost", err)
		}
		// Current epoch succeeds and persists lifecycle event atomically.
		changes, err := store.CompleteErrorEvent(
			ctx,
			second[0].ID,
			"owner-a",
			second[0].ClaimEpoch,
			delta,
			now.Add(3*time.Second),
		)
		if err != nil || len(changes) != 1 || changes[0].Kind != "new_issue" {
			t.Fatalf("valid epoch CompleteErrorEvent changes=%#v err=%v", changes, err)
		}
		lifecycles, err := store.ClaimIssueLifecycleEvents(ctx, "owner-a", 10, now.Add(3*time.Second), time.Minute)
		if err != nil || len(lifecycles) != 1 || lifecycles[0].Kind != "new_issue" ||
			lifecycles[0].IssueID != changes[0].IssueID {
			t.Fatalf("persisted lifecycle events=%#v err=%v", lifecycles, err)
		}
		if err := store.CompleteIssueLifecycleEvent(
			ctx,
			lifecycles[0].ID,
			"owner-a",
			lifecycles[0].ClaimEpoch,
			now.Add(4*time.Second),
		); err != nil {
			t.Fatalf("complete lifecycle event: %v", err)
		}
	})
}

func TestErrorInboxConcurrentRetryAndCompleteAvoidDeadlockAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "LockOrder", "lock-order")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		capacity := errortrack.InboxCapacity{MaxItems: 100, MaxBytes: 1 << 20}
		const n = 20
		entries := make([]errortrack.InboxEntry, 0, n)
		for i := 0; i < n; i++ {
			payload := []byte(fmt.Sprintf(`{"event_id":"lock-%d"}`, i))
			entries = append(entries, errortrack.InboxEntry{
				ProjectID: project.ID, EventID: fmt.Sprintf("lock-%d", i),
				CanonicalDigest: fmt.Sprintf("lock-digest-%d", i), Payload: payload,
				PayloadBytes: int64(len(payload)), OccurredAt: now, AcceptedAt: now,
			})
		}
		if err := store.AcceptErrorEvents(ctx, entries, capacity); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimErrorEvents(ctx, "lock-worker", n, now, time.Minute)
		if err != nil || len(claimed) != n {
			t.Fatalf("claimed=%d err=%v", len(claimed), err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for idx, item := range claimed {
			wg.Add(1)
			go func(idx int, item errortrack.InboxItem) {
				defer wg.Done()
				if idx%2 == 0 {
					errs <- store.RetryErrorEvent(ctx, item.ID, "lock-worker", item.ClaimEpoch, now.Add(time.Second), now.Add(2*time.Second), "retry")
					return
				}
				_, err := store.CompleteErrorEvent(ctx, item.ID, "lock-worker", item.ClaimEpoch, errortrack.IssueDelta{
					ProjectID: project.ID, Fingerprint: fmt.Sprintf("fp-%d", idx), Title: "LockErr", Level: "error",
					Count: 1, FirstSeen: now, LastSeen: now, LastEventID: item.EventID,
				}, now.Add(time.Second))
				errs <- err
			}(idx, item)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent retry/complete failed (possible AB-BA deadlock): %v", err)
			}
		}
		summary, err := store.ErrorInboxSummary(ctx)
		if err != nil || summary.ActiveItems != n/2 || summary.RetriesTotal != n/2 {
			t.Fatalf("summary=%#v err=%v", summary, err)
		}
	})
}

func TestAcceptErrorEventsIdempotencyAndConflictOutsideStateLockAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "IdemLock", "idem-lock")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		payload := []byte(`{"event_id":"idem-1"}`)
		capacity := errortrack.InboxCapacity{MaxItems: 1, MaxBytes: 1 << 20}
		entry := errortrack.InboxEntry{
			ProjectID: project.ID, EventID: "idem-1", CanonicalDigest: "digest-a",
			Payload: payload, PayloadBytes: int64(len(payload)), OccurredAt: now, AcceptedAt: now,
		}
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{entry}, capacity); err != nil {
			t.Fatalf("initial accept: %v", err)
		}
		// Duplicate retry at full capacity must return nil without bumping rejections_total.
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{entry}, capacity); err != nil {
			t.Fatalf("duplicate retry at full capacity failed: %v", err)
		}
		// Conflicting digest at full capacity must return ErrConflict without bumping rejections_total.
		conflict := entry
		conflict.CanonicalDigest = "digest-b"
		if err := store.AcceptErrorEvents(
			ctx,
			[]errortrack.InboxEntry{conflict},
			capacity,
		); !errors.Is(err, errortrack.ErrConflict) {
			t.Fatalf("conflicting digest err=%v want ErrConflict", err)
		}
		summary, err := store.ErrorInboxSummary(ctx)
		if err != nil || summary.ActiveItems != 1 || summary.RejectionsTotal != 0 {
			t.Fatalf("summary after idempotent/conflict checks=%#v err=%v", summary, err)
		}

		// Concurrent identical & conflicting writes for a brand-new event ID must
		// remain atomic, never produce unique-constraint errors, and count active_items once.
		capacity.MaxItems = 10
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := range 8 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				digest := "same-digest"
				if idx == 7 {
					digest = "conflicting-digest"
				}
				p := []byte(`{"event_id":"race-1"}`)
				errs <- store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
					ProjectID: project.ID, EventID: "race-1", CanonicalDigest: digest,
					Payload: p, PayloadBytes: int64(len(p)), OccurredAt: now, AcceptedAt: now,
				}}, capacity)
			}(i)
		}
		wg.Wait()
		close(errs)
		conflicts := 0
		for runErr := range errs {
			switch {
			case runErr == nil:
			case errors.Is(runErr, errortrack.ErrConflict):
				conflicts++
			default:
				t.Fatalf("unexpected concurrent accept error: %v", runErr)
			}
		}
		if conflicts == 0 {
			t.Fatalf("expected at least one conflict error from conflicting concurrent writer")
		}
		summary, err = store.ErrorInboxSummary(ctx)
		if err != nil || summary.ActiveItems != 2 {
			t.Fatalf("summary after concurrent race=%#v want active_items=2 err=%v", summary, err)
		}
	})
}

func TestIssueLifecycleEventsPersistNewIssueAndRegressionAtomicallyAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "LifecyclePG", "lifecycle-pg")
		if err != nil {
			t.Fatal(err)
		}
		base := time.Now().UTC().Truncate(time.Millisecond)
		capacity := errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}
		acceptAndComplete := func(eventID string, at time.Time) []errortrack.IssueChange {
			t.Helper()
			p := []byte(fmt.Sprintf(`{"event_id":%q}`, eventID))
			if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
				ProjectID: project.ID, EventID: eventID, CanonicalDigest: "digest-" + eventID,
				Payload: p, PayloadBytes: int64(len(p)), OccurredAt: at, AcceptedAt: at,
			}}, capacity); err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimErrorEvents(ctx, "worker-life", 1, at, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim %s=%#v err=%v", eventID, claimed, err)
			}
			changes, err := store.CompleteErrorEvent(
				ctx,
				claimed[0].ID,
				"worker-life",
				claimed[0].ClaimEpoch,
				errortrack.IssueDelta{
					ProjectID: project.ID, Fingerprint: "fp-life", Title: "LifeErr", Level: "error",
					Count: 1, FirstSeen: at, LastSeen: at, LastEventID: eventID,
				},
				at.Add(time.Second),
			)
			if err != nil {
				t.Fatalf("complete %s: %v", eventID, err)
			}
			return changes
		}

		// First event creates new_issue and persists one lifecycle event.
		firstChanges := acceptAndComplete("life-1", base)
		if len(firstChanges) != 1 || firstChanges[0].Kind != "new_issue" {
			t.Fatalf("firstChanges=%#v", firstChanges)
		}
		// Second event on open issue emits no lifecycle event.
		if secondChanges := acceptAndComplete("life-2", base.Add(2*time.Second)); len(secondChanges) != 0 {
			t.Fatalf("secondChanges=%#v want empty", secondChanges)
		}
		// Resolve, then emit a later event -> persists regression lifecycle event.
		if _, err := store.SetIssueStatus(
			ctx, project.ID, firstChanges[0].IssueID, "resolved", base.Add(5*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		regChanges := acceptAndComplete("life-3", base.Add(10*time.Second))
		if len(regChanges) != 1 || regChanges[0].Kind != "regression" {
			t.Fatalf("regChanges=%#v", regChanges)
		}

		events, err := store.ClaimIssueLifecycleEvents(ctx, "dispatcher-1", 10, base.Add(15*time.Second), time.Minute)
		if err != nil || len(events) != 2 {
			t.Fatalf("claimed lifecycle events=%#v err=%v", events, err)
		}
		if events[0].Kind != "new_issue" || events[1].Kind != "regression" ||
			events[0].IssueID != firstChanges[0].IssueID || events[1].IssueID != firstChanges[0].IssueID {
			t.Fatalf("unexpected lifecycle sequence=%#v", events)
		}
		// Verify fencing & retry/complete semantics on lifecycle events.
		if err := store.RetryIssueLifecycleEvent(
			ctx, events[0].ID, "dispatcher-1", events[0].ClaimEpoch,
			base.Add(16*time.Second), base.Add(17*time.Second), "transient",
		); err != nil {
			t.Fatalf("retry lifecycle event: %v", err)
		}
		if err := store.CompleteIssueLifecycleEvent(
			ctx, events[0].ID, "dispatcher-1", events[0].ClaimEpoch, base.Add(18*time.Second),
		); !errors.Is(err, errortrack.ErrClaimLost) {
			t.Fatalf("stale complete after retry err=%v want ErrClaimLost", err)
		}
		reclaimed, err := store.ClaimIssueLifecycleEvents(
			ctx,
			"dispatcher-2",
			10,
			base.Add(18*time.Second),
			time.Minute,
		)
		if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != events[0].ID || reclaimed[0].ClaimEpoch != 2 {
			t.Fatalf("reclaimed=%#v err=%v", reclaimed, err)
		}
		if err := store.CompleteIssueLifecycleEvent(
			ctx, reclaimed[0].ID, "dispatcher-2", reclaimed[0].ClaimEpoch, base.Add(19*time.Second),
		); err != nil {
			t.Fatalf("complete reclaimed lifecycle event: %v", err)
		}
	})
}

func TestCompleteErrorEventsBatchMergesDeltasAndReceiptsAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		p1, err := store.CreateProject(ctx, "BatchP1", "batch-p1")
		if err != nil {
			t.Fatal(err)
		}
		p2, err := store.CreateProject(ctx, "BatchP2", "batch-p2")
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now().UTC().Truncate(time.Millisecond)
		capacity := errortrack.InboxCapacity{MaxItems: 100, MaxBytes: 1 << 20}
		mkEntry := func(projectID int, eventID string, at time.Time) errortrack.InboxEntry {
			payload := []byte(fmt.Sprintf(`{"event_id":%q}`, eventID))
			return errortrack.InboxEntry{
				ProjectID: projectID, EventID: eventID, CanonicalDigest: "digest-" + eventID,
				Payload: payload, PayloadBytes: int64(len(payload)), OccurredAt: at, AcceptedAt: at,
			}
		}
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{
			mkEntry(p1.ID, "b-1", t0),
			mkEntry(p1.ID, "b-2", t0.Add(5*time.Second)),
			mkEntry(p1.ID, "b-3", t0.Add(time.Second)),
			mkEntry(p2.ID, "b-4", t0.Add(2*time.Second)),
		}, capacity); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimErrorEvents(ctx, "batch-worker", 10, t0.Add(6*time.Second), time.Minute)
		if err != nil || len(claimed) != 4 {
			t.Fatalf("claimed=%d err=%v", len(claimed), err)
		}

		fpMap := map[string]struct {
			fp   string
			bits uint64
		}{
			"b-1": {fp: "fp-shared", bits: 1 << 1},
			"b-2": {fp: "fp-shared", bits: 1 << 2},
			"b-3": {fp: "fp-other", bits: 1 << 1},
			"b-4": {fp: "fp-shared", bits: 1 << 3},
		}
		appliedAt := t0.Add(10 * time.Second)
		completions := make([]errortrack.ErrorCompletion, len(claimed))
		for idx, item := range claimed {
			spec := fpMap[item.EventID]
			completions[idx] = errortrack.ErrorCompletion{
				ID: item.ID, EventID: item.EventID, ClaimEpoch: item.ClaimEpoch, Attempts: item.Attempts,
				ProcessedAt: appliedAt,
				Delta: errortrack.IssueDelta{
					ProjectID: item.ProjectID, Fingerprint: spec.fp, Title: "Err-" + item.EventID,
					Level: "error", Count: 1, SessionBits: spec.bits,
					FirstSeen: item.OccurredAt, LastSeen: item.OccurredAt, LastEventID: item.EventID,
				},
			}
		}
		res, err := store.CompleteErrorEvents(ctx, "batch-worker", completions)
		if err != nil {
			t.Fatalf("CompleteErrorEvents: %v", err)
		}
		if len(res.CompletedIDs) != 4 || len(res.LostIDs) != 0 || len(res.Changes) != 3 {
			t.Fatalf("result=%#v", res)
		}
		summary, err := store.ErrorInboxSummary(ctx)
		if err != nil || summary.ActiveItems != 0 || summary.ActiveBytes != 0 {
			t.Fatalf("summary=%#v err=%v", summary, err)
		}

		p1Issues, err := store.ListIssues(ctx, p1.ID, IssueListOptions{})
		if err != nil || len(p1Issues) != 2 {
			t.Fatalf("p1Issues=%#v err=%v", p1Issues, err)
		}
		var shared IssueView
		for _, iss := range p1Issues {
			if iss.Fingerprint == "fp-shared" {
				shared = iss
			}
		}
		if shared.Count != 2 || shared.UserCount != 2 || !shared.FirstSeen.Equal(t0) ||
			!shared.LastSeen.Equal(t0.Add(5*time.Second)) || shared.LastEventID != "b-2" {
			t.Fatalf("merged shared issue=%#v", shared)
		}

		for _, item := range claimed {
			issueID, ok, err := store.IssueIDForErrorEvent(ctx, item.ProjectID, item.EventID)
			if err != nil || !ok || issueID <= 0 {
				t.Fatalf("IssueIDForErrorEvent(%d, %q) = (%d, %v, %v)", item.ProjectID, item.EventID, issueID, ok, err)
			}
			if item.ProjectID == p1.ID && fpMap[item.EventID].fp == "fp-shared" && issueID != shared.ID {
				t.Fatalf("receipt issueID=%d want %d for %q", issueID, shared.ID, item.EventID)
			}
		}

		// Merged batch regression cutoff: resolve at t0+20s, then complete one
		// event accepted before resolve and one accepted after resolve in one batch.
		resolvedAt := t0.Add(20 * time.Second)
		if _, err := store.SetIssueStatus(ctx, p1.ID, shared.ID, "resolved", resolvedAt); err != nil {
			t.Fatal(err)
		}
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{
			mkEntry(p1.ID, "b-before-resolve", resolvedAt.Add(-time.Second)),
			mkEntry(p1.ID, "b-after-resolve", resolvedAt.Add(time.Second)),
		}, capacity); err != nil {
			t.Fatal(err)
		}
		regClaimed, err := store.ClaimErrorEvents(ctx, "batch-worker", 10, resolvedAt.Add(2*time.Second), time.Minute)
		if err != nil || len(regClaimed) != 2 {
			t.Fatalf("regClaimed=%d err=%v", len(regClaimed), err)
		}
		regCompletions := make([]errortrack.ErrorCompletion, len(regClaimed))
		for idx, item := range regClaimed {
			regCompletions[idx] = errortrack.ErrorCompletion{
				ID: item.ID, EventID: item.EventID, ClaimEpoch: item.ClaimEpoch, Attempts: item.Attempts,
				ProcessedAt: resolvedAt.Add(3 * time.Second),
				Delta: errortrack.IssueDelta{
					ProjectID: p1.ID, Fingerprint: "fp-shared", Title: "Err-" + item.EventID,
					Level: "error", Count: 1, FirstSeen: item.OccurredAt, LastSeen: item.OccurredAt,
					LastEventID: item.EventID,
				},
			}
		}
		regRes, err := store.CompleteErrorEvents(ctx, "batch-worker", regCompletions)
		if err != nil || len(regRes.Changes) != 1 || regRes.Changes[0].Kind != "regression" {
			t.Fatalf("regRes=%#v err=%v", regRes, err)
		}
		reopened, err := store.GetIssue(ctx, p1.ID, shared.ID)
		if err != nil || reopened.Status != "open" || reopened.Count != 4 {
			t.Fatalf("reopened=%#v err=%v", reopened, err)
		}
	})
}

func TestCompleteErrorEventsPartialClaimLostLogsWarnAndCommitsValidAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		ctx := context.Background()
		project, err := store.CreateProject(ctx, "PartialFence", "partial-fence")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		capacity := errortrack.InboxCapacity{MaxItems: 20, MaxBytes: 1 << 20}
		entries := make([]errortrack.InboxEntry, 3)
		for i := range entries {
			id := fmt.Sprintf("pf-%d", i)
			p := []byte(fmt.Sprintf(`{"event_id":%q}`, id))
			entries[i] = errortrack.InboxEntry{
				ProjectID: project.ID, EventID: id, CanonicalDigest: "digest-" + id,
				Payload: p, PayloadBytes: int64(len(p)), OccurredAt: now, AcceptedAt: now,
			}
		}
		if err := store.AcceptErrorEvents(ctx, entries, capacity); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimErrorEvents(ctx, "worker-a", 10, now, time.Minute)
		if err != nil || len(claimed) != 3 {
			t.Fatalf("claimed=%d err=%v", len(claimed), err)
		}
		// Steal only the first claimed item by worker-b.
		stolen, err := store.ClaimErrorEvents(ctx, "worker-b", 1, now.Add(2*time.Minute), time.Minute)
		if err != nil || len(stolen) != 1 || stolen[0].ID != claimed[0].ID {
			t.Fatalf("stolen=%#v err=%v", stolen, err)
		}

		completions := make([]errortrack.ErrorCompletion, len(claimed))
		for idx, item := range claimed {
			completions[idx] = errortrack.ErrorCompletion{
				ID: item.ID, EventID: item.EventID, ClaimEpoch: item.ClaimEpoch, Attempts: item.Attempts,
				ProcessedAt: now.Add(3 * time.Minute),
				Delta: errortrack.IssueDelta{
					ProjectID: project.ID, Fingerprint: fmt.Sprintf("fp-%d", idx), Title: item.EventID,
					Level: "error", Count: 1, FirstSeen: now, LastSeen: now, LastEventID: item.EventID,
				},
			}
		}
		res, err := store.CompleteErrorEvents(ctx, "worker-a", completions)
		if err != nil {
			t.Fatalf("CompleteErrorEvents: %v", err)
		}
		if len(res.CompletedIDs) != 2 || len(res.LostIDs) != 1 || res.LostIDs[0] != stolen[0].ID ||
			len(res.Changes) != 2 {
			t.Fatalf("result=%#v want 2 completed, 1 lost (%d)", res, stolen[0].ID)
		}
		logged := logBuf.String()
		if !strings.Contains(logged, "losing claim lease") ||
			!strings.Contains(logged, "event_id="+stolen[0].EventID) ||
			!strings.Contains(logged, "owner=worker-a") ||
			!strings.Contains(logged, "claim_epoch=1") {
			t.Fatalf("expected slog.Warn with event_id, owner, and claim_epoch, got: %s", logged)
		}
		summary, err := store.ErrorInboxSummary(ctx)
		if err != nil || summary.ActiveItems != 1 {
			t.Fatalf("summary=%#v err=%v", summary, err)
		}

		// worker-b can still complete the stolen item cleanly.
		bRes, err := store.CompleteErrorEvents(ctx, "worker-b", []errortrack.ErrorCompletion{{
			ID: stolen[0].ID, EventID: stolen[0].EventID, ClaimEpoch: stolen[0].ClaimEpoch,
			Attempts: stolen[0].Attempts, ProcessedAt: now.Add(4 * time.Minute),
			Delta: errortrack.IssueDelta{
				ProjectID: project.ID, Fingerprint: "fp-0", Title: stolen[0].EventID,
				Level: "error", Count: 1, FirstSeen: now, LastSeen: now, LastEventID: stolen[0].EventID,
			},
		}})
		if err != nil || len(bRes.CompletedIDs) != 1 {
			t.Fatalf("worker-b complete=%#v err=%v", bRes, err)
		}
	})
}

func TestCompleteErrorEventsConcurrentLockOrderAvoidsDeadlockAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "BatchLockOrder", "batch-lock-order")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		capacity := errortrack.InboxCapacity{MaxItems: 200, MaxBytes: 4 << 20}
		const n = 24
		entries := make([]errortrack.InboxEntry, n)
		for i := range entries {
			id := fmt.Sprintf("bl-%d", i)
			p := []byte(fmt.Sprintf(`{"event_id":%q}`, id))
			entries[i] = errortrack.InboxEntry{
				ProjectID: project.ID, EventID: id, CanonicalDigest: "digest-" + id,
				Payload: p, PayloadBytes: int64(len(p)), OccurredAt: now, AcceptedAt: now,
			}
		}
		if err := store.AcceptErrorEvents(ctx, entries, capacity); err != nil {
			t.Fatal(err)
		}
		claimA, err := store.ClaimErrorEvents(ctx, "w-a", 8, now, time.Minute)
		if err != nil || len(claimA) != 8 {
			t.Fatalf("claimA=%d err=%v", len(claimA), err)
		}
		claimB, err := store.ClaimErrorEvents(ctx, "w-b", 8, now, time.Minute)
		if err != nil || len(claimB) != 8 {
			t.Fatalf("claimB=%d err=%v", len(claimB), err)
		}
		claimC, err := store.ClaimErrorEvents(ctx, "w-c", 8, now, time.Minute)
		if err != nil || len(claimC) != 8 {
			t.Fatalf("claimC=%d err=%v", len(claimC), err)
		}

		toCompletions := func(items []errortrack.InboxItem, reverse bool) []errortrack.ErrorCompletion {
			out := make([]errortrack.ErrorCompletion, len(items))
			for i, item := range items {
				idx := i
				if reverse {
					idx = len(items) - 1 - i
				}
				out[idx] = errortrack.ErrorCompletion{
					ID: item.ID, EventID: item.EventID, ClaimEpoch: item.ClaimEpoch, Attempts: item.Attempts,
					ProcessedAt: now.Add(time.Second),
					Delta: errortrack.IssueDelta{
						ProjectID: project.ID, Fingerprint: fmt.Sprintf("fp-%d", i%3), Title: "LockErr",
						Level: "error", Count: 1, FirstSeen: now, LastSeen: now, LastEventID: item.EventID,
					},
				}
			}
			return out
		}

		var wg sync.WaitGroup
		errs := make(chan error, 10)
		wg.Add(4)
		go func() {
			defer wg.Done()
			_, err := store.CompleteErrorEvents(ctx, "w-a", toCompletions(claimA, false))
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := store.CompleteErrorEvents(ctx, "w-b", toCompletions(claimB, true))
			errs <- err
		}()
		go func() {
			defer wg.Done()
			for _, item := range claimC {
				if err := store.RetryErrorEvent(
					ctx,
					item.ID,
					"w-c",
					item.ClaimEpoch,
					now.Add(time.Second),
					now.Add(2*time.Second),
					"retry",
				); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
		go func() {
			defer wg.Done()
			p := []byte(`{"event_id":"bl-extra"}`)
			errs <- store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
				ProjectID: project.ID, EventID: "bl-extra", CanonicalDigest: "digest-bl-extra",
				Payload: p, PayloadBytes: int64(len(p)), OccurredAt: now, AcceptedAt: now,
			}}, capacity)
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent batch lock-order operation failed: %v", err)
			}
		}
		summary, err := store.ErrorInboxSummary(ctx)
		if err != nil || summary.ActiveItems != 9 || summary.RetriesTotal != 8 {
			t.Fatalf("summary=%#v err=%v", summary, err)
		}
	})
}

func TestClaimErrorEventsSQLiteDeduplicatesProjectSlugsAcrossDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		p1, err := store.CreateProject(ctx, "Alpha", "alpha-slug")
		if err != nil {
			t.Fatal(err)
		}
		p2, err := store.CreateProject(ctx, "Beta", "beta-slug")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		entries := make([]errortrack.InboxEntry, 16)
		for i := range entries {
			proj := p1
			if i%2 == 1 {
				proj = p2
			}
			id := fmt.Sprintf("slug-%d", i)
			p := []byte(fmt.Sprintf(`{"event_id":%q}`, id))
			entries[i] = errortrack.InboxEntry{
				ProjectID: proj.ID, EventID: id, CanonicalDigest: "digest-" + id,
				Payload: p, PayloadBytes: int64(len(p)), OccurredAt: now, AcceptedAt: now,
			}
		}
		if err := store.AcceptErrorEvents(
			ctx,
			entries,
			errortrack.InboxCapacity{MaxItems: 50, MaxBytes: 1 << 20},
		); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimErrorEvents(ctx, "slug-worker", 16, now, time.Minute)
		if err != nil || len(claimed) != 16 {
			t.Fatalf("claimed=%d err=%v", len(claimed), err)
		}
		for _, item := range claimed {
			want := "alpha-slug"
			if item.ProjectID == p2.ID {
				want = "beta-slug"
			}
			if item.ProjectSlug != want {
				t.Fatalf("item %q projectSlug=%q want %q", item.EventID, item.ProjectSlug, want)
			}
		}
	})
}
