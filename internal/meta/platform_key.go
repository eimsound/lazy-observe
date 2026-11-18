package meta

import (
	"context"
	"fmt"
	"time"
)

// platformKeyFingerprintRowID is the fixed id of the single fingerprint row.
// The CHECK constraint in the baseline keeps the table single-row at the schema
// level; both write paths upsert onto this id.
const platformKeyFingerprintRowID = 1

// PlatformKeyFingerprint is the recorded identifier of the platform encryption
// key. It holds a truncated HMAC of the key, never key material.
type PlatformKeyFingerprint struct {
	Fingerprint string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// EnsurePlatformKeyFingerprint records fingerprint when the row is absent and
// returns the fingerprint that is in effect, with created reporting whether
// this call wrote it. Concurrent first starts converge on one row: the insert
// is conditional, so every other replica reads the winner's value and compares
// against it instead of overwriting it. A different key therefore never
// replaces the record; the caller decides what a mismatch means.
func (s *Store) EnsurePlatformKeyFingerprint(
	ctx context.Context,
	fingerprint string,
) (PlatformKeyFingerprint, bool, error) {
	now := time.Now().UTC()
	query := `INSERT INTO platform_key_fingerprints (id, fingerprint, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`
	result, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, query),
		platformKeyFingerprintRowID,
		fingerprint,
		now,
		now,
	)
	if err != nil {
		return PlatformKeyFingerprint{}, false, fmt.Errorf("record platform key fingerprint: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return PlatformKeyFingerprint{}, false, fmt.Errorf("record platform key fingerprint: %w", err)
	}
	record, _, err := s.recordedPlatformKeyFingerprint(ctx)
	if err != nil {
		return PlatformKeyFingerprint{}, false, err
	}
	return record, inserted == 1, nil
}

// ReplacePlatformKeyFingerprint overwrites the recorded fingerprint. It is the
// explicit rotation exit behind `admin reset-key-fingerprint`: only an operator
// who accepts that values sealed under the previous key can no longer be
// decrypted calls it. Nothing rotates automatically.
func (s *Store) ReplacePlatformKeyFingerprint(
	ctx context.Context,
	fingerprint string,
) (PlatformKeyFingerprint, error) {
	now := time.Now().UTC()
	query := `INSERT INTO platform_key_fingerprints (id, fingerprint, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET fingerprint = excluded.fingerprint, updated_at = excluded.updated_at`
	if _, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, query),
		platformKeyFingerprintRowID,
		fingerprint,
		now,
		now,
	); err != nil {
		return PlatformKeyFingerprint{}, fmt.Errorf("replace platform key fingerprint: %w", err)
	}
	record, _, err := s.recordedPlatformKeyFingerprint(ctx)
	if err != nil {
		return PlatformKeyFingerprint{}, err
	}
	return record, nil
}

// RecordedPlatformKeyFingerprint returns the fingerprint currently recorded,
// with ok false when no row exists yet (no server start recorded one, and no
// operator reset it). Callers that only need the value in effect after a write
// use EnsurePlatformKeyFingerprint instead.
func (s *Store) RecordedPlatformKeyFingerprint(
	ctx context.Context,
) (PlatformKeyFingerprint, bool, error) {
	return s.recordedPlatformKeyFingerprint(ctx)
}

func (s *Store) recordedPlatformKeyFingerprint(
	ctx context.Context,
) (PlatformKeyFingerprint, bool, error) {
	row, err := s.client.PlatformKeyFingerprint.Get(ctx, platformKeyFingerprintRowID)
	if IsNotFound(err) {
		return PlatformKeyFingerprint{}, false, nil
	}
	if err != nil {
		return PlatformKeyFingerprint{}, false, fmt.Errorf("read platform key fingerprint: %w", err)
	}
	return PlatformKeyFingerprint{
		Fingerprint: row.Fingerprint,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}, true, nil
}
