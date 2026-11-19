package meta

import (
	"context"
	"errors"
	"strings"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/sourcemapuploadtoken"
)

// SourceMapUploadTokenPrefix starts every source map upload token (ADR 0018). The
// gateway uses it to tell the credential kinds apart before any lookup.
const SourceMapUploadTokenPrefix = "los_"

const sourceMapUploadTokenPrefixLength = 12

// ErrInvalidSourceMapUploadToken distinguishes an unknown or revoked upload token
// from a transient metadata-store failure so gateways can return 401 vs 503.
var ErrInvalidSourceMapUploadToken = errors.New("invalid source map upload token")

type SourceMapUploadTokenInfo struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Revoked    bool       `json:"revoked"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// CreateSourceMapUploadToken issues an upload token for a project. The secret is
// returned only to the caller; it is stored as a SHA-256 digest like every other
// bearer credential, and later listings expose only the non-secret prefix.
func (s *Store) CreateSourceMapUploadToken(
	ctx context.Context,
	projectID int,
	name string,
) (SourceMapUploadTokenInfo, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return SourceMapUploadTokenInfo{}, "", errors.New("source map upload token name is required")
	}
	token, err := randomOpaqueToken(SourceMapUploadTokenPrefix, 24)
	if err != nil {
		return SourceMapUploadTokenInfo{}, "", err
	}
	created, err := s.client.SourceMapUploadToken.Create().
		SetToken(storedCredential(token)).
		SetName(name).
		SetPrefix(token[:sourceMapUploadTokenPrefixLength]).
		SetProjectID(projectID).
		Save(ctx)
	if err != nil {
		return SourceMapUploadTokenInfo{}, "", err
	}
	s.revision.Add(1)
	return sourceMapUploadTokenInfo(created), token, nil
}

// AuthenticateSourceMapUploadToken resolves a live upload token to its project.
func (s *Store) AuthenticateSourceMapUploadToken(ctx context.Context, token string) (ProjectInfo, error) {
	t, err := s.client.SourceMapUploadToken.Query().
		Where(sourcemapuploadtoken.TokenEQ(storedCredential(token)), sourcemapuploadtoken.RevokedEQ(false)).
		WithProject().
		Only(ctx)
	if err != nil {
		if IsNotFound(err) {
			return ProjectInfo{}, ErrInvalidSourceMapUploadToken
		}
		return ProjectInfo{}, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-apiTokenLastUsedInterval)
	if t.LastUsedAt == nil || t.LastUsedAt.Before(cutoff) {
		// Usage metadata must never turn a valid token into an API failure; the
		// conditional update keeps concurrent uploads from queueing SQLite writes.
		_, _ = s.client.SourceMapUploadToken.Update().
			Where(
				sourcemapuploadtoken.IDEQ(t.ID),
				sourcemapuploadtoken.Or(
					sourcemapuploadtoken.LastUsedAtIsNil(),
					sourcemapuploadtoken.LastUsedAtLT(cutoff),
				),
			).
			SetLastUsedAt(now).
			Save(ctx)
	}
	p, err := t.Edges.ProjectOrErr()
	if err != nil {
		return ProjectInfo{}, err
	}
	return ProjectInfo{ID: p.ID, Name: p.Name, Slug: p.Slug}, nil
}

func (s *Store) ListSourceMapUploadTokens(ctx context.Context, projectID int) ([]SourceMapUploadTokenInfo, error) {
	tokens, err := s.client.SourceMapUploadToken.Query().
		Where(sourcemapuploadtoken.ProjectIDEQ(projectID)).
		Order(orderAsc(sourcemapuploadtoken.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]SourceMapUploadTokenInfo, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, sourceMapUploadTokenInfo(token))
	}
	return result, nil
}

// RevokeSourceMapUploadTokenByID revokes one token of the given project. A token
// id from another project is reported as not found.
func (s *Store) RevokeSourceMapUploadTokenByID(ctx context.Context, projectID, tokenID int) error {
	n, err := s.client.SourceMapUploadToken.Update().
		Where(sourcemapuploadtoken.IDEQ(tokenID), sourcemapuploadtoken.ProjectIDEQ(projectID)).
		SetRevoked(true).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{sourcemapuploadtoken.Label}
	}
	s.revision.Add(1)
	return nil
}

func sourceMapUploadTokenInfo(token *dbent.SourceMapUploadToken) SourceMapUploadTokenInfo {
	return SourceMapUploadTokenInfo{
		ID:         token.ID,
		Name:       token.Name,
		Prefix:     token.Prefix,
		Revoked:    token.Revoked,
		CreatedAt:  token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
	}
}
