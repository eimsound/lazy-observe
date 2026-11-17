package meta

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Bearer credentials and session tokens must not be recoverable from a copy
// of the metadata database: only their digests are stored, and the plaintext
// values keep authenticating.
func TestCredentialsAreStoredAsDigests(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Digest", "digest")
		if err != nil {
			t.Fatal(err)
		}
		_, projectToken, err := store.CreateNamedProjectAPIToken(ctx, project.ID, "sdk")
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.CreateUser(ctx, "digest-user", "password", "viewer")
		if err != nil {
			t.Fatal(err)
		}
		_, userToken, err := store.CreateUserAPIToken(ctx, user.ID, "cli")
		if err != nil {
			t.Fatal(err)
		}
		sessionToken := "session-" + strings.Repeat("s", 40)
		now := time.Now().UTC()
		if err := store.CreateAccountSession(ctx, AccountSession{
			Token: sessionToken, UserID: user.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		for table, secret := range map[string]string{
			"project_api_tokens": projectToken, "user_api_tokens": userToken, "sessions": sessionToken,
		} {
			var stored string
			if err := store.db.QueryRowContext(ctx, "SELECT token FROM "+table).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored, secret) || len(stored) != 64 {
				t.Errorf("%s stores %q for secret %q; want a 64-hex digest", table, stored, secret)
			}
		}

		if got, err := store.AuthenticateProjectAPIToken(ctx, projectToken); err != nil || got.ID != project.ID {
			t.Fatalf("project token auth project=%+v err=%v", got, err)
		}
		if got, err := store.AuthenticateUserAPIToken(ctx, userToken); err != nil || got.ID != user.ID {
			t.Fatalf("user token auth user=%+v err=%v", got, err)
		}
		if got, err := store.AuthenticateAccountSession(ctx, sessionToken, now); err != nil || got != user.ID {
			t.Fatalf("session auth user=%d err=%v", got, err)
		}
		refs, err := store.ListAccountSessions(ctx, user.ID, sessionToken, now)
		if err != nil || len(refs) != 1 || !refs[0].Current || refs[0].ID != accountSessionReference(sessionToken) {
			t.Fatalf("session refs=%+v err=%v", refs, err)
		}
		if err := store.DeleteAccountSession(ctx, sessionToken); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AuthenticateAccountSession(ctx, sessionToken, now); err == nil {
			t.Fatal("deleted session still authenticates")
		}
	})
}

// The self-monitoring token is derived, not read back: every bootstrap with the
// same derived value converges on one stored digest.
func TestBootstrapSelfMonitoringAcceptsDerivedToken(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		derived := "lo_" + strings.Repeat("d", 48)
		for range 2 {
			project, token, err := store.BootstrapSelfMonitoring(ctx, derived)
			if err != nil || token != derived || project.Slug != SelfMonitoringProjectSlug {
				t.Fatalf("bootstrap project=%+v token=%q err=%v", project, token, err)
			}
		}
		var rows int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM project_api_tokens").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("self-monitoring token rows=%d want 1", rows)
		}
		if got, err := store.AuthenticateProjectAPIToken(ctx, derived); err != nil ||
			got.Slug != SelfMonitoringProjectSlug {
			t.Fatalf("derived token auth project=%+v err=%v", got, err)
		}
	})
}
