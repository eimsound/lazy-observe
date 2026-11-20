package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/secret"
)

// AISettingsStore persists the single-row AI configuration. The metadata
// store implements it; the gateway never decrypts keys it does not need.
type AISettingsStore interface {
	GetAISettings(context.Context) (meta.AISettings, bool, error)
	UpsertAISettings(context.Context, meta.AISettings) error
}

// aiDefaultMaxTokens applies when the stored max_tokens is unset.
const aiDefaultMaxTokens = 1024

// aiDefaultTimeout applies when the gateway has no explicit timeout override.
const aiDefaultTimeout = 60 * time.Second

// aiProviders is the whitelist the admin UI can choose from. Every provider
// maps to a known wire protocol; base_url stays overridable for gateways.
var aiProviders = map[string]bool{
	"openai":    true,
	"anthropic": true,
	"ollama":    true,
	"deepseek":  true,
}

// aiSnapshot is an immutable view of the current runtime AI configuration.
type aiSnapshot struct {
	enabled   bool
	provider  string
	baseURL   string
	model     string
	maxTokens int
	hasKey    bool
	client    ai.Client
}

// aiManager owns the runtime AI configuration. In dynamic mode (store != nil)
// the admin API persists changes and hot-swaps the client; in static mode it
// mirrors the constructor Config for tests and embedders.
type aiManager struct {
	mu      sync.RWMutex
	current aiSnapshot

	store     AISettingsStore
	box       *secret.Box
	build     func(ai.Config) (ai.Client, error)
	timeout   time.Duration
	outbound  outbound.Policy
	now       func() time.Time
	logger    *slog.Logger
	updatedBy string // actor of the last successful write (dynamic mode)
}

func newAIManager(
	store AISettingsStore,
	box *secret.Box,
	build func(ai.Config) (ai.Client, error),
	timeout time.Duration,
	outboundPolicy outbound.Policy,
	now func() time.Time,
	logger *slog.Logger,
) *aiManager {
	if build == nil {
		build = ai.New
	}
	if timeout <= 0 {
		timeout = aiDefaultTimeout
	}
	if now == nil {
		now = time.Now
	}
	return &aiManager{
		store: store, box: box, build: build, timeout: timeout, outbound: outboundPolicy, now: now, logger: logger,
	}
}

// snapshot returns the current runtime configuration.
func (m *aiManager) snapshot() aiSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// loadFromStore (re)builds the runtime state from the persisted row. Called at
// startup (dynamic mode) and after every successful save. Never returns an
// error: an unreadable AI config must not block a monitoring platform.
func (m *aiManager) loadFromStore(ctx context.Context) {
	stored, ok, err := m.store.GetAISettings(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Error("read AI settings failed; AI features stay disabled", "error", err)
		}
		return
	}
	snap, err := m.buildFromStored(stored, ok)
	if err != nil && m.logger != nil {
		m.logger.Error("build AI client failed; AI features stay disabled", "error", err)
	}
	m.mu.Lock()
	m.current = snap
	if ok {
		m.updatedBy = stored.UpdatedBy
	}
	m.mu.Unlock()
}

func (m *aiManager) buildFromStored(stored meta.AISettings, ok bool) (aiSnapshot, error) {
	if !ok {
		return aiSnapshot{}, nil
	}
	maxTokens := stored.MaxTokens
	if maxTokens <= 0 {
		maxTokens = aiDefaultMaxTokens
	}
	snap := aiSnapshot{
		enabled:   stored.Enabled,
		provider:  stored.Provider,
		baseURL:   stored.BaseURL,
		model:     stored.Model,
		maxTokens: maxTokens,
	}
	if !snap.enabled {
		if stored.APIKeyCiphertext != "" {
			if m.box == nil {
				snap.hasKey = true
			} else if decrypted, err := m.box.Decrypt(stored.APIKeyCiphertext); err == nil {
				snap.hasKey = decrypted != ""
			}
		}
		return snap, nil
	}
	apiKey := ""
	if stored.APIKeyCiphertext != "" {
		if m.box == nil {
			return aiSnapshot{}, errors.New("encryption is unavailable")
		}
		decrypted, err := m.box.Decrypt(stored.APIKeyCiphertext)
		if err != nil {
			// An undecryptable key is treated as a broken configuration: AI
			// stays disabled rather than sending unauthenticated requests.
			return aiSnapshot{}, fmt.Errorf("decrypt AI API key: %w", err)
		}
		apiKey = decrypted
	}
	snap.hasKey = apiKey != ""
	client, err := m.build(ai.Config{
		Provider:  stored.Provider,
		BaseURL:   stored.BaseURL,
		APIKey:    apiKey,
		Model:     stored.Model,
		MaxTokens: maxTokens,
		Timeout:   m.timeout,
		Outbound:  m.outbound,
	})
	if err != nil {
		return snap, err
	}
	snap.client = client
	return snap, nil
}

// ---- Gateway accessors ----------------------------------------------------

func (g *Gateway) aiState() aiSnapshot {
	if g.ai == nil {
		return aiSnapshot{}
	}
	return g.ai.snapshot()
}

// ---- Admin settings API ---------------------------------------------------

type aiSettingsWire struct {
	Enabled     *bool   `json:"enabled"`
	Provider    *string `json:"provider"`
	BaseURL     *string `json:"base_url"`
	Model       *string `json:"model"`
	APIKey      *string `json:"api_key"`
	MaxTokens   *int    `json:"max_tokens"`
	ClearAPIKey bool    `json:"clear_api_key"`
}

func (g *Gateway) aiSettingsGet(c fiber.Ctx) error {
	m := g.ai
	if m == nil || m.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "AI settings are managed by the server configuration",
		})
	}
	snap := m.snapshot()
	return c.JSON(fiber.Map{
		"enabled":     snap.enabled,
		"provider":    snap.provider,
		"base_url":    snap.baseURL,
		"model":       snap.model,
		"max_tokens":  snap.maxTokens,
		"has_api_key": snap.hasKey,
		"source":      "database",
		"updated_by":  m.updatedBy,
	})
}

func (g *Gateway) aiSettingsPut(c fiber.Ctx) error {
	m := g.ai
	if m == nil || m.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "AI settings are managed by the server configuration",
		})
	}
	var body aiSettingsWire
	if err := c.Bind().Body(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	actor := ""
	if user, ok := c.Locals(userLocal).(meta.UserInfo); ok {
		actor = user.Username
	}

	// Merge with the persisted row: absent fields keep their stored values.
	// The merged values outlive this request in the running snapshot and AI
	// client, so copy them: BodyParser fills form-encoded fields with strings
	// that borrow Fiber's reusable request buffer.
	stored, ok, err := m.store.GetAISettings(c.Context())
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "read AI settings failed"})
	}
	if !ok {
		stored = meta.AISettings{UpdatedAt: m.now().UTC()}
	}
	next := stored
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.Provider != nil {
		next.Provider = strings.Clone(strings.TrimSpace(*body.Provider))
	}
	if body.BaseURL != nil {
		next.BaseURL = strings.Clone(strings.TrimSpace(*body.BaseURL))
	}
	if body.Model != nil {
		next.Model = strings.Clone(strings.TrimSpace(*body.Model))
	}
	if body.MaxTokens != nil {
		next.MaxTokens = *body.MaxTokens
	}

	// Key lifecycle: explicit new key replaces; clear_api_key empties;
	// otherwise the stored ciphertext is kept.
	if body.ClearAPIKey {
		next.APIKeyCiphertext = ""
	} else if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		if m.box == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "encryption is unavailable"})
		}
		sealed, sealErr := m.box.Encrypt(strings.TrimSpace(*body.APIKey))
		if sealErr != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "encrypt API key failed"})
		}
		next.APIKeyCiphertext = sealed
	}

	if err := validateAISettings(next, m, body.BaseURL != nil); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": outbound.SanitizeError(err)})
	}

	// Build the client before persisting: invalid configurations must not be
	// saved, so the running state can never be worse than the stored state.
	// When disabled, buildFromStored only re-derives the display snapshot
	// (no client is constructed), so it cannot fail here.
	snap, err := m.buildFromStored(next, true)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": outbound.SanitizeError(err)})
	}
	next.UpdatedAt = m.now().UTC()
	next.UpdatedBy = actor
	if err := m.store.UpsertAISettings(c.Context(), next); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "save AI settings failed"})
	}

	m.mu.Lock()
	m.current = snap
	m.updatedBy = actor
	m.mu.Unlock()
	if m.logger != nil {
		m.logger.Info("AI settings updated",
			"enabled", snap.enabled, "provider", snap.provider, "model", snap.model, "by", actor)
	}

	return c.JSON(fiber.Map{
		"enabled":     snap.enabled,
		"provider":    snap.provider,
		"base_url":    snap.baseURL,
		"model":       snap.model,
		"max_tokens":  snap.maxTokens,
		"has_api_key": snap.hasKey,
		"source":      "database",
		"updated_by":  actor,
	})
}

func providerRequiresAPIKey(provider string) bool {
	return provider != "ollama"
}

func validateAISettings(s meta.AISettings, m *aiManager, validateBaseURL bool) error {
	if !s.Enabled {
		if s.Provider != "" && !aiProviders[s.Provider] {
			return errors.New("unsupported AI provider")
		}
		if validateBaseURL && s.BaseURL != "" {
			if _, err := outbound.ValidateURL(s.BaseURL, m.outbound); err != nil {
				return fmt.Errorf("invalid base_url: %w", err)
			}
		}
		return nil
	}
	if !aiProviders[s.Provider] {
		return errors.New("unsupported AI provider")
	}
	if strings.TrimSpace(s.Model) == "" {
		return errors.New("model is required when AI is enabled")
	}
	if providerRequiresAPIKey(s.Provider) && m.box != nil && s.APIKeyCiphertext == "" {
		return errors.New("API key is required when AI is enabled")
	}
	if s.BaseURL != "" {
		if _, err := outbound.ValidateURL(s.BaseURL, m.outbound); err != nil {
			return fmt.Errorf("invalid base_url: %w", err)
		}
	}
	if s.MaxTokens < 0 {
		return errors.New("max_tokens must not be negative")
	}
	return nil
}
