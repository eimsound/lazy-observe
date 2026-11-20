package account_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/redis/go-redis/v9"
)

func TestSessionAdaptersShareOpaqueReferenceContract(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	owner, err := store.CreateUser(t.Context(), "owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateUser(t.Context(), "other", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[string]account.Sessions{
		"metadata": account.NewMetadataSessions(store),
		"redis":    account.NewStateSessions(state.NewRedis(redisClient, "account-contract")),
	}
	for name, sessions := range adapters {
		t.Run(name, func(t *testing.T) {
			testSessionContract(t, sessions, owner.ID, other.ID, now)
		})
	}
}

func testSessionContract(t *testing.T, sessions account.Sessions, ownerID, otherID int, now time.Time) {
	t.Helper()
	ctx := context.Background()
	first, err := sessions.Create(ctx, ownerID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, account.SessionTokenPrefix) ||
		strings.HasPrefix(first, meta.SourceMapUploadTokenPrefix) {
		t.Fatalf(
			"session token prefix = %q, want %q and distinct from %q",
			first,
			account.SessionTokenPrefix,
			meta.SourceMapUploadTokenPrefix,
		)
	}
	second, err := sessions.Create(ctx, ownerID, now.Add(time.Minute), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Create(ctx, otherID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if userID, err := sessions.Authenticate(ctx, first, now); err != nil || userID != ownerID {
		t.Fatalf("authenticate user=%d err=%v", userID, err)
	}
	listed, err := sessions.List(ctx, ownerID, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(listed))
	}
	for _, session := range listed {
		if session.ID == "" || session.ID == first || session.ID == second {
			t.Fatalf("session reference exposes token: %#v", session)
		}
		if _, err := sessions.Authenticate(ctx, session.ID, now); !errors.Is(err, account.ErrSessionNotFound) {
			t.Fatalf("reference authenticated: err=%v", err)
		}
	}

	ref := listed[0].ID
	if err := sessions.Revoke(ctx, otherID, ref); !errors.Is(err, account.ErrSessionNotFound) {
		t.Fatalf("cross-user revoke err=%v", err)
	}
	if err := sessions.Revoke(ctx, ownerID, ref); err != nil {
		t.Fatal(err)
	}
	if err := sessions.RevokeOthers(ctx, ownerID, first); err != nil {
		t.Fatal(err)
	}
	remaining, err := sessions.List(ctx, ownerID, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) > 1 {
		t.Fatalf("remaining sessions=%d, want at most current", len(remaining))
	}
}

func TestLegacyLosPrefixedSessionsRemainCompatible(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, err := store.CreateUser(t.Context(), "legacy-owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	const legacyToken = "los_existing-legacy-session-token"
	if err := store.CreateAccountSession(t.Context(), meta.AccountSession{
		Token: legacyToken, UserID: owner.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	sessions := account.NewMetadataSessions(store)
	userID, err := sessions.Authenticate(t.Context(), legacyToken, now)
	if err != nil || userID != owner.ID {
		t.Fatalf("legacy session auth user=%d err=%v", userID, err)
	}
}
