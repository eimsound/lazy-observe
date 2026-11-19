package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Upload tokens are confidential credentials stored only as digests (ADR 0018), are
// project-scoped, and go away with their project.
func TestSourceMapUploadTokenLifecycle(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Upload", "upload")
		if err != nil {
			t.Fatal(err)
		}
		other, err := store.CreateProject(ctx, "Other", "other")
		if err != nil {
			t.Fatal(err)
		}
		info, value, err := store.CreateSourceMapUploadToken(ctx, project.ID, " ci ")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(value, SourceMapUploadTokenPrefix) || info.Name != "ci" ||
			!strings.HasPrefix(value, info.Prefix) || info.Prefix == value {
			t.Fatalf("issued %+v value=%q", info, value)
		}
		if _, _, err := store.CreateSourceMapUploadToken(ctx, project.ID, " "); err == nil {
			t.Fatal("blank name accepted")
		}

		var stored string
		if err := store.db.QueryRowContext(ctx, "SELECT token FROM source_map_upload_tokens").
			Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stored, value) || len(stored) != 64 {
			t.Fatalf("stored %q for %q; want a 64-hex digest", stored, value)
		}

		got, err := store.AuthenticateSourceMapUploadToken(ctx, value)
		if err != nil || got.ID != project.ID {
			t.Fatalf("authenticate project=%+v err=%v", got, err)
		}
		if err := store.RevokeSourceMapUploadTokenByID(ctx, other.ID, info.ID); !IsNotFound(err) {
			t.Fatalf("cross-project revoke err=%v", err)
		}
		before := store.Revision()
		if err := store.RevokeSourceMapUploadTokenByID(ctx, project.ID, info.ID); err != nil {
			t.Fatal(err)
		}
		if store.Revision() == before {
			t.Fatal("revoke did not advance the store revision")
		}
		if _, err := store.AuthenticateSourceMapUploadToken(
			ctx,
			value,
		); !errors.Is(
			err,
			ErrInvalidSourceMapUploadToken,
		) {
			t.Fatalf("revoked token err=%v", err)
		}
		listed, err := store.ListSourceMapUploadTokens(ctx, project.ID)
		if err != nil || len(listed) != 1 || !listed[0].Revoked {
			t.Fatalf("listed=%+v err=%v", listed, err)
		}

		if err := store.DeleteProject(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM source_map_upload_tokens").
			Scan(&rows); err != nil ||
			rows != 0 {
			t.Fatalf("rows after project delete=%d err=%v", rows, err)
		}
	})
}
