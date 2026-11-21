package management

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
)

func testManager(t *testing.T) (*Manager, *meta.Store) {
	t.Helper()
	store, err := meta.OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store), store
}

func TestUserManagementDeleteReturnsStableNotFound(t *testing.T) {
	manager, _ := testManager(t)
	facts, err := manager.DeleteUser(context.Background(), 404)
	if !IsErrorKind(err, ErrorNotFound) {
		t.Fatalf("DeleteUser error = %v, want stable not found", err)
	}
	if facts.Action != "user.delete" || facts.TargetType != "user" || facts.TargetID != 404 ||
		facts.Success || facts.FailureReason != "not_found" {
		t.Fatalf("unexpected audit facts: %#v", facts)
	}
}

func TestProjectManagementProtectsSelfMonitoringAndImmutableSlug(t *testing.T) {
	manager, store := testManager(t)
	ctx := context.Background()
	project, _, err := manager.CreateProject(ctx, CreateProjectInput{Name: "Managed Project", Slug: "managed-project"})
	if err != nil {
		t.Fatal(err)
	}
	_, facts, err := manager.UpdateProject(ctx, UpdateProjectInput{ID: project.ID, Slug: "renamed"})
	if !IsErrorKind(err, ErrorConflict) || facts.FailureReason != "immutable_slug" {
		t.Fatalf("immutable slug error = %v facts=%#v", err, facts)
	}
	unchanged, err := manager.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Slug != "managed-project" {
		t.Fatalf("immutable slug changed to %q", unchanged.Slug)
	}

	self, _, err := store.BootstrapSelfMonitoring(ctx, "lo_test-self-monitoring-derived-token-value")
	if err != nil {
		t.Fatal(err)
	}
	_, facts, err = manager.UpdateProject(ctx, UpdateProjectInput{ID: self.ID, Name: "Renamed Self"})
	if !IsErrorKind(err, ErrorProtected) || facts.FailureReason != "protected_project" {
		t.Fatalf("protected update error = %v facts=%#v", err, facts)
	}
	facts, err = manager.DeleteProject(ctx, self.ID)
	if !IsErrorKind(err, ErrorProtected) || facts.FailureReason != "protected_project" {
		t.Fatalf("protected delete error = %v facts=%#v", err, facts)
	}
}

func TestManagementValidationRulesAreConsistentAcrossEntryPoints(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()

	// CreateUser enforces username, password length, and role.
	if _, _, err := manager.CreateUser(ctx, CreateUserInput{Password: "short", Role: "member"}); !IsErrorKind(
		err,
		ErrorInvalidInput,
	) {
		t.Fatalf("short password: err = %v, want invalid input", err)
	}
	if _, _, err := manager.CreateUser(
		ctx,
		CreateUserInput{Username: "ok", Password: "long-enough", Role: "superuser"},
	); !IsErrorKind(
		err,
		ErrorInvalidInput,
	) {
		t.Fatalf("invalid role: err = %v, want invalid input", err)
	}
	if _, _, err := manager.CreateUser(ctx, CreateUserInput{Password: "long-enough", Role: "member"}); !IsErrorKind(
		err,
		ErrorInvalidInput,
	) {
		t.Fatalf("empty username: err = %v, want invalid input", err)
	}

	// UpdateUser enforces the same rules for the fields it touches.
	created, _, err := manager.CreateUser(
		ctx,
		CreateUserInput{Username: "validate-me", Password: "long-enough", Role: "member"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateUser(ctx, UpdateUserInput{ID: created.ID, Role: "owner"}); !IsErrorKind(
		err,
		ErrorInvalidInput,
	) {
		t.Fatalf("update invalid role: err = %v, want invalid input", err)
	}
	if _, _, err := manager.UpdateUser(ctx, UpdateUserInput{ID: created.ID, Password: "short"}); !IsErrorKind(
		err,
		ErrorInvalidInput,
	) {
		t.Fatalf("update short password: err = %v, want invalid input", err)
	}

	// IssueProjectToken enforces a non-empty token name.
	project, _, err := manager.CreateProject(ctx, CreateProjectInput{Name: "Validate", Slug: "validate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.IssueProjectToken(ctx, IssueProjectTokenInput{ProjectID: project.ID}); !IsErrorKind(
		err,
		ErrorInvalidInput,
	) {
		t.Fatalf("empty token name: err = %v, want invalid input", err)
	}
}

func TestProjectTokenManagementProtectsSecretAndProjectOwnership(t *testing.T) {
	manager, store := testManager(t)
	ctx := context.Background()
	alpha, _, err := manager.CreateProject(ctx, CreateProjectInput{Name: "Alpha", Slug: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	beta, _, err := manager.CreateProject(ctx, CreateProjectInput{Name: "Beta", Slug: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	issued, _, err := manager.IssueProjectToken(ctx, IssueProjectTokenInput{ProjectID: alpha.ID, Name: "collector"})
	if err != nil {
		t.Fatal(err)
	}
	if issued.Value == "" || issued.Token.Prefix == "" {
		t.Fatalf("issued token omitted one-time secret or prefix: %#v", issued)
	}
	tokens, err := manager.ListProjectTokens(ctx, alpha.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].ID != issued.Token.ID || tokens[0].Prefix != issued.Token.Prefix {
		t.Fatalf("unexpected token list: %#v", tokens)
	}
	if facts, err := manager.RevokeProjectToken(ctx, beta.ID, issued.Token.ID); !IsErrorKind(err, ErrorNotFound) ||
		facts.FailureReason != "not_found" {
		t.Fatalf("cross-project revoke error = %v facts=%#v", err, facts)
	}

	self, _, err := store.BootstrapSelfMonitoring(ctx, "lo_test-self-monitoring-derived-token-value")
	if err != nil {
		t.Fatal(err)
	}
	if _, facts, err := manager.IssueProjectToken(
		ctx,
		IssueProjectTokenInput{ProjectID: self.ID, Name: "forbidden"},
	); !IsErrorKind(err, ErrorProtected) || facts.FailureReason != "protected_project" {
		t.Fatalf("protected issue error = %v facts=%#v", err, facts)
	}
	if facts, err := manager.RevokeProjectToken(ctx, self.ID, issued.Token.ID); !IsErrorKind(err, ErrorProtected) ||
		facts.FailureReason != "protected_project" {
		t.Fatalf("protected revoke error = %v facts=%#v", err, facts)
	}
}

func TestCreateProjectValidatesSlugPattern(t *testing.T) {
	manager, _ := testManager(t)
	ctx := context.Background()

	invalidSlugs := []string{
		"team/app",
		"team app",
		"UpperCase",
		"-leading",
		"_leading",
		"a.b",
		"项目",
		strings.Repeat("a", 64),
	}
	for _, slug := range invalidSlugs {
		if _, facts, err := manager.CreateProject(ctx, CreateProjectInput{Name: "Bad", Slug: slug}); !IsErrorKind(
			err,
			ErrorInvalidInput,
		) || facts.Success {
			t.Fatalf("slug %q: err=%v facts=%+v, want invalid_input", slug, err, facts)
		}
	}

	validSlugs := []string{
		"a",
		"team-1_app",
		strings.Repeat("z", 63),
	}
	for _, slug := range validSlugs {
		got, facts, err := manager.CreateProject(ctx, CreateProjectInput{Name: "Good " + slug, Slug: slug})
		if err != nil || !facts.Success || got.Slug != slug {
			t.Fatalf("slug %q: got=%+v facts=%+v err=%v", slug, got, facts, err)
		}
	}
}

func TestUserManagementRevokesSessionsAndProtectsLastAdmin(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	manager, store := testManager(t)
	shared := state.NewMemorySessionStore()
	sessions := account.NewStateSessions(shared)
	manager.WithSessions(sessions)

	admin, _, err := manager.CreateUser(ctx, CreateUserInput{
		Username: "sole-admin", Password: "password-123", Role: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	adminTok, err := sessions.Create(ctx, admin.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if _, facts, err := manager.UpdateUser(ctx, UpdateUserInput{ID: admin.ID, Role: "member"}); !IsErrorKind(
		err,
		ErrorProtected,
	) || facts.FailureReason != "protected_last_admin" {
		t.Fatalf("demote sole admin err=%v facts=%#v", err, facts)
	}
	if facts, err := manager.DeleteUser(ctx, admin.ID); !IsErrorKind(err, ErrorProtected) ||
		facts.FailureReason != "protected_last_admin" {
		t.Fatalf("delete sole admin err=%v facts=%#v", err, facts)
	}
	if _, err := sessions.Authenticate(ctx, adminTok, now); err != nil {
		t.Fatalf("protected sole admin session unexpectedly revoked: %v", err)
	}

	member, _, err := manager.CreateUser(ctx, CreateUserInput{
		Username: "alice", Password: "password-123", Role: "member",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok1, err := sessions.Create(ctx, member.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateUser(
		ctx,
		UpdateUserInput{ID: member.ID, Password: "reset-password-456"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Authenticate(ctx, tok1, now); !errors.Is(err, account.ErrSessionNotFound) {
		t.Fatalf("session survived admin password reset: err=%v", err)
	}

	tok2, err := sessions.Create(ctx, member.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateUser(ctx, UpdateUserInput{ID: member.ID, Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Authenticate(ctx, tok2, now); !errors.Is(err, account.ErrSessionNotFound) {
		t.Fatalf("session survived role downgrade: err=%v", err)
	}

	tok3, err := sessions.Create(ctx, member.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DeleteUser(ctx, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Authenticate(ctx, tok3, now); !errors.Is(err, account.ErrSessionNotFound) {
		t.Fatalf("session survived user delete: err=%v", err)
	}
	_ = store
}

type failingSessionRevoker struct{ err error }

func (f failingSessionRevoker) RevokeOthers(context.Context, int, string) error { return f.err }

func TestUpdateAndDeleteUserReportSessionRevocationFailure(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	manager.WithSessions(failingSessionRevoker{err: errors.New("redis down")})

	if _, _, err := manager.CreateUser(ctx, CreateUserInput{
		Username: "admin-rev", Password: "password-123", Role: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	member, _, err := manager.CreateUser(ctx, CreateUserInput{
		Username: "member-rev", Password: "password-123", Role: "member",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, facts, err := manager.UpdateUser(
		ctx,
		UpdateUserInput{ID: member.ID, Password: "new-password-456"},
	); !IsErrorKind(err, ErrorUnavailable) || facts.FailureReason != "session_revoke_failed" {
		t.Fatalf("password reset revoke failure: err=%v facts=%#v", err, facts)
	}

	if _, facts, err := manager.UpdateUser(
		ctx,
		UpdateUserInput{ID: member.ID, Role: "viewer"},
	); !IsErrorKind(err, ErrorUnavailable) || facts.FailureReason != "session_revoke_failed" {
		t.Fatalf("role downgrade revoke failure: err=%v facts=%#v", err, facts)
	}

	if facts, err := manager.DeleteUser(ctx, member.ID); !IsErrorKind(err, ErrorUnavailable) ||
		facts.FailureReason != "session_revoke_failed" {
		t.Fatalf("delete user revoke failure: err=%v facts=%#v", err, facts)
	}
}
