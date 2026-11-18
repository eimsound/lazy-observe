package account_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/redis/go-redis/v9"
)

func openResetTestStore(t *testing.T) *meta.Store {
	t.Helper()
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// resetSessionAdapters returns one adapter per production session backend so a
// reset that only clears the metadata table cannot pass.
func resetSessionAdapters(t *testing.T, store *meta.Store) map[string]account.Sessions {
	t.Helper()
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	return map[string]account.Sessions{
		"metadata": account.NewMetadataSessions(store),
		"redis":    account.NewStateSessions(state.NewRedis(redisClient, "reset-password")),
	}
}

func TestResetPasswordReplacesCredentialsAndRevokesEverySession(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"metadata", "redis"} {
		t.Run(name, func(t *testing.T) {
			store := openResetTestStore(t)
			sessions := resetSessionAdapters(t, store)[name]
			admin, err := store.CreateUser(ctx, "admin", "old-admin-password", "admin")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			for range 2 {
				if _, err := sessions.Create(ctx, admin.ID, now, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}

			reset, err := account.ResetPassword(
				ctx, store, sessions, "admin", "operator-chosen-password", true,
			)
			if err != nil {
				t.Fatalf("reset password: %v", err)
			}
			if reset.Username != "admin" || !reset.MustChangePassword {
				t.Fatalf("reset user = %#v, want admin with must_change_password", reset)
			}
			if _, err := store.AuthenticateCredentials(ctx, "admin", "old-admin-password"); err == nil {
				t.Fatal("old password still authenticates admin")
			}
			updated, err := store.AuthenticateCredentials(ctx, "admin", "operator-chosen-password")
			if err != nil {
				t.Fatalf("new password rejected: %v", err)
			}
			if !updated.MustChangePassword {
				t.Fatal("reset must set must_change_password")
			}
			remaining, err := sessions.List(ctx, admin.ID, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if len(remaining) != 0 {
				t.Fatalf("sessions after reset = %d, want 0", len(remaining))
			}

			// A deployer-chosen password starts a clean state: the flag is
			// cleared, and every session of the previous password stays dead.
			if _, err := sessions.Create(ctx, admin.ID, now, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := account.ResetPassword(
				ctx, store, sessions, "admin", "second-new-password", false,
			); err != nil {
				t.Fatalf("second reset: %v", err)
			}
			cleared, err := store.AuthenticateCredentials(ctx, "admin", "second-new-password")
			if err != nil {
				t.Fatalf("second new password rejected: %v", err)
			}
			if cleared.MustChangePassword {
				t.Fatal("a password supplied by the operator must not force a change")
			}
			remaining, err = sessions.List(ctx, admin.ID, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if len(remaining) != 0 {
				t.Fatalf("sessions after second reset = %d, want 0", len(remaining))
			}
		})
	}
}

// resetRevokeFailure fails session revocation so the reset must keep the old
// password: a generated password file is dropped by the caller on any error,
// and deleting the only copy of an applied password would lock the operator
// out.
type resetRevokeFailure struct{ account.Sessions }

func (resetRevokeFailure) RevokeOthers(context.Context, int, string) error {
	return errors.New("session backend unavailable")
}

func TestResetPasswordKeepsTheOldPasswordWhenRevocationFails(t *testing.T) {
	ctx := context.Background()
	store := openResetTestStore(t)
	if _, err := store.CreateUser(ctx, "admin", "old-admin-password", "admin"); err != nil {
		t.Fatal(err)
	}
	sessions := resetRevokeFailure{Sessions: account.NewMetadataSessions(store)}
	_, err := account.ResetPassword(ctx, store, sessions, "admin", "new-admin-password", true)
	if account.Code(err) != account.CodeUnavailable {
		t.Fatalf("revocation failure error = %v, want service_unavailable", err)
	}
	if _, err := store.AuthenticateCredentials(ctx, "admin", "old-admin-password"); err != nil {
		t.Fatalf("failed revocation changed the password: %v", err)
	}
	if _, err := store.AuthenticateCredentials(ctx, "admin", "new-admin-password"); err == nil {
		t.Fatal("a reset with a failed revocation applied the new password")
	}
}

// secondRevokeFailure lets the first sweep succeed and fails the one after the
// password write, which is the race the two-sweep ordering protects against.
type secondRevokeFailure struct {
	account.Sessions
	calls int
}

func (s *secondRevokeFailure) RevokeOthers(ctx context.Context, userID int, keepToken string) error {
	s.calls++
	if s.calls == 2 {
		return errors.New("session backend unavailable")
	}
	return s.Sessions.RevokeOthers(ctx, userID, keepToken)
}

func TestResetPasswordReportsThePostWriteRevocationFailure(t *testing.T) {
	ctx := context.Background()
	store := openResetTestStore(t)
	admin, err := store.CreateUser(ctx, "admin", "old-admin-password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	base := account.NewMetadataSessions(store)
	now := time.Now()
	if _, err := base.Create(ctx, admin.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sessions := &secondRevokeFailure{Sessions: base}
	reset, err := account.ResetPassword(ctx, store, sessions, "admin", "new-admin-password", true)
	if account.Stage(err) != account.StageSessionRevokeAfterReset {
		t.Fatalf("post-write revocation failure = %v, want stage %q", err, account.StageSessionRevokeAfterReset)
	}
	if reset.Username != "admin" {
		t.Fatalf("reset result = %#v, want the admin account so callers keep the password file", reset)
	}
	if _, err := store.AuthenticateCredentials(ctx, "admin", "new-admin-password"); err != nil {
		t.Fatalf("the new password must already work: %v", err)
	}
	if _, err := store.AuthenticateCredentials(ctx, "admin", "old-admin-password"); err == nil {
		t.Fatal("the old password survived the reset")
	}
	remaining, err := base.List(ctx, admin.ID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("sessions after the first sweep = %d, want 0", len(remaining))
	}
}

func TestResetPasswordRejectsUnknownUser(t *testing.T) {
	ctx := context.Background()
	store := openResetTestStore(t)
	admin, err := store.CreateUser(ctx, "admin", "old-admin-password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	sessions := account.NewMetadataSessions(store)
	_, err = account.ResetPassword(ctx, store, sessions, "ghost", "ghost-password", true)
	if account.Code(err) != account.CodeNotFound {
		t.Fatalf("unknown user error = %v, want not_found", err)
	}
	if _, err := store.AuthenticateCredentials(ctx, "admin", "old-admin-password"); err != nil {
		t.Fatalf("a failed reset changed another user's password: %v", err)
	}
	if _, err := store.GetUser(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
}
