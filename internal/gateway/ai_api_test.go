package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
)

func TestAIStatusTestAndRBAC(t *testing.T) {
	const secret = "gateway-secret-must-not-leak"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatalf("provider request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("Reply with exactly: pong")) {
			t.Fatalf("connectivity prompt missing: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "fake-openai-model",
			"choices": []any{
				map[string]any{"message": map[string]any{"role": "assistant", "content": strings.Repeat("界", 101)}},
			},
		})
	}))
	defer provider.Close()
	client, err := ai.New(ai.Config{
		Provider: "openai", BaseURL: provider.URL, APIKey: secret,
		Model: "configured-model", MaxTokens: 1024, Timeout: time.Second, Client: provider.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	store := testStore(t)
	admin := sessionCookie(t, store, "ai-admin", "password", "admin")
	viewer := sessionCookie(t, store, "ai-viewer", "password", "viewer")
	app := newMetadataTestGateway(store,
		Config{
			Tokens:     store,
			Store:      store,
			AIEnabled:  true,
			AIProvider: "openai",
			AIModel:    "configured-model",
			AIClient:   client,
		},
	).App()

	request := func(method, path string, cookie *http.Cookie) (*http.Response, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("API response leaked key: %s", body)
		}
		return resp, body
	}

	resp, body := request(http.MethodGet, "/api/ai/status", viewer)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"enabled":true`)) ||
		bytes.Contains(body, []byte("api_key")) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	resp, _ = request(http.MethodPost, "/api/ai/test", viewer)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer test status=%d", resp.StatusCode)
	}
	resp, body = request(http.MethodPost, "/api/ai/test", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin test status=%d body=%s", resp.StatusCode, body)
	}
	var result struct {
		Latency int64  `json:"latency_ms"`
		Model   string `json:"model"`
		Echo    string `json:"echo"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Latency < 0 || result.Model != "fake-openai-model" || len([]rune(result.Echo)) != 100 {
		t.Fatalf("result=%#v", result)
	}
}

func TestAIDisabledReturnsConflict(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "disabled-ai-admin", "password", "admin")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, AIProvider: "openai"}).App()
	req := httptest.NewRequest(http.MethodPost, "/api/ai/test", nil)
	req = req.WithContext(context.Background())
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("AI integration is disabled")) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAIKeyNeverAppearsInErrorResponseOrLogs(t *testing.T) {
	const secret = "error-secret-must-not-leak"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"rejected error-secret-must-not-leak"}}`)
	}))
	defer provider.Close()
	client, _ := ai.New(
		ai.Config{
			Provider:  "openai",
			BaseURL:   provider.URL,
			APIKey:    secret,
			Model:     "test",
			MaxTokens: 20,
			Timeout:   time.Second,
			Client:    provider.Client(),
		},
	)
	store := testStore(t)
	admin := sessionCookie(t, store, "error-ai-admin", "password", "admin")
	app := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, AIEnabled: true, AIProvider: "openai", AIModel: "test", AIClient: client},
	).App()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	req := httptest.NewRequest(http.MethodPost, "/api/ai/test", nil)
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(secret)) || strings.Contains(logs.String(), secret) {
		t.Fatalf("secret leaked: response=%s logs=%s", body, logs.String())
	}
}
