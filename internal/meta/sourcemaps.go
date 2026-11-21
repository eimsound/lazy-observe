package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/eimsound/lazy-observe/internal/db/ent/sourcemapartifact"
)

const sourceMapAdvisoryLockNamespace = 7301

var ErrSourceMapQuotaExceeded = errors.New("source map release archive limit exceeded")

type SourceMapRelease struct {
	Release   string    `json:"release"`
	Files     int       `json:"files"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

type SourceMapArtifactInput struct {
	Filename string
	Size     int64
	SHA256   string
}

func (s *Store) UpsertSourceMapArtifact(
	ctx context.Context,
	projectID int,
	release, filename string,
	size int64,
	sha256 string,
	createdAt time.Time,
) error {
	_, err := s.db.ExecContext(ctx, bind(s.dialect, `INSERT INTO source_map_artifacts(
		release, filename, size, sha256, created_at, project_id
	) VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(project_id, release, filename) DO UPDATE SET
		size = excluded.size,
		sha256 = excluded.sha256`),
		release, filename, size, sha256, createdAt.UTC(), projectID)
	return err
}

// UpsertSourceMapArtifacts atomically validates per-release file count and byte
// quotas and upserts all artifacts in one transaction, eliminating quota TOCTOU
// races and concurrent duplicate-key 500s.
func (s *Store) UpsertSourceMapArtifacts(
	ctx context.Context,
	projectID int,
	release string,
	items []SourceMapArtifactInput,
	maxFiles int,
	maxBytes int64,
	createdAt time.Time,
) error {
	if len(items) == 0 {
		return nil
	}
	if s.dialect != dialect.Postgres {
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin source map upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`SELECT pg_advisory_xact_lock($1, $2)`,
			sourceMapAdvisoryLockNamespace,
			projectID,
		); err != nil {
			return fmt.Errorf("lock source map release quota: %w", err)
		}
	}
	incoming := make(map[string]SourceMapArtifactInput, len(items))
	var incomingBytes int64
	for _, item := range items {
		if prev, dup := incoming[item.Filename]; dup {
			incomingBytes -= prev.Size
		}
		incoming[item.Filename] = item
		incomingBytes += item.Size
	}
	existingCount, existingBytes, err := readExistingSourceMapReleaseQuotaTx(
		ctx,
		tx,
		s.dialect,
		projectID,
		release,
		incoming,
	)
	if err != nil {
		return err
	}
	if (maxFiles > 0 && existingCount+len(incoming) > maxFiles) ||
		(maxBytes > 0 && existingBytes+incomingBytes > maxBytes) {
		return ErrSourceMapQuotaExceeded
	}
	now := createdAt.UTC()
	upsertQuery := bind(s.dialect, `INSERT INTO source_map_artifacts(
		release, filename, size, sha256, created_at, project_id
	) VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(project_id, release, filename) DO UPDATE SET
		size = excluded.size,
		sha256 = excluded.sha256`)
	for _, item := range items {
		if _, err := tx.ExecContext(
			ctx,
			upsertQuery,
			release,
			item.Filename,
			item.Size,
			item.SHA256,
			now,
			projectID,
		); err != nil {
			return fmt.Errorf("upsert source map artifact %q: %w", item.Filename, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit source map upsert: %w", err)
	}
	return nil
}

func (s *Store) SourceMapArtifacts(ctx context.Context, projectID int, release string) ([]*SourceMapArtifact, error) {
	rows, err := s.client.SourceMapArtifact.Query().Where(
		sourcemapartifact.ProjectIDEQ(projectID), sourcemapartifact.ReleaseEQ(release),
	).Order(orderAsc(sourcemapartifact.FieldFilename)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]*SourceMapArtifact, 0, len(rows))
	for _, row := range rows {
		result = append(result, sourceMapArtifactFromEnt(row))
	}
	return result, nil
}

func (s *Store) ListSourceMapReleases(ctx context.Context, projectID, limit int) ([]SourceMapRelease, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(
		ctx,
		bind(
			s.dialect,
			`SELECT release, COUNT(*) AS files, COALESCE(SUM(size), 0) AS total_size, MAX(created_at) AS created_at
			FROM source_map_artifacts
			WHERE project_id = ?
			GROUP BY release
			ORDER BY MAX(created_at) DESC, release ASC
			LIMIT ?`,
		),
		projectID,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SourceMapRelease, 0, limit)
	for rows.Next() {
		var item SourceMapRelease
		var rawCreatedAt any
		if err := rows.Scan(&item.Release, &item.Files, &item.Size, &rawCreatedAt); err != nil {
			return nil, fmt.Errorf("scan source map release: %w", err)
		}
		createdAt, err := parseAggregateTime(rawCreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse source map release created_at for %q: %w", item.Release, err)
		}
		item.CreatedAt = createdAt
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func parseAggregateTime(raw any) (time.Time, error) {
	switch value := raw.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseAggregateTimeString(value)
	case []byte:
		return parseAggregateTimeString(string(value))
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp value type %T", raw)
	}
}

func parseAggregateTimeString(text string) (time.Time, error) {
	trimmed := strings.TrimSpace(text)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999 +0000 UTC",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05 +0000 UTC",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", text)
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
// repeatable-read snapshot transaction so retention sees a single consistent view.
func (s *Store) ReferencedSourceMapDigests(ctx context.Context, projectID int) (map[string]bool, error) {
	txOpts := &sql.TxOptions{ReadOnly: true}
	if s.dialect == dialect.Postgres {
		txOpts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, txOpts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	referenced, err := pendingSnapshotDigestsTx(ctx, tx, s.dialect, projectID)
	if err != nil {
		return nil, err
	}
	if err := readReferencedSourceMapDigestsTx(ctx, tx, s.dialect, projectID, referenced); err != nil {
		return nil, err
	}
	return referenced, tx.Commit()
}

func readExistingSourceMapReleaseQuotaTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	projectID int,
	release string,
	incoming map[string]SourceMapArtifactInput,
) (int, int64, error) {
	rows, err := tx.QueryContext(ctx, bind(dbDialect, `SELECT filename, size FROM source_map_artifacts
		WHERE project_id = ? AND release = ?`), projectID, release)
	if err != nil {
		return 0, 0, fmt.Errorf("read existing source map release quota: %w", err)
	}
	defer rows.Close()
	existingCount := 0
	var existingBytes int64
	for rows.Next() {
		var filename string
		var size int64
		if err := rows.Scan(&filename, &size); err != nil {
			return 0, 0, fmt.Errorf("scan source map release quota: %w", err)
		}
		if _, replaced := incoming[filename]; replaced {
			continue
		}
		existingCount++
		existingBytes += size
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate source map release quota: %w", err)
	}
	return existingCount, existingBytes, nil
}

func readReferencedSourceMapDigestsTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	projectID int,
	referenced map[string]bool,
) error {
	rows, err := tx.QueryContext(ctx, bind(dbDialect, `SELECT DISTINCT sha256 FROM source_map_artifacts
		WHERE project_id = ?`), projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return err
		}
		referenced[digest] = true
	}
	return rows.Err()
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
