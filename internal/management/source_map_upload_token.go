package management

import (
	"context"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
)

// Audit vocabulary for source map upload tokens (ADR 0018).
const (
	auditSourceMapUploadTokenCreate = "source_map_upload_token.create"
	auditSourceMapUploadTokenRevoke = "source_map_upload_token.revoke"
	auditSourceMapUploadTokenTarget = "source_map_upload_token"
)

// SourceMapUploadTokenView has the same non-secret shape as a project token view.
type SourceMapUploadTokenView = ProjectTokenView

type IssueSourceMapUploadTokenInput struct {
	ProjectID int
	Name      string
	Invalid   bool
}

type IssuedSourceMapUploadToken struct {
	Token SourceMapUploadTokenView
	Value string
}

func (m *Manager) ListSourceMapUploadTokens(ctx context.Context, projectID int) ([]SourceMapUploadTokenView, error) {
	if _, err := m.store.GetProject(ctx, projectID); err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return nil, managementErr
	}
	tokens, err := m.store.ListSourceMapUploadTokens(ctx, projectID)
	if err != nil {
		_, managementErr := failedFromMetadata(AuditFacts{}, err)
		return nil, managementErr
	}
	result := make([]SourceMapUploadTokenView, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, sourceMapUploadTokenView(token))
	}
	return result, nil
}

func (m *Manager) IssueSourceMapUploadToken(
	ctx context.Context,
	input IssueSourceMapUploadTokenInput,
) (IssuedSourceMapUploadToken, AuditFacts, error) {
	input.Name = strings.TrimSpace(input.Name)
	facts := AuditFacts{
		Action: auditSourceMapUploadTokenCreate, TargetType: auditSourceMapUploadTokenTarget,
		TargetName: input.Name, ProjectID: input.ProjectID,
	}
	project, err := m.store.GetProject(ctx, input.ProjectID)
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return IssuedSourceMapUploadToken{}, facts, managementErr
	}
	facts.ProjectSlug = project.Slug
	if input.Invalid {
		facts, managementErr := failed(facts, ErrorInvalidInput, "bad request")
		return IssuedSourceMapUploadToken{}, facts, managementErr
	}
	if input.Name == "" {
		facts, managementErr := failed(facts, ErrorInvalidInput, "source map upload token name is required")
		return IssuedSourceMapUploadToken{}, facts, managementErr
	}
	info, value, err := m.store.CreateSourceMapUploadToken(ctx, input.ProjectID, input.Name)
	if err != nil {
		facts, managementErr := failedFromMetadata(facts, err)
		return IssuedSourceMapUploadToken{}, facts, managementErr
	}
	view := sourceMapUploadTokenView(info)
	facts.TargetID = view.ID
	facts.TargetName = view.Name
	facts.Success = true
	return IssuedSourceMapUploadToken{Token: view, Value: value}, facts, nil
}

func (m *Manager) RevokeSourceMapUploadToken(ctx context.Context, projectID, tokenID int) (AuditFacts, error) {
	facts := AuditFacts{
		Action: auditSourceMapUploadTokenRevoke, TargetType: auditSourceMapUploadTokenTarget,
		TargetID: tokenID, ProjectID: projectID,
	}
	project, err := m.store.GetProject(ctx, projectID)
	if err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.ProjectSlug = project.Slug
	if tokenID <= 0 {
		return failedWithReason(facts, ErrorInvalidInput, "bad request", "invalid_target_id")
	}
	if err := m.store.RevokeSourceMapUploadTokenByID(ctx, projectID, tokenID); err != nil {
		return failedFromMetadata(facts, err)
	}
	facts.Success = true
	return facts, nil
}

func sourceMapUploadTokenView(token meta.SourceMapUploadTokenInfo) SourceMapUploadTokenView {
	return SourceMapUploadTokenView{
		ID: token.ID, Name: token.Name, Prefix: token.Prefix, Revoked: token.Revoked,
		CreatedAt: token.CreatedAt, LastUsedAt: token.LastUsedAt,
	}
}
