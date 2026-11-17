package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/sourcemapartifact"
)

type SourceMapRelease struct {
	Release   string    `json:"release"`
	Files     int       `json:"files"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) UpsertSourceMapArtifact(
	ctx context.Context,
	projectID int,
	release, filename string,
	size int64,
	sha256 string,
	createdAt time.Time,
) error {
	row, err := s.client.SourceMapArtifact.Query().Where(
		sourcemapartifact.ProjectIDEQ(projectID),
		sourcemapartifact.ReleaseEQ(release), sourcemapartifact.FilenameEQ(filename),
	).
		Only(ctx)
	if err == nil {
		_, err = row.Update().SetSize(size).SetSha256(sha256).Save(ctx)
		return err
	}
	if !IsNotFound(err) {
		return err
	}
	_, err = s.client.SourceMapArtifact.Create().SetProjectID(projectID).SetRelease(release).SetFilename(filename).
		SetSize(size).SetSha256(sha256).SetCreatedAt(createdAt).Save(ctx)
	return err
}

func (s *Store) SourceMapArtifacts(ctx context.Context, projectID int, release string) ([]*SourceMapArtifact, error) {
	return s.client.SourceMapArtifact.Query().Where(
		sourcemapartifact.ProjectIDEQ(projectID), sourcemapartifact.ReleaseEQ(release),
	).Order(orderAsc(sourcemapartifact.FieldFilename)).All(ctx)
}

func (s *Store) ListSourceMapReleases(ctx context.Context, projectID, limit int) ([]SourceMapRelease, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.client.SourceMapArtifact.Query().Where(sourcemapartifact.ProjectIDEQ(projectID)).
		Order(orderDesc(sourcemapartifact.FieldCreatedAt)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]SourceMapRelease, 0, limit)
	byRelease := make(map[string]int)
	for _, row := range rows {
		i, ok := byRelease[row.Release]
		if !ok {
			if len(result) == limit {
				continue
			}
			i = len(result)
			byRelease[row.Release] = i
			result = append(result, SourceMapRelease{Release: row.Release, CreatedAt: row.CreatedAt})
		}
		result[i].Files++
		result[i].Size += row.Size
	}
	return result, nil
}

func (s *Store) DeleteSourceMapArtifacts(ctx context.Context, projectID int, release string) error {
	_, err := s.client.SourceMapArtifact.Delete().Where(
		sourcemapartifact.ProjectIDEQ(projectID), sourcemapartifact.ReleaseEQ(release),
	).Exec(ctx)
	return err
}

// ReferencedSourceMapDigests returns every revision digest of a project that is
// still needed: referenced by a stored artifact, or captured in the snapshot of
// Inbox work that may still be symbolicated (ADR 0007). Both reads share one
// transaction so retention sees a single consistent view.
func (s *Store) ReferencedSourceMapDigests(ctx context.Context, projectID int) (map[string]bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	referenced, err := pendingSnapshotDigestsTx(ctx, tx, s.dialect, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, bind(s.dialect, `SELECT DISTINCT sha256 FROM source_map_artifacts
		WHERE project_id = ?`), projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			_ = rows.Close()
			return nil, err
		}
		referenced[digest] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return referenced, tx.Commit()
}

// pendingSnapshotDigestsTx collects revision digests captured by Inbox items
// that may still be symbolicated. The Inbox is capacity-bounded, so reading the
// project's unfinished snapshots is bounded too.
func pendingSnapshotDigestsTx(ctx context.Context, tx *sql.Tx, d string, projectID int) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, bind(d, `SELECT source_map_snapshot FROM telemetry_inbox
		WHERE project_id = ? AND status IN ('pending', 'processing')`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pinned := make(map[string]bool)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var snapshot map[string]string
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode pending source map snapshot: %w", err)
		}
		for key, digest := range snapshot {
			if strings.HasPrefix(key, "file:") {
				pinned[digest] = true
			}
		}
	}
	return pinned, rows.Err()
}
