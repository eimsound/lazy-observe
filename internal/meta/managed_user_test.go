package meta

import (
	"context"
	"testing"
)

func TestCreateManagedUserIsAtomic(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		existing, err := store.CreateUser(ctx, "managed-email-owner", "password", "member")
		if err != nil {
			t.Fatal(err)
		}
		email := "managed-occupied@example.com"
		if _, err := store.UpdateUserProfile(ctx, existing.ID, nil, &email); err != nil {
			t.Fatal(err)
		}

		_, err = store.CreateManagedUser(ctx, ManagedUserCreate{
			Username: "managed-must-roll-back",
			Password: "password",
			Role:     "member",
			Email:    email,
		})
		if !IsConstraintError(err) {
			t.Fatalf("CreateManagedUser error = %v, want constraint error", err)
		}

		users, err := store.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, user := range users {
			if user.Username == "managed-must-roll-back" {
				t.Fatalf("failed create left user behind: %#v", user)
			}
		}
	})
}

func TestUpdateManagedUserIsAtomic(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		target, err := store.CreateManagedUser(ctx, ManagedUserCreate{
			Username: "managed-before-update", Password: "password", Role: "member",
			DisplayName: "Managed Before", Email: "managed-before@example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.CreateManagedUser(ctx, ManagedUserCreate{
			Username: "managed-update-owner", Password: "password", Role: "member",
			Email: "managed-update-occupied@example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		displayName, occupiedEmail := "Managed After", "managed-update-occupied@example.com"

		_, err = store.UpdateManagedUser(ctx, ManagedUserUpdate{
			ID: target.ID, Username: "managed-after-update", Role: "viewer",
			DisplayName: &displayName, Email: &occupiedEmail,
		})
		if !IsConstraintError(err) {
			t.Fatalf("UpdateManagedUser error = %v, want constraint error", err)
		}

		updated, err := store.GetUser(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Username != "managed-before-update" || updated.Role != "member" ||
			updated.DisplayName != "Managed Before" || updated.Email != "managed-before@example.com" {
			t.Fatalf("failed update changed user: %#v", updated)
		}
	})
}
