// Package management owns administrator-facing project, user, and project API
// token behavior. Transport details stay in gateway; persistence details stay
// in meta.
package management

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
)

const minimumPasswordLength = 8

type ErrorKind string

const (
	ErrorInvalidInput ErrorKind = "invalid_input"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorConflict     ErrorKind = "conflict"
	ErrorProtected    ErrorKind = "protected_resource"
	ErrorUnavailable  ErrorKind = "unavailable"
)

// Error exposes a stable classification and safe client message without
// leaking persistence errors.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return e.Message }

func IsErrorKind(err error, kind ErrorKind) bool {
	var managementError *Error
	return errors.As(err, &managementError) && managementError.Kind == kind
}

type AuditFacts struct {
	Action        string
	TargetType    string
	TargetID      int
	TargetName    string
	TargetRole    string
	ProjectID     int
	ProjectSlug   string
	ChangedFields []string
	Success       bool
	FailureReason string
}

type CreateUserInput struct {
	Username    string
	Password    string
	Role        string
	DisplayName string
	Email       string
}

type UpdateUserInput struct {
	ID          int
	Username    string
	Password    string
	Role        string
	DisplayName *string
	Email       *string
}

type ProjectView struct {
	ID        int
	Name      string
	Slug      string
	CreatedAt time.Time
}

type CreateProjectInput struct {
	Name string
	Slug string
}

type UpdateProjectInput struct {
	ID      int
	Name    string
	Slug    string
	Invalid bool
}

type ProjectTokenView struct {
	ID         int
	Name       string
	Prefix     string
	Revoked    bool
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

type IssueProjectTokenInput struct {
	ProjectID int
	Name      string
	Invalid   bool
}

type IssuedProjectToken struct {
	Token ProjectTokenView
	Value string
}

type Manager struct {
	store *meta.Store
}

func New(store *meta.Store) *Manager {
	if store == nil {
		panic("management: metadata store is required")
	}
	return &Manager{store: store}
}

func (m *Manager) CreateUser(ctx context.Context, input CreateUserInput) (meta.UserInfo, AuditFacts, error) {
	input.Username = strings.TrimSpace(input.Username)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Email = strings.TrimSpace(input.Email)
	facts := AuditFacts{
		Action:     "user.create",
		TargetType: "user",
		TargetName: input.Username,
		TargetRole: input.Role,
	}
	if input.Username == "" || len(input.Password) < minimumPasswordLength || !validRole(input.Role) {
		facts, err := failed(facts, ErrorInvalidInput, "bad request")
		return meta.UserInfo{}, facts, err
	}

	created, err := m.store.CreateManagedUser(ctx, meta.ManagedUserCreate{
		Username:    input.Username,
		Password:    input.Password,
		Role:        input.Role,
		DisplayName: input.DisplayName,
		Email:       input.Email,
	})
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return meta.UserInfo{}, facts, managementErr
	}
	facts.TargetID = created.ID
	facts.TargetName = created.Username
	facts.TargetRole = created.Role
	facts.Success = true
	return created, facts, nil
}

func (m *Manager) UpdateUser(ctx context.Context, input UpdateUserInput) (meta.UserInfo, AuditFacts, error) {
	rawUsername := input.Username
	input.Username = strings.TrimSpace(input.Username)
	input.DisplayName = trimmed(input.DisplayName)
	input.Email = trimmed(input.Email)
	facts := AuditFacts{
		Action:        "user.update",
		TargetType:    "user",
		TargetID:      input.ID,
		TargetName:    input.Username,
		TargetRole:    input.Role,
		ChangedFields: userChangedFields(rawUsername, input),
	}
	if input.ID <= 0 ||
		(rawUsername == "" && input.Password == "" && input.Role == "" && input.DisplayName == nil && input.Email == nil) ||
		(input.Role != "" && !validRole(input.Role)) ||
		(input.Password != "" && len(input.Password) < minimumPasswordLength) {
		facts, err := failed(facts, ErrorInvalidInput, "bad request")
		return meta.UserInfo{}, facts, err
	}

	updated, err := m.store.UpdateManagedUser(ctx, meta.ManagedUserUpdate{
		ID: input.ID, Username: input.Username, Password: input.Password, Role: input.Role,
		DisplayName: input.DisplayName, Email: input.Email,
	})
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return meta.UserInfo{}, facts, managementErr
	}
	facts.TargetName = updated.Username
	facts.TargetRole = updated.Role
	facts.Success = true
	return updated, facts, nil
}

func (m *Manager) ListUsers(ctx context.Context) ([]meta.UserInfo, error) {
	users, err := m.store.ListUsers(ctx)
	if err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return nil, managementErr
	}
	return users, nil
}

func (m *Manager) GetUser(ctx context.Context, id int) (meta.UserInfo, error) {
	user, err := m.store.GetUser(ctx, id)
	if err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return meta.UserInfo{}, managementErr
	}
	return user, nil
}

func (m *Manager) DeleteUser(ctx context.Context, id int) (AuditFacts, error) {
	facts := AuditFacts{Action: "user.delete", TargetType: "user", TargetID: id}
	if id <= 0 {
		return failed(facts, ErrorInvalidInput, "bad request")
	}
	if err := m.store.DeleteUser(ctx, id); err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.Success = true
	return facts, nil
}

func (m *Manager) ListProjects(ctx context.Context) ([]ProjectView, error) {
	projects, err := m.store.ListProjects(ctx)
	if err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return nil, managementErr
	}
	result := make([]ProjectView, 0, len(projects))
	for _, project := range projects {
		result = append(result, projectView(project))
	}
	return result, nil
}

func (m *Manager) GetProject(ctx context.Context, id int) (ProjectView, error) {
	project, err := m.store.GetProject(ctx, id)
	if err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return ProjectView{}, managementErr
	}
	return projectView(project), nil
}

func (m *Manager) CreateProject(
	ctx context.Context,
	input CreateProjectInput,
) (ProjectView, AuditFacts, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Slug = strings.TrimSpace(input.Slug)
	facts := AuditFacts{
		Action: "project.create", TargetType: "project", TargetName: input.Name, ProjectSlug: input.Slug,
	}
	if input.Name == "" || input.Slug == "" {
		facts, err := failed(facts, ErrorInvalidInput, "bad request")
		return ProjectView{}, facts, err
	}
	created, err := m.store.CreateProject(ctx, input.Name, input.Slug)
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return ProjectView{}, facts, managementErr
	}
	view := projectView(created)
	facts.TargetID = view.ID
	facts.TargetName = view.Name
	facts.ProjectID = view.ID
	facts.ProjectSlug = view.Slug
	facts.Success = true
	return view, facts, nil
}

func (m *Manager) UpdateProject(
	ctx context.Context,
	input UpdateProjectInput,
) (ProjectView, AuditFacts, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Slug = strings.TrimSpace(input.Slug)
	facts := AuditFacts{Action: "project.update", TargetType: "project", TargetID: input.ID, ProjectID: input.ID}
	existing, err := m.store.GetProject(ctx, input.ID)
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return ProjectView{}, facts, managementErr
	}
	facts.TargetName = existing.Name
	facts.ProjectSlug = existing.Slug
	if existing.Slug == meta.SelfMonitoringProjectSlug {
		facts, managementErr := failedWithReason(
			facts,
			ErrorProtected,
			"the built-in self-monitoring project is immutable",
			"protected_project",
		)
		return ProjectView{}, facts, managementErr
	}
	if input.Invalid {
		facts, managementErr := failed(facts, ErrorInvalidInput, "bad request")
		return ProjectView{}, facts, managementErr
	}
	if input.Name == "" && input.Slug == "" {
		facts, managementErr := failed(facts, ErrorInvalidInput, "bad request")
		return ProjectView{}, facts, managementErr
	}
	if input.Slug != "" && input.Slug != existing.Slug {
		facts.ChangedFields = []string{"slug"}
		facts, managementErr := failedWithReason(
			facts,
			ErrorConflict,
			"project slug is immutable",
			"immutable_slug",
		)
		return ProjectView{}, facts, managementErr
	}
	if input.Name != "" {
		facts.ChangedFields = []string{"name"}
	}
	updated, err := m.store.UpdateProject(ctx, input.ID, input.Name, "")
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return ProjectView{}, facts, managementErr
	}
	view := projectView(updated)
	facts.TargetName = view.Name
	facts.ProjectSlug = view.Slug
	facts.Success = true
	return view, facts, nil
}

func (m *Manager) DeleteProject(ctx context.Context, id int) (AuditFacts, error) {
	facts := AuditFacts{Action: "project.delete", TargetType: "project", TargetID: id, ProjectID: id}
	project, err := m.store.GetProject(ctx, id)
	if err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.TargetName = project.Name
	facts.ProjectSlug = project.Slug
	if project.Slug == meta.SelfMonitoringProjectSlug {
		return failedWithReason(
			facts,
			ErrorProtected,
			"the built-in self-monitoring project is immutable",
			"protected_project",
		)
	}
	if err := m.store.DeleteProject(ctx, id); err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.Success = true
	return facts, nil
}

func projectView(project *meta.Project) ProjectView {
	return ProjectView{ID: project.ID, Name: project.Name, Slug: project.Slug, CreatedAt: project.CreatedAt}
}

func (m *Manager) ListProjectTokens(ctx context.Context, projectID int) ([]ProjectTokenView, error) {
	tokens, err := m.store.ListProjectAPITokens(ctx, projectID)
	if err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return nil, managementErr
	}
	result := make([]ProjectTokenView, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, projectTokenView(token))
	}
	return result, nil
}

func (m *Manager) IssueProjectToken(
	ctx context.Context,
	input IssueProjectTokenInput,
) (IssuedProjectToken, AuditFacts, error) {
	input.Name = strings.TrimSpace(input.Name)
	facts := AuditFacts{
		Action: "project_api_token.create", TargetType: "project_api_token", TargetName: input.Name,
		ProjectID: input.ProjectID,
	}
	project, err := m.store.GetProject(ctx, input.ProjectID)
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return IssuedProjectToken{}, facts, managementErr
	}
	facts.ProjectSlug = project.Slug
	if project.Slug == meta.SelfMonitoringProjectSlug {
		facts, managementErr := failedWithReason(
			facts,
			ErrorProtected,
			"self-monitoring tokens are managed internally",
			"protected_project",
		)
		return IssuedProjectToken{}, facts, managementErr
	}
	if input.Invalid {
		facts, managementErr := failed(facts, ErrorInvalidInput, "bad request")
		return IssuedProjectToken{}, facts, managementErr
	}
	if input.Name == "" {
		facts, managementErr := failed(facts, ErrorInvalidInput, "project API token name is required")
		return IssuedProjectToken{}, facts, managementErr
	}
	info, value, err := m.store.CreateNamedProjectAPIToken(ctx, input.ProjectID, input.Name)
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return IssuedProjectToken{}, facts, managementErr
	}
	view := projectTokenView(info)
	facts.TargetID = view.ID
	facts.TargetName = view.Name
	facts.Success = true
	return IssuedProjectToken{Token: view, Value: value}, facts, nil
}

func (m *Manager) RevokeProjectToken(ctx context.Context, projectID, tokenID int) (AuditFacts, error) {
	facts := AuditFacts{
		Action: "project_api_token.revoke", TargetType: "project_api_token", TargetID: tokenID, ProjectID: projectID,
	}
	project, err := m.store.GetProject(ctx, projectID)
	if err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.ProjectSlug = project.Slug
	if project.Slug == meta.SelfMonitoringProjectSlug {
		return failedWithReason(
			facts,
			ErrorProtected,
			"self-monitoring tokens are managed internally",
			"protected_project",
		)
	}
	if tokenID <= 0 {
		return failedWithReason(facts, ErrorInvalidInput, "bad request", "invalid_target_id")
	}
	if err := m.store.RevokeProjectAPITokenByID(ctx, projectID, tokenID); err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.Success = true
	return facts, nil
}

func projectTokenView(token meta.ProjectAPITokenInfo) ProjectTokenView {
	return ProjectTokenView{
		ID: token.ID, Name: token.Name, Prefix: token.Prefix, Revoked: token.Revoked,
		CreatedAt: token.CreatedAt, LastUsedAt: token.LastUsedAt,
	}
}

func trimmed(value *string) *string {
	if value == nil {
		return nil
	}
	trimmedValue := strings.TrimSpace(*value)
	return &trimmedValue
}

func userChangedFields(rawUsername string, input UpdateUserInput) []string {
	changed := make([]string, 0, 5)
	if rawUsername != "" {
		changed = append(changed, "username")
	}
	if input.Password != "" {
		changed = append(changed, "password")
	}
	if input.Role != "" {
		changed = append(changed, "role")
	}
	if input.DisplayName != nil {
		changed = append(changed, "display_name")
	}
	if input.Email != nil {
		changed = append(changed, "email")
	}
	return changed
}

func failed(facts AuditFacts, kind ErrorKind, message string) (AuditFacts, error) {
	return failedWithReason(facts, kind, message, failureReason(kind))
}

func failedWithReason(facts AuditFacts, kind ErrorKind, message, reason string) (AuditFacts, error) {
	facts.FailureReason = reason
	return facts, &Error{Kind: kind, Message: message}
}

func failedFromMetadata(facts AuditFacts, err error) (AuditFacts, error) {
	switch {
	case meta.IsNotFound(err):
		return failed(facts, ErrorNotFound, "not found")
	case meta.IsConstraintError(err):
		return failed(facts, ErrorConflict, "conflict")
	default:
		return failed(facts, ErrorUnavailable, "internal error")
	}
}

func failureReason(kind ErrorKind) string {
	switch kind {
	case ErrorInvalidInput:
		return "invalid_request"
	case ErrorNotFound:
		return "not_found"
	case ErrorConflict:
		return "conflict"
	case ErrorProtected:
		return "protected_resource"
	default:
		return "internal_error"
	}
}

func validRole(role string) bool { return role == "admin" || role == "member" || role == "viewer" }
