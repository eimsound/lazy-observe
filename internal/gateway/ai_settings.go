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

// aiSettingsCASStore is an optional atomic compare-and-swap extension
// implemented by *meta.Store so concurrent writers across replicas cannot
// silently lose updates.
type aiSettingsCASStore interface {
	CompareAndSwapAISettings(ctx context.Context, expected *meta.AISettings, next meta.AISettings) (bool, error)
}

// aiDefaultMaxTokens applies when the stored max_tokens is unset.
const aiDefaultMaxTokens = 1024

// aiDefaultTimeout applies when the gateway has no explicit timeout override.
const aiDefaultTimeout = 60 * time.Second

// aiDefaultRefreshInterval controls how frequently dynamic-mode replicas poll
// the metadata store for out-of-band AI setting rotations or revocations.
const aiDefaultRefreshInterval = 5 * time.Second

const aiSettingsCASRetries = 8

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
	updatedBy string
	updatedAt time.Time
	revision  uint64
	client    ai.Client
}

// aiManager owns the runtime AI configuration. In dynamic mode (store != nil)
// the admin API persists changes and hot-swaps the client; in static mode it
// mirrors the constructor Config for tests and embedders.
type aiManager struct {
	mu              sync.RWMutex
	current         aiSnapshot
	hasStored       bool
	stored          meta.AISettings
	lastSync        time.Time
	writeMu         sync.Mutex
	store           AISettingsStore
	box             *secret.Box
	build           func(ai.Config) (ai.Client, error)
	timeout         time.Duration
	refreshInterval time.Duration
	outbound        outbound.Policy
	now             func() time.Time
	logger          *slog.Logger

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

func newAIManager(
	store AISettingsStore,
	box *secret.Box,
	build func(ai.Config) (ai.Client, error),
	timeout time.Duration,
	refreshInterval time.Duration,
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
	if refreshInterval <= 0 {
		refreshInterval = aiDefaultRefreshInterval
	}
	if now == nil {
		now = time.Now
	}
	doneCh := make(chan struct{})
	close(doneCh)
	return &aiManager{
		store:           store,
		box:             box,
		build:           build,
		timeout:         timeout,
		refreshInterval: refreshInterval,
		outbound:        outboundPolicy,
		now:             now,
		logger:          logger,
		doneCh:          doneCh,
	}
}

// startSyncLoop periodically reconciles the in-memory AI snapshot with the
// metadata store so key rotations, model changes, and disablements performed on
// another replica take effect without a restart.
func (m *aiManager) startSyncLoop() {
	if m == nil || m.store == nil || m.refreshInterval <= 0 {
		return
	}
	m.startOnce.Do(func() {
		stopCh := make(chan struct{})
		doneCh := make(chan struct{})
		m.mu.Lock()
		m.stopCh = stopCh
		m.doneCh = doneCh
		m.mu.Unlock()
		go func() {
			defer close(doneCh)
			ticker := time.NewTicker(m.refreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					m.refreshFromStore(ctx)
					cancel()
				}
			}
		}()
	})
}

// Close stops the background HA sync loop and waits for it to exit.
func (m *aiManager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	stopCh := m.stopCh
	doneCh := m.doneCh
	m.mu.RUnlock()
	if stopCh == nil {
		return nil
	}
	m.stopOnce.Do(func() { close(stopCh) })
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// snapshot returns an immutable copy of the current runtime configuration.
func (m *aiManager) snapshot() aiSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// loadFromStore (re)builds the runtime state from the persisted row. Called at
// startup (dynamic mode) and by the HA refresh loop. Never returns an error:
// an unreadable AI config must not block a monitoring platform.
func (m *aiManager) loadFromStore(ctx context.Context) {
	m.refreshFromStore(ctx)
}

func (m *aiManager) refreshFromStore(ctx context.Context) {
	if m == nil || m.store == nil {
		return
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	stored, ok, err := m.store.GetAISettings(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Error("read AI settings failed; keeping last AI snapshot", "error", err)
		}
		return
	}
	m.applyStoredLocked(stored, ok)
}

func (m *aiManager) applyStoredLocked(stored meta.AISettings, ok bool) {
	m.mu.RLock()
	unchanged := m.hasStored == ok && (!ok || sameStoredAISettings(m.stored, stored)) && !m.lastSync.IsZero()
	m.mu.RUnlock()
	if unchanged {
		m.mu.Lock()
		m.lastSync = m.now()
		m.mu.Unlock()
		return
	}
	snap, err := m.buildFromStored(stored, ok)
	if err != nil && m.logger != nil {
		m.logger.Error("build AI client failed; AI features stay disabled", "error", err)
	}
	m.mu.Lock()
	snap.revision = m.current.revision + 1
	m.current = snap
	m.hasStored = ok
	m.stored = stored
	m.lastSync = m.now()
	m.mu.Unlock()
}

func sameStoredAISettings(a, b meta.AISettings) bool {
	return a.Enabled == b.Enabled &&
		a.Provider == b.Provider &&
		a.BaseURL == b.BaseURL &&
		a.Model == b.Model &&
		a.APIKeyCiphertext == b.APIKeyCiphertext &&
		a.MaxTokens == b.MaxTokens &&
		a.UpdatedBy == b.UpdatedBy &&
		a.UpdatedAt.UTC().Truncate(time.Microsecond).Equal(b.UpdatedAt.UTC().Truncate(time.Microsecond))
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
		updatedBy: stored.UpdatedBy,
		updatedAt: stored.UpdatedAt.UTC(),
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
			return aiSnapshot{
				updatedBy: stored.UpdatedBy,
				updatedAt: stored.UpdatedAt.UTC(),
			}, errors.New("encryption is unavailable")
		}
		decrypted, err := m.box.Decrypt(stored.APIKeyCiphertext)
		if err != nil {
			// An undecryptable key is treated as a broken configuration: AI
			// stays disabled rather than sending unauthenticated requests.
			return aiSnapshot{
				updatedBy: stored.UpdatedBy,
				updatedAt: stored.UpdatedAt.UTC(),
			}, fmt.Errorf("decrypt AI API key: %w", err)
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
		snap.enabled = false
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
	Enabled          *bool   `json:"enabled"`
	Provider         *string `json:"provider"`
	BaseURL          *string `json:"base_url"`
	Model            *string `json:"model"`
	APIKey           *string `json:"api_key"`
	MaxTokens        *int    `json:"max_tokens"`
	ClearAPIKey      bool    `json:"clear_api_key"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

func aiSettingsResponse(snap aiSnapshot) fiber.Map {
	return fiber.Map{
		"enabled":     snap.enabled,
		"provider":    snap.provider,
		"base_url":    snap.baseURL,
		"model":       snap.model,
		"max_tokens":  snap.maxTokens,
		"has_api_key": snap.hasKey,
		"source":      "database",
		"updated_by":  snap.updatedBy,
		"revision":    snap.revision,
	}
}

func (g *Gateway) aiSettingsGet(c fiber.Ctx) error {
	m := g.ai
	if m == nil || m.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "AI settings are managed by the server configuration",
		})
	}
	m.refreshFromStore(c.Context())
	return c.JSON(aiSettingsResponse(m.snapshot()))
}

func (g *Gateway) aiSettingsPut(c fiber.Ctx) error {
	m := g.ai
	if m == nil || m.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "AI settings are managed by the server configuration",
		})
	}
	var body aiSettingsWire
	audit := auditEvent{
		Action:     "ai_settings.update",
		TargetType: "ai_settings",
	}
	defer func() {
		if !audit.Success && audit.FailureReason == "" {
			audit.FailureReason = "update_failed"
		}
		g.auditManagement(c, audit)
	}()
	if err := c.Bind().Body(&body); err != nil {
		audit.FailureReason = "invalid_request"
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if body.ClearAPIKey && body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		audit.FailureReason = "conflicting_api_key_params"
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "api_key and clear_api_key cannot both be set",
		})
	}
	audit.ChangedFields = aiSettingsChangedFields(body)
	actor := ""
	if user, ok := c.Locals(userLocal).(meta.UserInfo); ok {
		actor = strings.Clone(user.Username)
	}

	var sealedKey string
	var hasNewKey bool
	if !body.ClearAPIKey && body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		if m.box == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "encryption is unavailable"})
		}
		var sealErr error
		sealedKey, sealErr = m.box.Encrypt(strings.TrimSpace(*body.APIKey))
		if sealErr != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "encrypt API key failed"})
		}
		hasNewKey = true
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	casStore, hasCAS := m.store.(aiSettingsCASStore)
	for attempt := 0; attempt < aiSettingsCASRetries; attempt++ {
		// Merge with the persisted row: absent fields keep their stored values.
		// The merged values outlive this request in the running snapshot and AI
		// client, so copy them: BodyParser fills form-encoded fields with strings
		// that borrow Fiber's reusable request buffer.
		stored, ok, err := m.store.GetAISettings(c.Context())
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "read AI settings failed"})
		}
		m.applyStoredLocked(stored, ok)
		currentSnap := m.snapshot()
		if body.ExpectedRevision != nil && *body.ExpectedRevision != currentSnap.revision {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":    "AI settings were modified concurrently",
				"revision": currentSnap.revision,
			})
		}

		next := stored
		if !ok {
			next = meta.AISettings{UpdatedAt: m.now().UTC()}
		}
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
		} else if hasNewKey {
			next.APIKeyCiphertext = sealedKey
		}

		if err := validateAISettings(next, m, body.BaseURL != nil); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": outbound.SanitizeError(err)})
		}

		next.UpdatedAt = m.now().UTC().Truncate(time.Microsecond)
		if ok {
			prevAt := stored.UpdatedAt.UTC().Truncate(time.Microsecond)
			if !next.UpdatedAt.After(prevAt) {
				next.UpdatedAt = prevAt.Add(time.Microsecond)
			}
		}
		next.UpdatedBy = actor

		// Build the client before persisting: invalid configurations must not be
		// saved, so the running state can never be worse than the stored state.
		// When disabled, buildFromStored only re-derives the display snapshot
		// (no client is constructed), so it cannot fail here.
		snap, err := m.buildFromStored(next, true)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": outbound.SanitizeError(err)})
		}

		if hasCAS {
			var expected *meta.AISettings
			if ok {
				cp := stored
				expected = &cp
			}
			swapped, casErr := casStore.CompareAndSwapAISettings(c.Context(), expected, next)
			if casErr != nil {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "save AI settings failed"})
			}
			if !swapped {
				if body.ExpectedRevision != nil {
					m.refreshLocked(c.Context())
					return c.Status(fiber.StatusConflict).JSON(fiber.Map{
						"error":    "AI settings were modified concurrently",
						"revision": m.snapshot().revision,
					})
				}
				continue
			}
		} else if err := m.store.UpsertAISettings(c.Context(), next); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "save AI settings failed"})
		}

		m.mu.Lock()
		snap.revision = m.current.revision + 1
		m.current = snap
		m.hasStored = true
		m.stored = next
		m.lastSync = m.now()
		m.mu.Unlock()
		audit.TargetName = snap.provider
		audit.Success = true
		if m.logger != nil {
			m.logger.Info("AI settings updated",
				"enabled", snap.enabled,
				"provider", snap.provider,
				"model", snap.model,
				"revision", snap.revision,
				"by", snap.updatedBy)
		}
		return c.JSON(aiSettingsResponse(snap))
	}
	return c.Status(fiber.StatusConflict).JSON(fiber.Map{
		"error":    "AI settings were modified concurrently; retry",
		"revision": m.snapshot().revision,
	})
}

func aiSettingsChangedFields(body aiSettingsWire) []string {
	changed := make([]string, 0, 6)
	if body.Enabled != nil {
		changed = append(changed, "enabled")
	}
	if body.Provider != nil {
		changed = append(changed, "provider")
	}
	if body.BaseURL != nil {
		changed = append(changed, "base_url")
	}
	if body.Model != nil {
		changed = append(changed, "model")
	}
	if body.MaxTokens != nil {
		changed = append(changed, "max_tokens")
	}
	if body.ClearAPIKey || (body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "") {
		changed = append(changed, "api_key")
	}
	return changed
}

func (m *aiManager) refreshLocked(ctx context.Context) {
	stored, ok, err := m.store.GetAISettings(ctx)
	if err != nil {
		return
	}
	m.applyStoredLocked(stored, ok)
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
