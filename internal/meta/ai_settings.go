package meta

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// aiSettingsRowID is the fixed id of the single AI settings row. The CHECK
// constraint in the migration enforces the single-row invariant at the schema
// level; the store upserts onto this id.
const aiSettingsRowID = 1

// AISettings is the runtime AI configuration row. APIKeyCiphertext holds the
// encrypted BYO key; the plaintext key never reaches the metadata database.
type AISettings struct {
	Enabled          bool
	Provider         string
	BaseURL          string
	Model            string
	APIKeyCiphertext string
	MaxTokens        int
	UpdatedAt        time.Time
	UpdatedBy        string
}

// GetAISettings returns the stored AI settings row. ok is false when the
// administrator has never saved settings (fresh-install defaults apply).
func (s *Store) GetAISettings(ctx context.Context) (AISettings, bool, error) {
	row, err := s.client.AISetting.Get(ctx, aiSettingsRowID)
	if IsNotFound(err) {
		return AISettings{}, false, nil
	}
	if err != nil {
		return AISettings{}, false, err
	}
	return AISettings{
		Enabled:          row.Enabled,
		Provider:         row.Provider,
		BaseURL:          row.BaseURL,
		Model:            row.Model,
		APIKeyCiphertext: row.APIKeyCiphertext,
		MaxTokens:        row.MaxTokens,
		UpdatedAt:        row.UpdatedAt,
		UpdatedBy:        row.UpdatedBy,
	}, true, nil
}

// UpsertAISettings replaces the single AI settings row (create when missing).
// Uses a plain SQL upsert because the Ent codegen does not enable the upsert
// feature; both SQLite and PostgreSQL support ON CONFLICT here.
func (s *Store) UpsertAISettings(ctx context.Context, settings AISettings) error {
	s.aiSettingsMu.Lock()
	defer s.aiSettingsMu.Unlock()
	if settings.UpdatedAt.IsZero() {
		settings.UpdatedAt = time.Now().UTC()
	}
	settings.UpdatedAt = settings.UpdatedAt.UTC().Truncate(time.Microsecond)
	query := `INSERT INTO ai_settings (
    id, enabled, provider, base_url, model, api_key_ciphertext, max_tokens, updated_at, updated_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    enabled = excluded.enabled,
    provider = excluded.provider,
    base_url = excluded.base_url,
    model = excluded.model,
    api_key_ciphertext = excluded.api_key_ciphertext,
    max_tokens = excluded.max_tokens,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by`
	_, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, query),
		aiSettingsRowID,
		settings.Enabled,
		settings.Provider,
		settings.BaseURL,
		settings.Model,
		settings.APIKeyCiphertext,
		settings.MaxTokens,
		settings.UpdatedAt,
		settings.UpdatedBy,
	)
	return err
}

// CompareAndSwapAISettings atomically inserts or replaces the single AI
// settings row only when the stored row matches expected (nil expects no row).
// Returns (false, nil) on conflict so callers can reload and retry or reject.
func (s *Store) CompareAndSwapAISettings(ctx context.Context, expected *AISettings, next AISettings) (bool, error) {
	s.aiSettingsMu.Lock()
	defer s.aiSettingsMu.Unlock()

	if next.UpdatedAt.IsZero() {
		next.UpdatedAt = time.Now().UTC()
	}
	next.UpdatedAt = next.UpdatedAt.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	selectQuery := `SELECT enabled, provider, base_url, model, api_key_ciphertext, max_tokens, updated_at, updated_by
FROM ai_settings WHERE id = ?`
	if s.dialect == "postgres" {
		selectQuery += " FOR UPDATE"
	}
	var current AISettings
	scanErr := tx.QueryRowContext(ctx, bind(s.dialect, selectQuery), aiSettingsRowID).Scan(
		&current.Enabled,
		&current.Provider,
		&current.BaseURL,
		&current.Model,
		&current.APIKeyCiphertext,
		&current.MaxTokens,
		&current.UpdatedAt,
		&current.UpdatedBy,
	)
	if errors.Is(scanErr, sql.ErrNoRows) {
		if expected != nil {
			return false, nil
		}
		insertQuery := `INSERT INTO ai_settings (
    id, enabled, provider, base_url, model, api_key_ciphertext, max_tokens, updated_at, updated_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`
		res, execErr := tx.ExecContext(
			ctx,
			bind(s.dialect, insertQuery),
			aiSettingsRowID,
			next.Enabled,
			next.Provider,
			next.BaseURL,
			next.Model,
			next.APIKeyCiphertext,
			next.MaxTokens,
			next.UpdatedAt,
			next.UpdatedBy,
		)
		if execErr != nil {
			return false, execErr
		}
		affected, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return false, rowsErr
		}
		if affected != 1 {
			return false, nil
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return false, commitErr
		}
		return true, nil
	}
	if scanErr != nil {
		return false, scanErr
	}
	if expected == nil || !sameAISettingsRow(current, *expected) {
		return false, nil
	}
	curAt := current.UpdatedAt.UTC().Truncate(time.Microsecond)
	if !next.UpdatedAt.After(curAt) {
		next.UpdatedAt = curAt.Add(time.Microsecond)
	}
	updateQuery := `UPDATE ai_settings SET
    enabled = ?,
    provider = ?,
    base_url = ?,
    model = ?,
    api_key_ciphertext = ?,
    max_tokens = ?,
    updated_at = ?,
    updated_by = ?
WHERE id = ?`
	if _, execErr := tx.ExecContext(
		ctx,
		bind(s.dialect, updateQuery),
		next.Enabled,
		next.Provider,
		next.BaseURL,
		next.Model,
		next.APIKeyCiphertext,
		next.MaxTokens,
		next.UpdatedAt,
		next.UpdatedBy,
		aiSettingsRowID,
	); execErr != nil {
		return false, execErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return false, commitErr
	}
	return true, nil
}

func sameAISettingsRow(a, b AISettings) bool {
	return a.Enabled == b.Enabled &&
		a.Provider == b.Provider &&
		a.BaseURL == b.BaseURL &&
		a.Model == b.Model &&
		a.APIKeyCiphertext == b.APIKeyCiphertext &&
		a.MaxTokens == b.MaxTokens &&
		a.UpdatedBy == b.UpdatedBy &&
		a.UpdatedAt.UTC().Truncate(time.Microsecond).Equal(b.UpdatedAt.UTC().Truncate(time.Microsecond))
}
