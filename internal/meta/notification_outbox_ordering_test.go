package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/notificationchannel"
)

func enqueueOrdered(
	t *testing.T,
	store *Store,
	project *Project,
	channelID int,
	key, orderingKey string,
	priority int,
) int64 {
	t.Helper()
	id, inserted, err := store.EnqueueNotification(context.Background(), NotificationOutboxEnqueue{
		IdempotencyKey: key,
		ProjectID:      project.ID,
		ChannelID:      channelID,
		Payload:        json.RawMessage(fmt.Sprintf(`{"key":%q}`, key)),
		Priority:       priority,
		OrderingKey:    orderingKey,
	})
	if err != nil || !inserted {
		t.Fatalf("enqueue %s inserted=%v err=%v", key, inserted, err)
	}
	return id
}

func claimedIDs(t *testing.T, store *Store, owner string, limit int) []int64 {
	t.Helper()
	items, err := store.ClaimNotifications(context.Background(), owner, limit, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	slices.Sort(ids)
	return ids
}

func secondOutboxChannel(t *testing.T, store *Store, name string) int {
	t.Helper()
	channel, err := store.client.NotificationChannel.Create().
		SetType(notificationchannel.TypeWebhook).
		SetName(name).
		SetConfig(map[string]any{"url": "https://example.invalid"}).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return channel.ID
}

// Issue #386: a resolved row (priority 100) must not be claimed while the
// firing row ahead of it in the same (ordering_key, channel) stream is pending
// or processing, neither in the same batch nor while the firing row waits in
// retry backoff. Terminal predecessors (delivered or dead) release it.
func TestNotificationOutboxOrderingStreamClaimsInOrder(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, _, channel := createOutboxFixture(t, store, "ordering")
		other := secondOutboxChannel(t, store, "outbox-ordering-other")
		stream := "platform-state:1:error_rate_spike"
		firing1 := enqueueOrdered(t, store, project, channel.ID, "firing-1", stream, 0)
		resolved1 := enqueueOrdered(t, store, project, channel.ID, "resolved-1", stream, 100)
		firing2 := enqueueOrdered(t, store, project, channel.ID, "firing-2", stream, 0)
		otherChannel := enqueueOrdered(t, store, project, other, "other-channel", stream, 100)
		otherStream := enqueueOrdered(t, store, project, channel.ID, "other-stream", "rule:9", 100)
		unordered := enqueueOrdered(t, store, project, channel.ID, "unordered", "", 50)

		item, err := store.NotificationOutboxItem(ctx, resolved1)
		if err != nil || item.OrderingKey != stream {
			t.Fatalf("stored ordering key=%q err=%v", item.OrderingKey, err)
		}
		depth, err := store.NotificationOutboxDepth(ctx)
		if err != nil || depth.Pending != 6 || depth.Due != 4 {
			t.Fatalf("depth=%+v err=%v want 6 pending, 4 due (successors blocked)", depth, err)
		}

		// One batch holds at most the head of each stream.
		got := claimedIDs(t, store, "worker-a", 10)
		want := []int64{firing1, otherChannel, otherStream, unordered}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("first claim=%v want heads %v", got, want)
		}
		for _, id := range []int64{otherChannel, otherStream, unordered} {
			if err := store.CompleteNotification(ctx, id, "worker-a"); err != nil {
				t.Fatal(err)
			}
		}
		// Processing firing keeps resolved blocked.
		if got := claimedIDs(t, store, "worker-b", 10); len(got) != 0 {
			t.Fatalf("claimed %v while firing is processing", got)
		}
		// Firing backs off after a transient failure: resolved still waits.
		if status, err := store.RetryNotification(ctx, firing1, "worker-a", "boom", time.Hour, 5); err != nil ||
			status != "pending" {
			t.Fatalf("retry status=%q err=%v", status, err)
		}
		if got := claimedIDs(t, store, "worker-b", 10); len(got) != 0 {
			t.Fatalf("claimed %v while firing waits in backoff", got)
		}
		// The rate limiter deferring firing does not let resolved overtake it.
		makeOutboxDue(t, store, firing1)
		if got := claimedIDs(t, store, "worker-b", 10); !slices.Equal(got, []int64{firing1}) {
			t.Fatalf("claim after backoff=%v want firing %d", got, firing1)
		}
		if err := store.DeferNotification(ctx, firing1, "worker-b", time.Hour); err != nil {
			t.Fatal(err)
		}
		if got := claimedIDs(t, store, "worker-b", 10); len(got) != 0 {
			t.Fatalf("claimed %v while firing is deferred", got)
		}
		makeOutboxDue(t, store, firing1)
		if got := claimedIDs(t, store, "worker-b", 10); !slices.Equal(got, []int64{firing1}) {
			t.Fatalf("claim after deferral=%v want firing %d", got, firing1)
		}
		if err := store.CompleteNotification(ctx, firing1, "worker-b"); err != nil {
			t.Fatal(err)
		}
		// Delivered firing releases resolved, but not firing(C+1) behind it.
		if got := claimedIDs(t, store, "worker-c", 10); !slices.Equal(got, []int64{resolved1}) {
			t.Fatalf("claim after firing delivered=%v want resolved %d", got, resolved1)
		}
		// A resolved that exhausts its retries (dead) releases the next cycle.
		if status, err := store.RetryNotification(ctx, resolved1, "worker-c", "gone", time.Second, 1); err != nil ||
			status != "dead" {
			t.Fatalf("terminal retry status=%q err=%v", status, err)
		}
		if got := claimedIDs(t, store, "worker-c", 10); !slices.Equal(got, []int64{firing2}) {
			t.Fatalf("claim after resolved dead=%v want firing(C+1) %d", got, firing2)
		}
	})
}

// A predecessor whose claim expired (its worker died mid-delivery) is
// reclaimed first; its successor stays blocked until it is terminal.
func TestNotificationOutboxOrderingExpiredClaimKeepsSuccessorBlocked(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, _, channel := createOutboxFixture(t, store, "ordering-expired")
		firing := enqueueOrdered(t, store, project, channel.ID, "expired-firing", "rule:1", 0)
		resolved := enqueueOrdered(t, store, project, channel.ID, "expired-resolved", "rule:1", 100)
		if got := claimedIDs(t, store, "crashed", 10); !slices.Equal(got, []int64{firing}) {
			t.Fatalf("first claim=%v want firing %d", got, firing)
		}
		expireOutboxClaim(t, store, firing)
		if got := claimedIDs(t, store, "survivor", 10); !slices.Equal(got, []int64{firing}) {
			t.Fatalf("claim after expiry=%v want only firing %d reclaimed", got, firing)
		}
		if err := store.CompleteNotification(ctx, firing, "survivor"); err != nil {
			t.Fatal(err)
		}
		if got := claimedIDs(t, store, "survivor", 10); !slices.Equal(got, []int64{resolved}) {
			t.Fatalf("claim after completion=%v want resolved %d", got, resolved)
		}
	})
}

// Competing workers (separate stores on PostgreSQL) drain many interleaved
// streams; no row may be claimed while an earlier row of its stream is not yet
// delivered, and every row is delivered exactly once.
func TestNotificationOutboxOrderingConcurrentWorkers(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, _, channel := createOutboxFixture(t, store, "ordering-concurrent")
		const streams, depth = 6, 5
		prev := make(map[int64]int64)
		for step := range depth {
			for s := range streams {
				id := enqueueOrdered(t, store, project, channel.ID, fmt.Sprintf("c-%d-%d", s, step),
					fmt.Sprintf("rule:%d", s), 100*(step%2))
				if step > 0 {
					prev[id] = id - int64(streams)
				}
			}
		}
		stores := []*Store{store, store, store, store}
		if store.dialect == "postgres" {
			for i := 1; i < len(stores); i++ {
				other, err := OpenStore(ctx, store.dbURL)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Close() })
				stores[i] = other
			}
		}
		var (
			mu        sync.Mutex
			delivered = make(map[int64]bool)
			failures  []string
			wg        sync.WaitGroup
		)
		deadline := time.Now().Add(20 * time.Second)
		for w, workerStore := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				owner := fmt.Sprintf("worker-%d", w)
				for time.Now().Before(deadline) {
					items, err := workerStore.ClaimNotifications(ctx, owner, 3, time.Minute)
					if err != nil {
						mu.Lock()
						failures = append(failures, err.Error())
						mu.Unlock()
						return
					}
					mu.Lock()
					done := len(delivered) == streams*depth
					for _, item := range items {
						if before, ok := prev[item.ID]; ok && !delivered[before] {
							failures = append(
								failures,
								fmt.Sprintf("claimed %d before predecessor %d", item.ID, before),
							)
						}
					}
					mu.Unlock()
					if done {
						return
					}
					for _, item := range items {
						// Record delivery before the completion commits: a
						// successor becomes claimable only after that commit.
						mu.Lock()
						if delivered[item.ID] {
							failures = append(failures, fmt.Sprintf("row %d delivered twice", item.ID))
						}
						delivered[item.ID] = true
						mu.Unlock()
						if err := workerStore.CompleteNotification(ctx, item.ID, owner); err != nil {
							mu.Lock()
							failures = append(failures, err.Error())
							mu.Unlock()
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		if len(failures) != 0 {
			t.Fatalf("ordering violations: %v", failures)
		}
		if len(delivered) != streams*depth {
			t.Fatalf("delivered=%d want %d", len(delivered), streams*depth)
		}
	})
}
