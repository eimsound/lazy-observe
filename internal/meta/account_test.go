package meta

import (
	"context"
	"testing"
	"time"
)

func TestChangePassword(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		u, err := store.CreateUser(ctx, "changer", "old-password", "member")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ChangePassword(ctx, u.ID, "wrong", "new-password"); !IsInvalidCredentials(err) {
			t.Fatalf("wrong current password: err=%v", err)
		}
		if err := store.ChangePassword(ctx, u.ID, "old-password", "new-password"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AuthenticateCredentials(ctx, "changer", "new-password"); err != nil {
			t.Fatalf("new password rejected: %v", err)
		}
		if _, err := store.AuthenticateCredentials(ctx, "changer", "old-password"); !IsInvalidCredentials(err) {
			t.Fatalf("old password still accepted: %v", err)
		}
	})
}

func TestUpdateUserProfileSetAndClear(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		u, err := store.CreateUser(ctx, "profiler", "password", "member")
		if err != nil {
			t.Fatal(err)
		}
		name, email := "Ada Lovelace", "ada@example.com"
		u, err = store.UpdateUserProfile(ctx, u.ID, &name, &email)
		if err != nil {
			t.Fatal(err)
		}
		if u.DisplayName != name || u.Email != email {
			t.Fatalf("profile not stored: %#v", u)
		}
		empty := ""
		u, err = store.UpdateUserProfile(ctx, u.ID, &empty, nil)
		if err != nil {
			t.Fatal(err)
		}
		if u.DisplayName != "" || u.Email != email {
			t.Fatalf("display name not cleared: %#v", u)
		}
	})
}

func TestMarkLoginStampsLastLoginAt(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		u, err := store.CreateUser(ctx, "stamper", "password", "member")
		if err != nil {
			t.Fatal(err)
		}
		if u.LastLoginAt != nil {
			t.Fatal("new user already has last_login_at")
		}
		if err := store.MarkLogin(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		u, err = store.GetUser(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if u.LastLoginAt == nil || u.LastLoginAt.IsZero() {
			t.Fatal("last_login_at was not stamped")
		}
	})
}

func TestAuditRecordingListingAndCleanup(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		records := []AuditRecord{
			{Action: "auth.login", ActorUsername: "admin", ActorRole: "admin", TargetType: "user", Success: true},
			{
				Action:        "user.create",
				ActorUsername: "admin",
				ActorRole:     "admin",
				TargetType:    "user",
				TargetName:    "bob",
				Success:       true,
			},
			{
				Action:        "auth.login",
				ActorUsername: "bob",
				TargetType:    "user",
				Success:       false,
				FailureReason: "invalid_credentials",
			},
			{
				Action:        "project.create",
				ActorUsername: "admin",
				TargetType:    "project",
				TargetName:    "Alpha",
				ProjectID:     7,
				ProjectSlug:   "alpha",
				Success:       true,
			},
		}
		for _, record := range records {
			if err := store.RecordAudit(ctx, record); err != nil {
				t.Fatal(err)
			}
		}
		all, err := store.ListAudit(ctx, AuditListOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 {
			t.Fatalf("audit list = %d, want 4", len(all))
		}
		if all[0].Action != "project.create" {
			t.Fatalf("audit not newest-first: %#v", all[0])
		}
		loginOnly, err := store.ListAudit(ctx, AuditListOptions{Action: "auth.login", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(loginOnly) != 2 {
			t.Fatalf("login audit filter = %d, want 2", len(loginOnly))
		}
		byActor, err := store.ListAudit(ctx, AuditListOptions{Actor: "bob", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(byActor) != 1 || byActor[0].FailureReason != "invalid_credentials" {
			t.Fatalf("actor filter wrong: %#v", byActor)
		}
		byProject, err := store.ListAudit(ctx, AuditListOptions{ProjectID: 7, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(byProject) != 1 || byProject[0].ProjectSlug != "alpha" {
			t.Fatalf("project filter wrong: %#v", byProject)
		}
		deleted, err := store.DeleteAuditBefore(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 4 {
			t.Fatalf("cleanup deleted %d, want 4", deleted)
		}
	})
}

func TestArgon2ConcurrencyLimiterRejectsWhenSaturated(t *testing.T) {
	argonLimiterMu.Lock()
	prev := defaultArgonLimiter
	custom := &argonLimiter{
		sem:     make(chan struct{}, 1),
		timeout: 30 * time.Millisecond,
	}
	defaultArgonLimiter = custom
	argonLimiterMu.Unlock()
	t.Cleanup(func() {
		argonLimiterMu.Lock()
		defaultArgonLimiter = prev
		argonLimiterMu.Unlock()
	})

	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the single verification slot.
	custom.sem <- struct{}{}
	defer func() { <-custom.sem }()

	if _, err := VerifyPasswordContext(
		context.Background(),
		hash,
		"correct-password",
	); !IsPasswordVerificationBusy(
		err,
	) {
		t.Fatalf("VerifyPasswordContext err = %v, want ErrPasswordVerificationBusy", err)
	}
	if _, err := HashPasswordContext(context.Background(), "another-password"); !IsPasswordVerificationBusy(err) {
		t.Fatalf("HashPasswordContext err = %v, want ErrPasswordVerificationBusy", err)
	}
}

func TestAuthenticateCredentialsPropagatesStoreError(t *testing.T) {
	store, err := OpenStore(context.Background(), t.TempDir()+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(context.Background(), "alice", "password-123", "member"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.AuthenticateCredentials(context.Background(), "alice", "password-123")
	if err == nil {
		t.Fatal("expected error from closed store")
	}
	if IsInvalidCredentials(err) {
		t.Fatalf("closed store error was misclassified as invalid credentials: %v", err)
	}
}

func TestLastAdminProtectionAndSessionRevocationInStore(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		firstAdmin, err := store.CreateUser(ctx, "admin-one", "password-123", "admin")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := store.CreateAccountSession(ctx, AccountSession{
			Token: "lss_admin-one-session", UserID: firstAdmin.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		// Demoting or deleting the sole admin must be rejected.
		if _, err := store.UpdateManagedUser(ctx, ManagedUserUpdate{
			ID: firstAdmin.ID, Role: "member",
		}); !IsLastAdminProtected(err) {
			t.Fatalf("demoting sole admin err = %v, want ErrLastAdminProtected", err)
		}
		if err := store.DeleteUser(ctx, firstAdmin.ID); !IsLastAdminProtected(err) {
			t.Fatalf("deleting sole admin err = %v, want ErrLastAdminProtected", err)
		}
		// Rejected demotion must not revoke the admin's session.
		if _, err := store.AuthenticateAccountSession(ctx, "lss_admin-one-session", now); err != nil {
			t.Fatalf("rejected demotion revoked session: %v", err)
		}

		// Adding a second admin allows demoting the first and revokes its sessions.
		secondAdmin, err := store.CreateUser(ctx, "admin-two", "password-123", "admin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateManagedUser(ctx, ManagedUserUpdate{
			ID: firstAdmin.ID, Role: "member",
		}); err != nil {
			t.Fatalf("demote first admin with second present: %v", err)
		}
		if _, err := store.AuthenticateAccountSession(ctx, "lss_admin-one-session", now); !IsNotFound(err) {
			t.Fatalf("demoted admin session still active: err=%v", err)
		}

		// Password reset via UpdateManagedUser revokes persisted sessions.
		if err := store.CreateAccountSession(ctx, AccountSession{
			Token: "lss_member-session", UserID: firstAdmin.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateManagedUser(ctx, ManagedUserUpdate{
			ID: firstAdmin.ID, Password: "new-password-456",
		}); err != nil {
			t.Fatalf("reset member password: %v", err)
		}
		if _, err := store.AuthenticateAccountSession(ctx, "lss_member-session", now); !IsNotFound(err) {
			t.Fatalf("session survived admin password reset: err=%v", err)
		}

		// The remaining sole admin is still protected from deletion.
		if err := store.DeleteUser(ctx, secondAdmin.ID); !IsLastAdminProtected(err) {
			t.Fatalf("deleting remaining sole admin err = %v, want ErrLastAdminProtected", err)
		}
	})
}
