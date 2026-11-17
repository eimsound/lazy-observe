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
