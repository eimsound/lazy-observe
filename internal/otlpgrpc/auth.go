package otlpgrpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	userTokenPrefix    = "lou_"
	projectTokenPrefix = "lo_"
)

// extractBearerToken parses the gRPC incoming "authorization" metadata header
// (case-insensitive "Bearer <token>"). Ambiguous multiple values are rejected.
func extractBearerToken(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get("authorization")
	if len(values) != 1 {
		return "", false
	}
	raw := strings.TrimSpace(values[0])
	scheme, token, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

func authenticateContext(
	ctx context.Context,
	auth TokenAuthenticator,
	logger *slog.Logger,
) (meta.ProjectInfo, error) {
	token, ok := extractBearerToken(ctx)
	if !ok {
		return meta.ProjectInfo{}, status.Error(codes.Unauthenticated, "missing or invalid authorization metadata")
	}

	// Enforce ADR 0010 (project ingest tokens only) and ADR 0018 (upload tokens
	// cannot ingest telemetry) before consulting the project token store.
	switch {
	case strings.HasPrefix(token, userTokenPrefix):
		return meta.ProjectInfo{}, status.Error(
			codes.PermissionDenied,
			"user API tokens cannot ingest telemetry; use a project API token",
		)
	case strings.HasPrefix(token, meta.SourceMapUploadTokenPrefix):
		return meta.ProjectInfo{}, status.Error(
			codes.PermissionDenied,
			"upload_only_token: source map upload tokens can only upload source maps",
		)
	case !strings.HasPrefix(token, projectTokenPrefix):
		return meta.ProjectInfo{}, status.Error(codes.Unauthenticated, "invalid project API token")
	}

	project, err := auth.AuthenticateProjectToken(ctx, token)
	if err != nil {
		if errors.Is(err, meta.ErrInvalidProjectAPIToken) || meta.IsNotFound(err) {
			return meta.ProjectInfo{}, status.Error(codes.Unauthenticated, "invalid project API token")
		}
		if errors.Is(err, context.Canceled) {
			return meta.ProjectInfo{}, status.Error(codes.Canceled, "request canceled")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return meta.ProjectInfo{}, status.Error(codes.DeadlineExceeded, "authentication timed out")
		}
		if logger != nil {
			logger.Warn("otlp grpc token authentication unavailable", "error", err)
		}
		return meta.ProjectInfo{}, status.Error(codes.Unavailable, "authentication temporarily unavailable")
	}
	if project.ID <= 0 || strings.TrimSpace(project.Slug) == "" {
		return meta.ProjectInfo{}, status.Error(codes.Internal, "project context is missing")
	}
	return project, nil
}
