package management

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
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
