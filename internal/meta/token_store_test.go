package meta

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNamedProjectAPITokenLifecycle(t *testing.T) {
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "token.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Token project", "token-project")
	if err != nil {
		t.Fatal(err)
	}
	info, value, err := store.CreateNamedProjectAPIToken(ctx, project.ID, "deploy-agent")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "deploy-agent" || info.Prefix == "" || info.Revoked || info.LastUsedAt != nil ||
		len(value) <= len(info.Prefix) ||
		value[:len(info.Prefix)] != info.Prefix {
		t.Fatalf("info=%#v value=%q", info, value)
	}
	if _, err := store.AuthenticateProjectAPIToken(ctx, value); err != nil {
		t.Fatal(err)
	}
	tokens, err := store.ListProjectAPITokens(ctx, project.ID)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == nil {
		t.Fatalf("tokens=%#v err=%v", tokens, err)
	}
	firstUsedAt := *tokens[0].LastUsedAt
	if _, err := store.AuthenticateProjectAPIToken(ctx, value); err != nil {
		t.Fatal(err)
	}
	tokens, err = store.ListProjectAPITokens(ctx, project.ID)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == nil || !tokens[0].LastUsedAt.Equal(firstUsedAt) {
		t.Fatalf("last_used_at changed inside throttle window: tokens=%#v err=%v first=%s", tokens, err, firstUsedAt)
	}
	staleUsedAt := time.Now().UTC().Add(-2 * apiTokenLastUsedInterval)
	if _, err := store.client.ProjectAPIToken.UpdateOneID(info.ID).SetLastUsedAt(staleUsedAt).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateProjectAPIToken(ctx, value); err != nil {
		t.Fatal(err)
	}
	tokens, err = store.ListProjectAPITokens(ctx, project.ID)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == nil || !tokens[0].LastUsedAt.After(staleUsedAt) {
		t.Fatalf("stale last_used_at was not refreshed: tokens=%#v err=%v stale=%s", tokens, err, staleUsedAt)
	}
	if err := store.RevokeProjectAPITokenByID(ctx, project.ID, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateProjectAPIToken(ctx, value); err == nil {
		t.Fatal("revoked token authenticated")
	}
	if err := store.DeleteProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProject(ctx, project.ID); !IsNotFound(err) {
		t.Fatalf("deleted project lookup err=%v", err)
	}
}

func TestUserAPITokenLifecycle(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		u, err := store.CreateUser(ctx, "token-user", "password", "member")
		if err != nil {
			t.Fatal(err)
		}
		info, value, err := store.CreateUserAPIToken(ctx, u.ID, "local-cli")
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != "local-cli" || !strings.HasPrefix(value, "lou_") || info.Prefix == "" || info.Revoked ||
			info.LastUsedAt != nil ||
			!strings.HasPrefix(value, info.Prefix) {
			t.Fatalf("info=%#v value=%q", info, value)
		}
		authenticated, err := store.AuthenticateUserAPIToken(ctx, value)
		if err != nil || authenticated.ID != u.ID || authenticated.Username != u.Username ||
			authenticated.Role != u.Role {
			t.Fatalf("authenticated=%#v err=%v", authenticated, err)
		}
		tokens, err := store.ListUserAPITokens(ctx, u.ID)
		if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == nil {
			t.Fatalf("tokens=%#v err=%v", tokens, err)
		}
		if err := store.RevokeUserAPITokenByID(ctx, u.ID, info.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AuthenticateUserAPIToken(ctx, value); !errors.Is(err, ErrInvalidUserAPIToken) {
			t.Fatalf("revoked authentication err=%v", err)
		}
		if err := store.DeleteUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	})
}
