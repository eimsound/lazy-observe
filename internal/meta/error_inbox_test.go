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
	changes, err := store.CompleteErrorEvent(ctx, claimed[0].ID, "receipt-worker", delta, acceptedAt.Add(time.Second))
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
