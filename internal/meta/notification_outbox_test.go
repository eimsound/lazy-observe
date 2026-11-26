package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/notificationchannel"
)

func TestNotificationOutboxLifecycle(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, rule, channel := createOutboxFixture(t, store, "lifecycle")
		payload := json.RawMessage(`{"project":"alpha","status":"firing"}`)
		firstID, inserted, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "delivery-firing", ProjectID: project.ID, RuleID: &rule.ID,
			ChannelID: channel.ID, Payload: payload,
		})
		if err != nil || !inserted || firstID == 0 {
			t.Fatalf("first enqueue id=%d inserted=%v err=%v", firstID, inserted, err)
		}
		duplicateID, inserted, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "delivery-firing", ProjectID: project.ID, RuleID: &rule.ID,
			ChannelID: channel.ID, Payload: payload,
		})
		if err != nil || inserted || duplicateID != firstID {
			t.Fatalf("duplicate enqueue id=%d inserted=%v err=%v", duplicateID, inserted, err)
		}
		resolvedID, inserted, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "delivery-resolved", ProjectID: project.ID, RuleID: &rule.ID,
			ChannelID: channel.ID, Payload: json.RawMessage(`{"status":"resolved"}`), Priority: 10,
		})
		if err != nil || !inserted {
			t.Fatal(err)
		}

		items, err := store.ClaimNotifications(ctx, "worker-a", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].ID != resolvedID || items[0].Attempts != 1 ||
			items[0].ClaimOwner != "worker-a" {
			t.Fatalf("priority claim=%#v err=%v", items, err)
		}
		if err := store.CompleteNotification(
			ctx,
			resolvedID,
			"worker-b",
		); !errors.Is(
			err,
			ErrNotificationOutboxClaimLost,
		) {
			t.Fatalf("foreign completion error=%v", err)
		}
		if err := store.CompleteNotification(ctx, resolvedID, "worker-a"); err != nil {
			t.Fatal(err)
		}

		items, err = store.ClaimNotifications(ctx, "worker-a", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].ID != firstID || !jsonPayloadEqual(items[0].Payload, payload) {
			t.Fatalf("firing claim=%#v err=%v", items, err)
		}
		if _, err := store.ClaimNotifications(ctx, "worker-b", 1, time.Minute); err != nil {
			t.Fatal(err)
		}
		expireOutboxClaim(t, store, firstID)
		items, err = store.ClaimNotifications(ctx, "worker-b", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].ID != firstID || items[0].Attempts != 2 {
			t.Fatalf("recovered claim=%#v err=%v", items, err)
		}
		status, err := store.RetryNotification(ctx, firstID, "worker-b", "temporary", time.Minute, 3)
		if err != nil || status != "pending" {
			t.Fatalf("retry status=%q err=%v", status, err)
		}
		makeOutboxDue(t, store, firstID)
		items, err = store.ClaimNotifications(ctx, "worker-c", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].Attempts != 3 || items[0].LastError != "temporary" {
			t.Fatalf("final claim=%#v err=%v", items, err)
		}
		status, err = store.RetryNotification(ctx, firstID, "worker-c", "permanent", time.Minute, 3)
		if err != nil || status != "dead" {
			t.Fatalf("dead status=%q err=%v", status, err)
		}

		deferredID, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "delivery-deferred", ProjectID: project.ID, ChannelID: channel.ID,
			Payload: json.RawMessage(`{"status":"firing"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		items, err = store.ClaimNotifications(ctx, "worker-a", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].ID != deferredID || items[0].Attempts != 1 {
			t.Fatalf("defer claim=%#v err=%v", items, err)
		}
		if err := store.DeferNotification(ctx, deferredID, "worker-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		makeOutboxDue(t, store, deferredID)
		items, err = store.ClaimNotifications(ctx, "worker-b", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].Attempts != 1 {
			t.Fatalf("deferred reclaim=%#v err=%v", items, err)
		}
		if err := store.CompleteNotification(ctx, deferredID, "worker-b"); err != nil {
			t.Fatal(err)
		}

		for id, want := range map[int64]string{firstID: "dead", resolvedID: "delivered", deferredID: "delivered"} {
			item, err := store.NotificationOutboxItem(ctx, id)
			if err != nil || item.Status != want {
				t.Fatalf("outbox id=%d status=%q want=%q err=%v", id, item.Status, want, err)
			}
		}

		// Requeueing a dead item must reset attempts to 0 and make it
		// immediately claimable via the database clock.
		requeued, err := store.RequeueNotification(ctx, firstID, project.ID)
		if err != nil || !requeued {
			t.Fatalf("requeue dead item=%v err=%v", requeued, err)
		}
		requeuedItem, err := store.NotificationOutboxItem(ctx, firstID)
		if err != nil || requeuedItem.Status != "pending" || requeuedItem.Attempts != 0 {
			t.Fatalf("requeued item=%#v err=%v", requeuedItem, err)
		}
		items, err = store.ClaimNotifications(ctx, "worker-requeue", 1, time.Minute)
		if err != nil || len(items) != 1 || items[0].ID != firstID || items[0].Attempts != 1 {
			t.Fatalf("claim after requeue=%#v err=%v", items, err)
		}
		if err := store.CompleteNotification(ctx, firstID, "worker-requeue"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPostgresNotificationClaimsAreDisjoint(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	storeA := newPostgresTestStore(t)
	storeB, err := OpenStore(context.Background(), storeA.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	project, _, channel := createOutboxFixture(t, storeA, "disjoint")
	for i := range 10 {
		if _, _, err := storeA.EnqueueNotification(context.Background(), NotificationOutboxEnqueue{
			IdempotencyKey: fmt.Sprintf("disjoint-%d", i), ProjectID: project.ID, ChannelID: channel.ID,
			Payload: json.RawMessage(fmt.Sprintf(`{"sequence":%d}`, i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	claimed := make(chan []NotificationOutboxItem, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, store := range []*Store{storeA, storeB} {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			<-start
			items, err := store.ClaimNotifications(context.Background(), fmt.Sprintf("worker-%d", i), 10, time.Minute)
			claimed <- items
			errs <- err
		}(i, store)
	}
	close(start)
	wg.Wait()
	close(claimed)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[int64]struct{})
	for batch := range claimed {
		for _, item := range batch {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("outbox item %d claimed by both workers", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
	if len(seen) != 10 {
		t.Fatalf("claimed=%d want=10", len(seen))
	}
}

func createOutboxFixture(
	t *testing.T,
	store *Store,
	suffix string,
) (*Project, *dbent.AlertRule, *dbent.NotificationChannel) {
	t.Helper()
	project, err := store.client.Project.Create().
		SetName("Outbox " + suffix).
		SetSlug("outbox-" + suffix).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.client.AlertRule.Create().
		SetProjectID(project.ID).
		SetName("Outbox " + suffix).
		SetExpr("up").
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.client.NotificationChannel.Create().
		SetType(notificationchannel.TypeWebhook).
		SetName("outbox-" + suffix).
		SetConfig(map[string]any{"url": "https://example.invalid"}).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return projectFromEnt(project), rule, channel
}

func expireOutboxClaim(t *testing.T, store *Store, id int64) {
	t.Helper()
	query := `UPDATE notification_outbox SET claim_until = datetime('now', '-1 hour') WHERE id = ?`
	if store.dialect == "postgres" {
		query = `UPDATE notification_outbox SET claim_until = CURRENT_TIMESTAMP - INTERVAL '1 hour' WHERE id = ?`
	}
	if _, err := store.db.ExecContext(context.Background(), bind(store.dialect, query), id); err != nil {
		t.Fatal(err)
	}
}

func makeOutboxDue(t *testing.T, store *Store, id int64) {
	t.Helper()
	query := `UPDATE notification_outbox SET available_at = datetime('now', '-1 hour') WHERE id = ?`
	if store.dialect == "postgres" {
		query = `UPDATE notification_outbox SET available_at = CURRENT_TIMESTAMP - INTERVAL '1 hour' WHERE id = ?`
	}
	if _, err := store.db.ExecContext(context.Background(), bind(store.dialect, query), id); err != nil {
		t.Fatal(err)
	}
}

func jsonPayloadEqual(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func TestNotificationOutboxDepthAndFiringPlatformStates(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, _, channel := createOutboxFixture(t, store, "depth")

		// Seed one pending due, one processing, and one dead outbox item.
		pendingID, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "depth-pending", ProjectID: project.ID, ChannelID: channel.ID,
			Payload: json.RawMessage(`{"status":"firing"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		procID, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "depth-processing", ProjectID: project.ID, ChannelID: channel.ID,
			Payload: json.RawMessage(`{"status":"firing"}`), Priority: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		deadID, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "depth-dead", ProjectID: project.ID, ChannelID: channel.ID,
			Payload: json.RawMessage(`{"status":"firing"}`), Priority: 20,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Claim deadID and mark dead; claim procID and leave processing.
		claimed, err := store.ClaimNotifications(ctx, "depth-worker", 2, time.Minute)
		if err != nil || len(claimed) != 2 || claimed[0].ID != deadID || claimed[1].ID != procID {
			t.Fatalf("claimed=%#v err=%v", claimed, err)
		}
		if status, err := store.RetryNotification(ctx, deadID, "depth-worker", "boom", time.Minute, 1); err != nil ||
			status != "dead" {
			t.Fatalf("mark dead status=%q err=%v", status, err)
		}
		_ = pendingID

		depth, err := store.NotificationOutboxDepth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if depth.Pending != 1 || depth.Processing != 1 || depth.Dead != 1 || depth.Due != 1 {
			t.Fatalf("unexpected outbox depth=%+v want pending=1 processing=1 dead=1 due=1", depth)
		}

		// Verify ProjectFiringPlatformAlertStates tracks active durable spikes.
		now := time.Now().UTC().Truncate(time.Second)
		for i := range 6 {
			if _, err := store.RecordPlatformErrorRate(
				ctx,
				project.ID,
				now.Add(time.Duration(i)*time.Second),
				now.Add(time.Duration(i)*time.Second),
				5*time.Minute,
				1,
			); err != nil {
				t.Fatal(err)
			}
		}
		firing, err := store.ProjectFiringPlatformAlertStates(ctx, project.ID)
		if err != nil || len(firing) != 1 || firing[0].Kind != "error_rate_spike" ||
			firing[0].ProjectSlug != project.Slug {
			t.Fatalf("firing platform states=%#v err=%v", firing, err)
		}
		if _, err := store.ClearPlatformErrorRateState(ctx, project.ID, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		firing, err = store.ProjectFiringPlatformAlertStates(ctx, project.ID)
		if err != nil || len(firing) != 0 {
			t.Fatalf("expected 0 firing platform states after clear, got %#v err=%v", firing, err)
		}
	})
}

func TestRecentPlatformAlertStates(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		pFiring, _, _ := createOutboxFixture(t, store, "recent-firing")
		pResolvedBoundary, _, _ := createOutboxFixture(t, store, "recent-boundary")
		pResolvedOld, _, _ := createOutboxFixture(t, store, "recent-old")

		now := time.Now().UTC().Truncate(time.Second)
		resolvedBoundary := now.Add(6 * time.Minute)

		seedSpike := func(projectID int) {
			for i := range 6 {
				ts := now.Add(time.Duration(i) * time.Second)
				if _, err := store.RecordPlatformErrorRate(ctx, projectID, ts, ts, 5*time.Minute, 1); err != nil {
					t.Fatal(err)
				}
			}
		}

		seedSpike(pFiring.ID)
		seedSpike(pResolvedBoundary.ID)
		if tr, err := store.ClearPlatformErrorRateState(ctx, pResolvedBoundary.ID, resolvedBoundary); err != nil ||
			tr.Transition != "resolved" {
			t.Fatalf("clear boundary project transition=%+v err=%v", tr, err)
		}
		seedSpike(pResolvedOld.ID)
		if tr, err := store.ClearPlatformErrorRateState(
			ctx,
			pResolvedOld.ID,
			resolvedBoundary.Add(-time.Second),
		); err != nil || tr.Transition != "resolved" {
			t.Fatalf("clear old project transition=%+v err=%v", tr, err)
		}
		if _, err := store.db.ExecContext(
			ctx,
			bind(
				store.dialect,
				`INSERT INTO platform_alert_states(project_id, kind, status, active_cycle, starts_at, ends_at, updated_at)
				 VALUES (?, 'other_kind', 'firing', 1, ?, NULL, ?)`,
			),
			pFiring.ID,
			now,
			now,
		); err != nil {
			t.Fatal(err)
		}

		// States whose stream still has unacknowledged transitions are left to
		// the transition drain, so the state replay cannot overtake them.
		blocked, err := store.RecentPlatformAlertStates(ctx, platformAlertKindErrorRateSpike, resolvedBoundary, 0)
		if err != nil || len(blocked) != 0 {
			t.Fatalf("states with pending transitions=%#v err=%v want none", blocked, err)
		}
		pending, err := store.PendingPlatformAlertTransitions(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range pending {
			if err := store.AckPlatformAlertTransition(ctx, tr.ID); err != nil {
				t.Fatal(err)
			}
		}

		got, err := store.RecentPlatformAlertStates(ctx, platformAlertKindErrorRateSpike, resolvedBoundary, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("recent states len=%d want 2: %#v", len(got), got)
		}
		if got[0].ProjectID != pFiring.ID || got[0].Status != "firing" || got[0].EndsAt != nil ||
			got[0].ProjectSlug != pFiring.Slug {
			t.Fatalf("got[0]=%#v want firing for project %d", got[0], pFiring.ID)
		}
		if got[1].ProjectID != pResolvedBoundary.ID || got[1].Status != "resolved" ||
			got[1].EndsAt == nil || !got[1].EndsAt.Equal(resolvedBoundary) {
			t.Fatalf(
				"got[1]=%#v want resolved at boundary %v for project %d",
				got[1],
				resolvedBoundary,
				pResolvedBoundary.ID,
			)
		}

		onlyFiring, err := store.RecentPlatformAlertStates(
			ctx,
			platformAlertKindErrorRateSpike,
			resolvedBoundary,
			pFiring.ID,
		)
		if err != nil || len(onlyFiring) != 1 || onlyFiring[0].ProjectID != pFiring.ID {
			t.Fatalf("project-filtered firing=%#v err=%v", onlyFiring, err)
		}
		onlyBoundary, err := store.RecentPlatformAlertStates(
			ctx,
			platformAlertKindErrorRateSpike,
			resolvedBoundary,
			pResolvedBoundary.ID,
		)
		if err != nil || len(onlyBoundary) != 1 || onlyBoundary[0].ProjectID != pResolvedBoundary.ID {
			t.Fatalf("project-filtered boundary=%#v err=%v", onlyBoundary, err)
		}
		onlyOld, err := store.RecentPlatformAlertStates(
			ctx,
			platformAlertKindErrorRateSpike,
			resolvedBoundary,
			pResolvedOld.ID,
		)
		if err != nil || len(onlyOld) != 0 {
			t.Fatalf("project-filtered old=%#v err=%v want empty", onlyOld, err)
		}
		otherKind, err := store.RecentPlatformAlertStates(ctx, "other_kind", resolvedBoundary, 0)
		if err != nil || len(otherKind) != 1 || otherKind[0].Kind != "other_kind" ||
			otherKind[0].ProjectID != pFiring.ID {
			t.Fatalf("other_kind states=%#v err=%v", otherKind, err)
		}
	})
}

func TestNotificationOutboxExistingKeys(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, _, channel := createOutboxFixture(t, store, "existing-keys")

		if _, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "exist-pending",
			ProjectID:      project.ID,
			ChannelID:      channel.ID,
			Payload:        json.RawMessage(`{"status":"firing"}`),
		}); err != nil {
			t.Fatal(err)
		}
		deliveredID, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "exist-delivered",
			ProjectID:      project.ID,
			ChannelID:      channel.ID,
			Payload:        json.RawMessage(`{"status":"firing"}`),
			Priority:       10,
		})
		if err != nil {
			t.Fatal(err)
		}
		deadID, _, err := store.EnqueueNotification(ctx, NotificationOutboxEnqueue{
			IdempotencyKey: "exist-dead",
			ProjectID:      project.ID,
			ChannelID:      channel.ID,
			Payload:        json.RawMessage(`{"status":"firing"}`),
			Priority:       20,
		})
		if err != nil {
			t.Fatal(err)
		}

		claimed, err := store.ClaimNotifications(ctx, "exist-worker", 2, time.Minute)
		if err != nil || len(claimed) != 2 || claimed[0].ID != deadID || claimed[1].ID != deliveredID {
			t.Fatalf("claimed=%#v err=%v", claimed, err)
		}
		if status, err := store.RetryNotification(ctx, deadID, "exist-worker", "boom", time.Minute, 1); err != nil ||
			status != "dead" {
			t.Fatalf("mark dead status=%q err=%v", status, err)
		}
		if err := store.CompleteNotification(ctx, deliveredID, "exist-worker"); err != nil {
			t.Fatal(err)
		}

		empty, err := store.NotificationOutboxExistingKeys(ctx, nil)
		if err != nil || len(empty) != 0 {
			t.Fatalf("nil keys=%#v err=%v", empty, err)
		}
		blankOnly, err := store.NotificationOutboxExistingKeys(ctx, []string{""})
		if err != nil || len(blankOnly) != 0 {
			t.Fatalf("blank keys=%#v err=%v", blankOnly, err)
		}

		got, err := store.NotificationOutboxExistingKeys(
			ctx,
			[]string{"exist-pending", "exist-delivered", "exist-dead", "exist-missing", "exist-pending", ""},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("existing keys len=%d want 3: %#v", len(got), got)
		}
		for _, want := range []string{"exist-pending", "exist-delivered", "exist-dead"} {
			if _, ok := got[want]; !ok {
				t.Fatalf("missing expected key %q in %#v", want, got)
			}
		}
		if _, ok := got["exist-missing"]; ok {
			t.Fatalf("unexpected key exist-missing in %#v", got)
		}
	})
}
