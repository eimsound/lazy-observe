package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMemoryContracts(t *testing.T) {
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	testSessionContract(t, newMemorySessionStore(func() time.Time { return now }))
	testLimiterContract(t, newMemoryLimiter(func() time.Time { return now }), func() { now = now.Add(time.Minute) })
}

func TestRedisContractsAndCrossReplicaSharing(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	replicaA := NewRedis(clientA, "contract")
	replicaB := NewRedis(clientB, "contract")
	testSessionContract(t, NewRedis(clientA, "session-contract"))
	testLimiterContract(t, NewRedis(clientA, "limiter-contract"), func() { server.FastForward(time.Minute) })

	expires := time.Now().Add(time.Hour)
	session := Session{Token: "shared", UserID: 7, ExpiresAt: expires}
	if err := replicaA.Create(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	got, err := replicaB.Get(context.Background(), session.Token)
	if err != nil || got.UserID != session.UserID {
		t.Fatalf("cross-replica session=%#v err=%v", got, err)
	}
	if err := replicaB.Delete(context.Background(), session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := replicaA.Get(context.Background(), session.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session error=%v", err)
	}

	const attempts, limit = 100, 25
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replica := replicaA
			if i%2 == 1 {
				replica = replicaB
			}
			ok, allowErr := replica.Allow(context.Background(), "atomic", limit, time.Minute)
			if allowErr != nil {
				t.Errorf("Allow: %v", allowErr)
			} else if ok {
				allowed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := allowed.Load(); got != limit {
		t.Fatalf("allowed=%d want=%d", got, limit)
	}
}

func testSessionContract(t *testing.T, store SessionStore) {
	t.Helper()
	ctx := context.Background()
	session := Session{
		Token:     "token",
		UserID:    1,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, session.Token); err != nil || got.UserID != session.UserID {
		t.Fatalf("Get=%#v err=%v", got, err)
	}
	if err := store.Touch(ctx, session.Token, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, session.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete error=%v", err)
	}
}

func testLimiterContract(t *testing.T, limiter Limiter, advance func()) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		allowed, err := limiter.Allow(ctx, "key", 2, time.Minute)
		if err != nil || allowed != (i < 2) {
			t.Fatalf("attempt %d allowed=%v err=%v", i, allowed, err)
		}
	}
	advance()
	allowed, err := limiter.Allow(ctx, "key", 2, time.Minute)
	if err != nil || !allowed {
		t.Fatalf("reset allowed=%v err=%v", allowed, err)
	}
}

func TestMemorySessionManagementAndLoginGate(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	store := newMemorySessionStore(func() time.Time { return now })
	_ = store.Create(context.Background(), Session{Token: "a", UserID: 1, ExpiresAt: now.Add(time.Hour)})
	_ = store.Create(context.Background(), Session{Token: "b", UserID: 1, ExpiresAt: now.Add(time.Hour)})
	_ = store.Create(context.Background(), Session{Token: "c", UserID: 2, ExpiresAt: now.Add(time.Hour)})

	lister := store
	refs, err := lister.ListByUser(context.Background(), 1, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("user 1 session count = %d, want 2", len(refs))
	}
	for _, ref := range refs {
		if ref.Current != (ref.ID == sessionRefID("a")) {
			t.Fatalf("current flag wrong: %#v", ref)
		}
	}
	if err := lister.DeleteByRef(context.Background(), 1, sessionRefID("b")); err != nil {
		t.Fatal(err)
	}
	if err := lister.DeleteByRef(context.Background(), 1, sessionRefID("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err=%v", err)
	}
	if err := lister.DeleteByRef(context.Background(), 2, sessionRefID("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete err=%v", err)
	}
	if err := lister.DeleteOtherByUser(context.Background(), 1, "a"); err != nil {
		t.Fatal(err)
	}
	refs, _ = lister.ListByUser(context.Background(), 1, "a")
	if len(refs) != 1 || !refs[0].Current {
		t.Fatalf("sessions after sweep: %#v", refs)
	}

	gate := NewMemoryLoginGateWithClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		locked, err := gate.ConsumeFailure(context.Background(), "acct", 3, time.Minute, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if locked != (i == 2) {
			t.Fatalf("failure %d locked=%v", i+1, locked)
		}
	}
	locked, err := gate.Locked(context.Background(), "acct")
	if err != nil || !locked {
		t.Fatalf("expected locked, got %v err=%v", locked, err)
	}
	if err := gate.Clear(context.Background(), "acct"); err != nil {
		t.Fatal(err)
	}
	locked, _ = gate.Locked(context.Background(), "acct")
	if locked {
		t.Fatal("lock not cleared")
	}
}

func TestRedisSessionManagementAndLoginGate(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := NewRedis(client, "lo-test")
	now := time.Now()
	_ = store.Create(
		context.Background(),
		Session{Token: "r-a", UserID: 7, ExpiresAt: now.Add(time.Hour), CreatedAt: now},
	)
	_ = store.Create(
		context.Background(),
		Session{Token: "r-b", UserID: 7, ExpiresAt: now.Add(time.Hour), CreatedAt: now},
	)
	lister := store
	refs, err := lister.ListByUser(context.Background(), 7, "r-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("redis session count = %d, want 2", len(refs))
	}
	if err := lister.DeleteByRef(context.Background(), 7, sessionRefID("r-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "r-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("redis session not deleted: %v", err)
	}
	if err := lister.DeleteOtherByUser(context.Background(), 7, "r-a"); err != nil {
		t.Fatal(err)
	}
	refs, _ = lister.ListByUser(context.Background(), 7, "r-a")
	if len(refs) != 1 {
		t.Fatalf("redis sessions after sweep: %#v", refs)
	}
	if err := store.Delete(context.Background(), "r-a"); err != nil {
		t.Fatal(err)
	}
	refs, _ = lister.ListByUser(context.Background(), 7, "")
	if len(refs) != 0 {
		t.Fatalf("redis index not cleaned: %#v", refs)
	}

	gate := LoginGate(store)
	for i := 0; i < 3; i++ {
		locked, err := gate.ConsumeFailure(context.Background(), "acct", 3, time.Minute, time.Hour)
		if err != nil || locked != (i == 2) {
			t.Fatalf("redis failure %d locked=%v err=%v", i+1, locked, err)
		}
	}
	locked, err := gate.Locked(context.Background(), "acct")
	if err != nil || !locked {
		t.Fatalf("redis expected locked, got %v err=%v", locked, err)
	}
	if err := gate.Clear(context.Background(), "acct"); err != nil {
		t.Fatal(err)
	}
	locked, _ = gate.Locked(context.Background(), "acct")
	if locked {
		t.Fatal("redis lock not cleared")
	}
}

// Redis holds shared sessions in HA mode; a dump of it must not yield usable
// session tokens, so keys and per-user index members use the token digest.
func TestRedisSessionsStoreTokenDigests(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedis(client, "digest")
	const token = "plaintext-session-token-value"
	if err := store.Create(
		context.Background(),
		Session{Token: token, UserID: 9, ExpiresAt: time.Now().Add(time.Hour)},
	); err != nil {
		t.Fatal(err)
	}
	for _, key := range server.Keys() {
		if strings.Contains(key, token) {
			t.Fatalf("redis key %q contains the session token", key)
		}
		if members, err := server.Members(key); err == nil {
			for _, member := range members {
				if strings.Contains(member, token) {
					t.Fatalf("redis set %q member contains the session token", key)
				}
			}
		}
	}
	if got, err := store.Get(context.Background(), token); err != nil || got.UserID != 9 {
		t.Fatalf("session=%+v err=%v", got, err)
	}
	refs, err := store.ListByUser(context.Background(), 9, token)
	if err != nil || len(refs) != 1 || !refs[0].Current || refs[0].ID != sessionRefID(token) {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
}
