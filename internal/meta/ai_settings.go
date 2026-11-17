package meta

import (
	"context"
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
	if settings.UpdatedAt.IsZero() {
		settings.UpdatedAt = time.Now().UTC()
	}
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
