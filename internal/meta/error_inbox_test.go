package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
