package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/secret"
)

// dynamicAITestApp builds a gateway in dynamic AI mode: a fake provider that
// echoes requests, a secret box, and the metadata store as the settings
// backend. Returns the app plus admin/viewer cookies.
func dynamicAITestApp(t *testing.T) (*httptest.Server, *Gateway, *meta.Store, *http.Cookie, *http.Cookie, string) {
	t.Helper()
	const apiKey = "dynamic-mode-test-key"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("pong")) {
			t.Fatalf("provider body=%s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "deepseek-chat",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "pong"}}},
		})
	}))
	t.Cleanup(provider.Close)
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	admin := sessionCookie(t, store, "ai-settings-admin", "password", "admin")
	viewer := sessionCookie(t, store, "ai-settings-viewer", "password", "viewer")
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
		AITimeout:       time.Second,
		OutboundPolicy:  outbound.Policy{AllowPrivate: true},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	return provider, gw, store, admin, viewer, apiKey
}

func requestJSON(
	t *testing.T,
	app *Gateway,
	method, path string,
	cookie *http.Cookie,
	payload any,
) (*http.Response, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(cookie)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestAISettingsRBACAndRoundTrip(t *testing.T) {
	provider, gw, store, admin, viewer, key := dynamicAITestApp(t)

	// GET /api/ai/settings requires admin.
	resp, _ := requestJSON(t, gw, http.MethodGet, "/api/ai/settings", viewer, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer GET settings status=%d", resp.StatusCode)
	}
	resp, body := requestJSON(t, gw, http.MethodGet, "/api/ai/settings", admin, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"enabled":false`)) {
		t.Fatalf("fresh settings status=%d body=%s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte("\"api_key\":")) {
		t.Fatalf("settings response leaked an api_key field: %s", body)
	}

	// Save a disabled configuration first (provider + model only).
	resp, body = requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": false, "provider": "deepseek", "base_url": "", "model": "deepseek-chat",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save disabled settings status=%d body=%s", resp.StatusCode, body)
	}

	// Enabling without a key must be rejected.
	resp, body = requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enable without key status=%d body=%s", resp.StatusCode, body)
	}

	// Enable with a key, pointing at the fake provider.
	resp, body = requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true, "provider": "openai", "base_url": provider.URL, "model": "configured-model",
		"api_key": key,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable settings status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"enabled":true`)) || !bytes.Contains(body, []byte(`"has_api_key":true`)) ||
		bytes.Contains(body, []byte(key)) {
		t.Fatalf("enabled settings body=%s", body)
	}

	// The key must be stored encrypted, never in plaintext.
	stored, ok, err := store.GetAISettings(t.Context())
	if err != nil || !ok {
		t.Fatalf("stored settings ok=%v err=%v", ok, err)
	}
	if stored.APIKeyCiphertext == key || stored.APIKeyCiphertext == "" {
		t.Fatalf("api key stored insecurely: %q", stored.APIKeyCiphertext)
	}

	// The live client now talks to the provider.
	resp, body = requestJSON(t, gw, http.MethodPost, "/api/ai/test", admin, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"echo":"pong"`)) {
		t.Fatalf("test after enable status=%d body=%s", resp.StatusCode, body)
	}

	// Disable again: the client must be swapped out without a restart.
	resp, body = requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = requestJSON(t, gw, http.MethodPost, "/api/ai/test", admin, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("test after disable status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAISettingsRejectsInvalidProvidersAndModels(t *testing.T) {
	_, gw, _, admin, _, key := dynamicAITestApp(t)

	cases := []map[string]any{
		{"enabled": true, "provider": "unknown", "model": "m", "api_key": key},
		{"enabled": true, "provider": "deepseek", "model": "", "api_key": key},
		{"enabled": true, "provider": "deepseek", "model": "deepseek-chat", "api_key": " "},
	}
	for i, payload := range cases {
		resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %d status=%d body=%s", i, resp.StatusCode, body)
		}
	}
	// Invalid saves must not corrupt the running state.
	state := gw.aiState()
	if state.enabled {
		t.Fatal("invalid save left AI enabled")
	}
}

func TestAISettingsClearAPIKeyAndDecrypt(t *testing.T) {
	_, gw, store, admin, _, key := dynamicAITestApp(t)

	requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true, "provider": "deepseek", "model": "deepseek-chat", "api_key": key,
	})
	_, body := requestJSON(t, gw, http.MethodGet, "/api/ai/settings", admin, nil)
	if !bytes.Contains(body, []byte(`"has_api_key":true`)) {
		t.Fatalf("settings after save body=%s", body)
	}

	// Clearing the key while AI stays enabled must be rejected: an enabled
	// configuration without a usable key is not a valid state.
	resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"clear_api_key": true,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("clear key while enabled status=%d body=%s", resp.StatusCode, body)
	}

	// Disable + clear empties the stored ciphertext.
	resp, body = requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": false, "clear_api_key": true,
	})
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"has_api_key":false`)) {
		t.Fatalf("disable+clear status=%d body=%s", resp.StatusCode, body)
	}
	stored, _, err := store.GetAISettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKeyCiphertext != "" {
		t.Fatalf("ciphertext not cleared: %q", stored.APIKeyCiphertext)
	}
}

func TestAIManagerRestoresPersistedSettingsOnStartup(t *testing.T) {
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	sealed, err := box.Encrypt("startup-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAISettings(t.Context(), meta.AISettings{
		Enabled: true, Provider: "deepseek", Model: "deepseek-chat",
		APIKeyCiphertext: sealed, MaxTokens: 2048,
		UpdatedAt: time.Now().UTC(), UpdatedBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
		AITimeout:       time.Second,
		OutboundPolicy:  outbound.Policy{AllowPrivate: true},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	state := gw.aiState()
	if !state.enabled || state.provider != "deepseek" || state.model != "deepseek-chat" || !state.hasKey ||
		state.client == nil {
		t.Fatalf("startup state=%#v", state)
	}
	if state.maxTokens != 2048 {
		t.Fatalf("max_tokens=%d want 2048", state.maxTokens)
	}
}

func TestAIManagerCorruptCiphertextKeepsAIDisabled(t *testing.T) {
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	if err := store.UpsertAISettings(t.Context(), meta.AISettings{
		Enabled: true, Provider: "deepseek", Model: "deepseek-chat",
		APIKeyCiphertext: "garbage-not-a-sealed-value", // cannot decrypt
		UpdatedAt:        time.Now().UTC(), UpdatedBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
		AITimeout:       time.Second,
		OutboundPolicy:  outbound.Policy{AllowPrivate: true},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	state := gw.aiState()
	if state.enabled || state.hasKey || state.client != nil {
		t.Fatalf("corrupt key state=%#v", state)
	}
}

func TestAISettingsDeepSeekDefaults(t *testing.T) {
	_, gw, _, admin, _, key := dynamicAITestApp(t)
	resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true, "provider": "deepseek", "model": "deepseek-chat", "api_key": key,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save deepseek status=%d body=%s", resp.StatusCode, body)
	}
	state := gw.aiState()
	if !state.enabled || state.provider != "deepseek" || state.model != "deepseek-chat" || !state.hasKey ||
		state.client == nil {
		t.Fatalf("deepseek state=%#v", state)
	}
	// The provider default base URL (https://api.deepseek.com) applies when
	// none is stored; verified in depth by the ai package tests.
	if _, err := ai.New(
		ai.Config{Provider: "deepseek", APIKey: key, Model: "deepseek-chat", MaxTokens: 1024, Timeout: time.Second},
	); err != nil {
		t.Fatalf("deepseek client: %v", err)
	}
}

func TestAISettingsOllamaWithoutAPIKey(t *testing.T) {
	var authHeader string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if r.URL.Path != "/api/chat" {
			t.Fatalf("ollama path=%q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "qwen2.5",
			"message": map[string]any{"role": "assistant", "content": "pong"},
			"done":    true,
		})
	}))
	defer ollama.Close()

	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	admin := sessionCookie(t, store, "ollama-admin", "password", "admin")
	gw := newMetadataTestGateway(store, Config{
		Tokens:          store,
		Store:           store,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
		AITimeout:       time.Second,
		OutboundPolicy:  outbound.Policy{AllowPrivate: true},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	// Production mode (box != nil) allows enabling ollama with no API key.
	resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled":  true,
		"provider": "ollama",
		"base_url": ollama.URL,
		"model":    "qwen2.5",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable ollama without key status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"enabled":true`)) || !bytes.Contains(body, []byte(`"has_api_key":false`)) {
		t.Fatalf("ollama settings body=%s", body)
	}

	resp, body = requestJSON(t, gw, http.MethodPost, "/api/ai/test", admin, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"echo":"pong"`)) {
		t.Fatalf("ollama test status=%d body=%s", resp.StatusCode, body)
	}
	if authHeader != "" {
		t.Fatalf("ollama request sent unexpected Authorization header: %q", authHeader)
	}
}

func TestAISettingsCorruptCiphertextAllowsDisable(t *testing.T) {
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	admin := sessionCookie(t, store, "corrupt-admin", "password", "admin")
	if err := store.UpsertAISettings(t.Context(), meta.AISettings{
		Enabled:          true,
		Provider:         "deepseek",
		Model:            "deepseek-chat",
		APIKeyCiphertext: "corrupt-unreadable-ciphertext",
		UpdatedAt:        time.Now().UTC(),
		UpdatedBy:        "admin",
	}); err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{
		Tokens:          store,
		Store:           store,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
		AITimeout:       time.Second,
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	// Admin must be able to disable AI without first decrypting corrupt ciphertext.
	resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable with corrupt ciphertext status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"enabled":false`)) || !bytes.Contains(body, []byte(`"has_api_key":false`)) {
		t.Fatalf("disabled settings body=%s", body)
	}
}

func TestAISettingsRejectsSSRFBaseURL(t *testing.T) {
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	admin := sessionCookie(t, store, "ssrf-ai-admin", "password", "admin")
	gw := newMetadataTestGateway(store, Config{
		Tokens:          store,
		Store:           store,
		AISettingsStore: store,
		AISecretBox:     box,
		AIBuildClient:   ai.New,
		AITimeout:       time.Second,
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	for _, badURL := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:18428",
		"http://10.0.0.5:8080",
		"http://api.openai.com:0443",
	} {
		resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
			"enabled":  true,
			"provider": "openai",
			"base_url": badURL,
			"model":    "gpt-4o",
			"api_key":  "sk-secret",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("base_url=%q status=%d want 400 body=%s", badURL, resp.StatusCode, body)
		}
	}
}

func TestAISettingsConcurrentReadWriteNoRaceAndNoLostUpdates(t *testing.T) {
	provider, gw, _, admin, _, key := dynamicAITestApp(t)
	resp, body := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true, "provider": "openai", "base_url": provider.URL, "model": "initial-model",
		"api_key": key, "max_tokens": 512,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed settings status=%d body=%s", resp.StatusCode, body)
	}

	var initial struct {
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(body, &initial); err != nil || initial.Revision == 0 {
		t.Fatalf("expected positive revision in response: %s", body)
	}

	// Stale expected_revision must return 409 Conflict.
	staleRev := initial.Revision + 99
	resp, body = requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"model":             "should-conflict",
		"expected_revision": staleRev,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale expected_revision status=%d want 409 body=%s", resp.StatusCode, body)
	}

	// Concurrent readers (GET /api/ai/settings) + concurrent disjoint PUTs must
	// not race on updatedBy and must preserve both disjoint updates.
	var wg sync.WaitGroup
	stopReaders := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					r, _ := requestJSON(t, gw, http.MethodGet, "/api/ai/settings", admin, nil)
					if r.StatusCode != http.StatusOK {
						t.Errorf("concurrent GET status=%d", r.StatusCode)
					}
				}
			}
		}()
	}

	var putWG sync.WaitGroup
	putWG.Add(2)
	go func() {
		defer putWG.Done()
		r, b := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
			"model": "updated-model",
		})
		if r.StatusCode != http.StatusOK {
			t.Errorf("concurrent model PUT status=%d body=%s", r.StatusCode, b)
		}
	}()
	go func() {
		defer putWG.Done()
		r, b := requestJSON(t, gw, http.MethodPut, "/api/ai/settings", admin, map[string]any{
			"max_tokens": 2048,
		})
		if r.StatusCode != http.StatusOK {
			t.Errorf("concurrent max_tokens PUT status=%d body=%s", r.StatusCode, b)
		}
	}()
	putWG.Wait()
	close(stopReaders)
	wg.Wait()

	state := gw.aiState()
	if state.model != "updated-model" || state.maxTokens != 2048 || state.updatedBy != "ai-settings-admin" {
		t.Fatalf("lost concurrent update: state=%#v", state)
	}
}

func TestAISettingsHAReplicaRefreshAndKeyRevocation(t *testing.T) {
	var lastAuth atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "ha-model",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "pong"}}},
		})
	}))
	defer provider.Close()

	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	admin := sessionCookie(t, store, "ha-ai-admin", "password", "admin")

	newReplica := func() *Gateway {
		gw := newMetadataTestGateway(store, Config{
			Tokens:                    store,
			Store:                     store,
			AISettingsStore:           store,
			AISecretBox:               box,
			AIBuildClient:             ai.New,
			AITimeout:                 time.Second,
			AISettingsRefreshInterval: 20 * time.Millisecond,
			OutboundPolicy:            outbound.Policy{AllowPrivate: true},
		})
		t.Cleanup(func() { _ = gw.Shutdown() })
		return gw
	}
	replicaA := newReplica()
	replicaB := newReplica()

	// Enable AI on replica A with key-1.
	resp, body := requestJSON(t, replicaA, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": true, "provider": "openai", "base_url": provider.URL, "model": "ha-model", "api_key": "key-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replicaA enable status=%d body=%s", resp.StatusCode, body)
	}

	// Replica B must pick up the new settings automatically via background HA sync.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if st := replicaB.aiState(); st.enabled && st.client != nil && st.model == "ha-model" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replicaB did not sync enabled AI settings: %#v", replicaB.aiState())
		}
		time.Sleep(15 * time.Millisecond)
	}
	resp, body = requestJSON(t, replicaB, http.MethodPost, "/api/ai/test", admin, nil)
	if resp.StatusCode != http.StatusOK || lastAuth.Load() != "Bearer key-1" {
		t.Fatalf("replicaB test status=%d auth=%v body=%s", resp.StatusCode, lastAuth.Load(), body)
	}

	// Rotate key on replica A, then revoke and disable.
	resp, body = requestJSON(t, replicaA, http.MethodPut, "/api/ai/settings", admin, map[string]any{
		"enabled": false, "clear_api_key": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replicaA revoke status=%d body=%s", resp.StatusCode, body)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		if st := replicaB.aiState(); !st.enabled && st.client == nil && !st.hasKey {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replicaB did not sync key revocation: %#v", replicaB.aiState())
		}
		time.Sleep(15 * time.Millisecond)
	}
}
