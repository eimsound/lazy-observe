package meta

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/eimsound/lazy-observe/internal/db/ent/sourcemapartifact"
)

const sourceMapAdvisoryLockNamespace = 7301

var (
	ErrSourceMapQuotaExceeded    = errors.New("source map release archive limit exceeded")
	ErrSourceMapUploadPinExpired = errors.New("source map upload pin expired")
)

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
		sha256 = excluded.sha256,
		created_at = excluded.created_at`),
		release, filename, size, sha256, createdAt.UTC().Round(0), projectID)
	return err
}

// PinSourceMapUploads records in-flight revision digests for a project before
// blob writes begin so concurrent janitor sweeps cannot reclaim existing or
// newly written revision blobs before metadata commit.
func (s *Store) PinSourceMapUploads(
	ctx context.Context,
	projectID int,
	digests []string,
	expiresAt time.Time,
) (string, error) {
	uniqueDigests := deduplicateNonEmptyStrings(digests)
	if len(uniqueDigests) == 0 {
		return "", nil
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate source map upload pin token: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	expires := expiresAt.UTC().Round(0)
	if s.dialect != dialect.Postgres {
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin source map upload pin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockSourceMapProjectTx(ctx, tx, projectID); err != nil {
		return "", err
	}
	insertQuery := bind(s.dialect, `INSERT INTO source_map_upload_pins(
		token, project_id, sha256, expires_at
	) VALUES (?, ?, ?, ?)`)
	for _, digest := range uniqueDigests {
		if _, err := tx.ExecContext(ctx, insertQuery, token, projectID, digest, expires); err != nil {
			return "", fmt.Errorf("pin source map digest %q: %w", digest, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit source map upload pin: %w", err)
	}
	return token, nil
}

// ReleaseSourceMapUploadPins removes an uncommitted upload's pins immediately
// on failure or cancellation instead of waiting for pin expiry.
func (s *Store) ReleaseSourceMapUploadPins(ctx context.Context, projectID int, token string) error {
	if token == "" {
		return nil
	}
	if s.dialect != dialect.Postgres {
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin source map upload pin release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockSourceMapProjectTx(ctx, tx, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM source_map_upload_pins WHERE project_id = ? AND token = ?`),
		projectID,
		token,
	); err != nil {
		return fmt.Errorf("release source map upload pins: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit source map upload pin release: %w", err)
	}
	return nil
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
	return s.commitSourceMapArtifacts(ctx, projectID, release, "", items, maxFiles, maxBytes, createdAt)
}

// CommitPinnedSourceMapArtifacts atomically verifies and consumes the caller's
// upload pins while upserting the release's source map artifacts under the
// project's source map lock. If the pins were already reclaimed by a janitor
// sweep after expiry, the transaction rolls back with ErrSourceMapUploadPinExpired
// so metadata can never point to a deleted blob.
func (s *Store) CommitPinnedSourceMapArtifacts(
	ctx context.Context,
	projectID int,
	release, pinToken string,
	items []SourceMapArtifactInput,
	maxFiles int,
	maxBytes int64,
	createdAt time.Time,
) error {
	return s.commitSourceMapArtifacts(ctx, projectID, release, pinToken, items, maxFiles, maxBytes, createdAt)
}

func (s *Store) commitSourceMapArtifacts(
	ctx context.Context,
	projectID int,
	release, pinToken string,
	items []SourceMapArtifactInput,
	maxFiles int,
	maxBytes int64,
	createdAt time.Time,
) error {
	if len(items) == 0 {
		return s.ReleaseSourceMapUploadPins(ctx, projectID, pinToken)
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
	if err := s.lockSourceMapProjectTx(ctx, tx, projectID); err != nil {
		return err
	}
	if pinToken != "" {
		pinned, err := consumeSourceMapUploadPinsTx(ctx, tx, s.dialect, projectID, pinToken)
		if err != nil {
			return err
		}
		for _, item := range items {
			if !pinned[item.SHA256] {
				return ErrSourceMapUploadPinExpired
			}
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
	now := createdAt.UTC().Round(0)
	upsertQuery := bind(s.dialect, `INSERT INTO source_map_artifacts(
		release, filename, size, sha256, created_at, project_id
	) VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(project_id, release, filename) DO UPDATE SET
		size = excluded.size,
		sha256 = excluded.sha256,
		created_at = excluded.created_at`)
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

func (s *Store) lockSourceMapProjectTx(ctx context.Context, tx *sql.Tx, projectID int) error {
	if s.dialect != dialect.Postgres {
		return nil
	}
	if _, err := tx.ExecContext(
		ctx,
		`SELECT pg_advisory_xact_lock($1, $2)`,
		sourceMapAdvisoryLockNamespace,
		projectID,
	); err != nil {
		return fmt.Errorf("lock source map project: %w", err)
	}
	return nil
}

func consumeSourceMapUploadPinsTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	projectID int,
	token string,
) (map[string]bool, error) {
	rows, err := tx.QueryContext(
		ctx,
		bind(dbDialect, `DELETE FROM source_map_upload_pins
			WHERE project_id = ? AND token = ?
			RETURNING sha256`),
		projectID,
		token,
	)
	if err != nil {
		return nil, fmt.Errorf("consume source map upload pins: %w", err)
	}
	defer rows.Close()
	pinned := make(map[string]bool)
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, fmt.Errorf("scan source map upload pin: %w", err)
		}
		pinned[digest] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source map upload pins: %w", err)
	}
	return pinned, nil
}

func deduplicateNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
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
	if s.dialect != dialect.Postgres {
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin source map delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockSourceMapProjectTx(ctx, tx, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM source_map_artifacts WHERE project_id = ? AND release = ?`),
		projectID,
		release,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteExpiredSourceMapRelease removes a release's metadata rows only if no
// artifact in that release has been created or refreshed at or after cutoff.
func (s *Store) DeleteExpiredSourceMapRelease(
	ctx context.Context,
	projectID int,
	release string,
	cutoff time.Time,
) (bool, error) {
	cutoff = cutoff.UTC().Round(0)
	if s.dialect != dialect.Postgres {
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin expired source map delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockSourceMapProjectTx(ctx, tx, projectID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM source_map_artifacts
			WHERE project_id = ? AND release = ?
			  AND NOT EXISTS (
				SELECT 1 FROM source_map_artifacts
				WHERE project_id = ? AND release = ? AND created_at >= ?
			  )`),
		projectID,
		release,
		projectID,
		release,
		cutoff,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ReferencedSourceMapDigests returns every revision digest of a project that is
// still needed: referenced by a stored artifact, pinned by an active in-flight
// upload, or captured in the snapshot of Inbox work that may still be
// symbolicated (ADR 0007). All reads share one repeatable-read snapshot
// transaction so retention sees a single consistent view.
func (s *Store) ReferencedSourceMapDigests(ctx context.Context, projectID int) (map[string]bool, error) {
	return s.ReferencedSourceMapDigestsAt(ctx, projectID, time.Now().UTC())
}

// ReferencedSourceMapDigestsAt returns every revision digest referenced by the
// project as of now (ignoring upload pins with expires_at <= now).
func (s *Store) ReferencedSourceMapDigestsAt(
	ctx context.Context,
	projectID int,
	now time.Time,
) (map[string]bool, error) {
	txOpts := &sql.TxOptions{ReadOnly: true}
	if s.dialect == dialect.Postgres {
		txOpts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, txOpts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	referenced, err := readAllReferencedSourceMapDigestsTx(ctx, tx, s.dialect, projectID, now.UTC().Round(0))
	if err != nil {
		return nil, err
	}
	return referenced, tx.Commit()
}

// WithSourceMapSweepLock serializes a project's orphan blob reclamation against
// concurrent upload pin registration, metadata commits, and release deletions
// across both SQLite and multi-replica PostgreSQL deployments. Expired upload
// pins for the project are purged before taking the consistent reference
// snapshot; fn runs with no open database transaction while the project's
// source map lock remains held.
func (s *Store) WithSourceMapSweepLock(
	ctx context.Context,
	projectID int,
	now time.Time,
	fn func(referenced map[string]bool) error,
) error {
	if fn == nil {
		return nil
	}
	now = now.UTC().Round(0)
	if s.dialect != dialect.Postgres {
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin source map sweep snapshot: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, `DELETE FROM source_map_upload_pins WHERE project_id = ? AND expires_at <= ?`),
			projectID,
			now,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("prune expired source map upload pins: %w", err)
		}
		referenced, err := readAllReferencedSourceMapDigestsTx(ctx, tx, s.dialect, projectID, now)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit source map sweep snapshot: %w", err)
		}
		return fn(referenced)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve source map sweep connection: %w", err)
	}
	if _, err := conn.ExecContext(
		ctx,
		`SELECT pg_advisory_lock($1, $2)`,
		sourceMapAdvisoryLockNamespace,
		projectID,
	); err != nil {
		_ = conn.Close()
		return fmt.Errorf("lock source map sweep: %w", err)
	}
	unlock := func() error {
		defer conn.Close()
		return releasePostgresAdvisoryLock2(
			conn,
			sourceMapAdvisoryLockNamespace,
			projectID,
			"PostgreSQL source map sweep",
		)
	}
	if _, err := conn.ExecContext(
		ctx,
		`DELETE FROM source_map_upload_pins WHERE project_id = $1 AND expires_at <= $2`,
		projectID,
		now,
	); err != nil {
		return errors.Join(fmt.Errorf("prune expired source map upload pins: %w", err), unlock())
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return errors.Join(fmt.Errorf("begin source map sweep snapshot: %w", err), unlock())
	}
	referenced, err := readAllReferencedSourceMapDigestsTx(ctx, tx, s.dialect, projectID, now)
	if err != nil {
		_ = tx.Rollback()
		return errors.Join(err, unlock())
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(fmt.Errorf("commit source map sweep snapshot: %w", err), unlock())
	}
	return errors.Join(fn(referenced), unlock())
}

// PruneExpiredSourceMapUploadPins removes any leftover expired upload pins in
// bounded batches so crashed uploads that failed before blob write do not grow
// the pin table unboundedly.
func (s *Store) PruneExpiredSourceMapUploadPins(ctx context.Context, now time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired source map upload pins",
		`DELETE FROM source_map_upload_pins WHERE (token, sha256) IN (
			SELECT token, sha256 FROM source_map_upload_pins
			WHERE expires_at <= ?
			ORDER BY expires_at, token, sha256
			LIMIT ?
		)`,
		now.UTC().Round(0),
	)
}

func readAllReferencedSourceMapDigestsTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	projectID int,
	now time.Time,
) (map[string]bool, error) {
	referenced, err := pendingSnapshotDigestsTx(ctx, tx, dbDialect, projectID)
	if err != nil {
		return nil, err
	}
	if err := readReferencedSourceMapDigestsTx(ctx, tx, dbDialect, projectID, referenced); err != nil {
		return nil, err
	}
	if err := readPinnedSourceMapDigestsTx(ctx, tx, dbDialect, projectID, now, referenced); err != nil {
		return nil, err
	}
	return referenced, nil
}

func readPinnedSourceMapDigestsTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	projectID int,
	now time.Time,
	referenced map[string]bool,
) error {
	rows, err := tx.QueryContext(
		ctx,
		bind(dbDialect, `SELECT DISTINCT sha256 FROM source_map_upload_pins
			WHERE project_id = ? AND expires_at > ?`),
		projectID,
		now,
	)
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
