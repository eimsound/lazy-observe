package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestProviderRequestConstructionAndResponseParsing(t *testing.T) {
	tests := []struct {
		name, provider, path, response, wantContent, wantModel string
		check                                                  func(*testing.T, *http.Request, map[string]any)
	}{
		{
			name:        "openai",
			provider:    "openai",
			path:        "/v1/chat/completions",
			response:    `{"model":"gpt-test","choices":[{"message":{"role":"assistant","content":"pong-openai"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			wantContent: "pong-openai",
			wantModel:   "gpt-test",
			check: func(t *testing.T, r *http.Request, body map[string]any) {
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
				}
				messages := body["messages"].([]any)
				if messages[0].(map[string]any)["role"] != "system" || body["max_tokens"] != float64(77) {
					t.Fatalf("body=%#v", body)
				}
			},
		},
		{
			name:        "anthropic",
			provider:    "anthropic",
			path:        "/v1/messages",
			response:    `{"model":"claude-test","content":[{"type":"text","text":"pong-"},{"type":"text","text":"anthropic"}],"usage":{"input_tokens":4,"output_tokens":2}}`,
			wantContent: "pong-anthropic",
			wantModel:   "claude-test",
			check: func(t *testing.T, r *http.Request, body map[string]any) {
				if r.Header.Get("x-api-key") != "test-secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Fatalf("headers=%v", r.Header)
				}
				if body["system"] != "be concise" || body["max_tokens"] != float64(77) {
					t.Fatalf("body=%#v", body)
				}
			},
		},
		{
			name:        "ollama",
			provider:    "ollama",
			path:        "/api/chat",
			response:    `{"model":"qwen-test","message":{"role":"assistant","content":"pong-ollama"},"prompt_eval_count":5,"eval_count":2,"done":true}`,
			wantContent: "pong-ollama",
			wantModel:   "qwen-test",
			check: func(t *testing.T, r *http.Request, body map[string]any) {
				if r.Header.Get("Authorization") != "" {
					t.Fatalf("unexpected authorization header")
				}
				messages := body["messages"].([]any)
				options := body["options"].(map[string]any)
				if messages[0].(map[string]any)["role"] != "system" || options["num_predict"] != float64(77) {
					t.Fatalf("body=%#v", body)
				}
			},
		},
		{
			name:        "deepseek",
			provider:    "deepseek",
			path:        "/v1/chat/completions",
			response:    `{"model":"deepseek-chat","choices":[{"message":{"role":"assistant","content":"pong-deepseek"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			wantContent: "pong-deepseek",
			wantModel:   "deepseek-chat",
			check: func(t *testing.T, r *http.Request, body map[string]any) {
				if r.Header.Get("Authorization") != "Bearer deepseek-secret" {
					t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
				}
				messages := body["messages"].([]any)
				if messages[0].(map[string]any)["role"] != "system" || body["max_tokens"] != float64(77) {
					t.Fatalf("body=%#v", body)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Fatalf("path=%q want=%q", r.URL.Path, tc.path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["stream"] != false || body["model"] != "configured-model" ||
					body["temperature"] != 0.25 && tc.provider != "ollama" {
					t.Fatalf("body=%#v", body)
				}
				tc.check(t, r, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			key := "test-secret"
			switch tc.provider {
			case "ollama":
				key = ""
			case "deepseek":
				key = "deepseek-secret"
			}
			client, err := New(
				Config{
					Provider:  tc.provider,
					BaseURL:   server.URL,
					APIKey:    key,
					Model:     "configured-model",
					MaxTokens: 77,
					Timeout:   time.Second,
					Client:    server.Client(),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Chat(
				context.Background(),
				ChatRequest{
					System:      "be concise",
					Messages:    []Message{{Role: "user", Content: "ping"}},
					Temperature: 0.25,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if got.Content != tc.wantContent || got.Model != tc.wantModel || got.Usage.InputTokens == 0 ||
				got.Usage.OutputTokens == 0 {
				t.Fatalf("response=%#v", got)
			}
		})
	}
}

func TestProviderStreamingFraming(t *testing.T) {
	tests := []struct{ provider, contentType, stream string }{
		{
			"openai",
			"text/event-stream",
			"data: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\ndata: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n",
		},
		{
			"anthropic",
			"text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			"ollama",
			"application/x-ndjson",
			"{\"model\":\"qwen-test\",\"message\":{\"role\":\"assistant\",\"content\":\"hel\"},\"done\":false}\n{\"model\":\"qwen-test\",\"message\":{\"role\":\"assistant\",\"content\":\"lo\"},\"done\":false}\n{\"model\":\"qwen-test\",\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true}\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["stream"] != true {
					t.Fatalf("stream=%v", body["stream"])
				}
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.stream)
			}))
			defer server.Close()
			client, err := New(
				Config{
					Provider:  tc.provider,
					BaseURL:   server.URL,
					Model:     "test",
					MaxTokens: 20,
					Timeout:   time.Second,
					Client:    server.Client(),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := client.ChatStream(
				context.Background(),
				ChatRequest{Messages: []Message{{Role: "user", Content: "ping"}}},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var got strings.Builder
			for stream.Next() {
				got.WriteString(stream.Chunk().Content)
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
			if got.String() != "hello" {
				t.Fatalf("content=%q", got.String())
			}
		})
	}
}

func TestDeepseekDefaultBaseURL(t *testing.T) {
	// The OpenAI-compatible endpoint is built from the provider default.
	client, err := New(
		Config{Provider: "deepseek", APIKey: "sk-ds", Model: "deepseek-chat", MaxTokens: 20, Timeout: time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := client.(*providerClient)
	if got.baseURL != "https://api.deepseek.com" || got.protocol != protocolOpenAI {
		t.Fatalf("deepseek client = %+v", got)
	}
	if endpoint := got.endpoint(); endpoint != "https://api.deepseek.com/v1/chat/completions" {
		t.Fatalf("deepseek endpoint = %q", endpoint)
	}
}

func TestErrorNormalizationAndSecretRedaction(t *testing.T) {
	secret := "super-secret-key"
	tests := []struct {
		name   string
		status int
		body   string
		kind   ErrorKind
	}{
		{
			"unauthorized",
			http.StatusUnauthorized,
			`{"error":{"message":"bad key super-secret-key"}}`,
			ErrorUnauthorized,
		},
		{"rate limit", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, ErrorRateLimited},
		{
			"context",
			http.StatusBadRequest,
			`{"error":{"message":"maximum context length exceeded"}}`,
			ErrorContextTooLong,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client, _ := New(
				Config{
					Provider:  "openai",
					BaseURL:   server.URL,
					APIKey:    secret,
					Model:     "test",
					MaxTokens: 20,
					Timeout:   time.Second,
					Client:    server.Client(),
				},
			)
			_, err := client.Chat(context.Background(), ChatRequest{})
			if !IsKind(err, tc.kind) {
				t.Fatalf("error=%#v want kind=%s", err, tc.kind)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("secret leaked in error: %s", err)
			}
		})
	}
}

func TestTimeoutNormalization(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(100 * time.Millisecond) }),
	)
	defer server.Close()
	client, _ := New(
		Config{
			Provider:  "openai",
			BaseURL:   server.URL,
			Model:     "test",
			MaxTokens: 20,
			Timeout:   20 * time.Millisecond,
			Client:    server.Client(),
		},
	)
	_, err := client.Chat(context.Background(), ChatRequest{})
	if !IsKind(err, ErrorTimeout) {
		t.Fatalf("error=%#v", err)
	}
}

func TestSecretIsRedactedFromModelResponses(t *testing.T) {
	const secret = "response-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(
			w,
			`{"model":"test","choices":[{"message":{"role":"assistant","content":"echo response-secret"}}]}`,
		)
	}))
	defer server.Close()
	client, _ := New(
		Config{
			Provider:  "openai",
			BaseURL:   server.URL,
			APIKey:    secret,
			Model:     "test",
			MaxTokens: 20,
			Timeout:   time.Second,
			Client:    server.Client(),
		},
	)
	response, err := client.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Content, secret) || response.Content != "echo [REDACTED]" {
		t.Fatalf("response was not redacted: %q", response.Content)
	}
}

func TestProviderBlocksSSRFByDefault(t *testing.T) {
	for _, baseURL := range []string{
		"http://127.0.0.1:18428",
		"http://169.254.169.254",
		"http://10.0.0.1:8080",
		"http://example.com:080",
	} {
		if _, err := New(Config{
			Provider:  "openai",
			BaseURL:   baseURL,
			APIKey:    "sk-test",
			Model:     "gpt-4o",
			MaxTokens: 32,
			Timeout:   time.Second,
		}); err == nil {
			t.Errorf("New(%q) succeeded, want SSRF/port validation error", baseURL)
		}
	}
}

func TestProviderBlocksRedirectsAndNeverLeaksAnthropicAPIKey(t *testing.T) {
	var leakedKey string
	var secondHit bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
		leakedKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client, err := New(Config{
		Provider:  "anthropic",
		BaseURL:   redirector.URL,
		APIKey:    "sk-ant-top-secret",
		Model:     "claude-3-7-sonnet",
		MaxTokens: 32,
		Timeout:   time.Second,
		Outbound:  outbound.Policy{AllowPrivate: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "ping"}},
	})
	if !errors.Is(err, outbound.ErrRedirectBlocked) {
		t.Fatalf("err=%v want ErrRedirectBlocked", err)
	}
	if secondHit || leakedKey != "" {
		t.Fatalf("redirect target hit=%v leakedKey=%q", secondHit, leakedKey)
	}
}

func TestOpenAIReasoningModelParameters(t *testing.T) {
	reasoningModels := []string{"o1", "o1-mini", "o3-mini", "o4-mini", "gpt-5", "gpt-5-mini", "openai/gpt-5.1"}
	for _, model := range reasoningModels {
		t.Run(model, func(t *testing.T) {
			var captured map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(
					w,
					`{"model":"`+model+`","choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
				)
			}))
			defer srv.Close()

			client, err := New(Config{
				Provider:  "openai",
				BaseURL:   srv.URL,
				APIKey:    "sk-test",
				Model:     model,
				MaxTokens: 256,
				Timeout:   time.Second,
				Client:    srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Chat(context.Background(), ChatRequest{
				Messages:    []Message{{Role: "user", Content: "hi"}},
				Temperature: 0,
			}); err != nil {
				t.Fatal(err)
			}
			if captured["max_completion_tokens"] != float64(256) {
				t.Fatalf("max_completion_tokens=%#v want 256", captured["max_completion_tokens"])
			}
			if _, hasMaxTokens := captured["max_tokens"]; hasMaxTokens {
				t.Fatalf("reasoning model must not send max_tokens: %#v", captured)
			}
			if _, hasTemp := captured["temperature"]; hasTemp {
				t.Fatalf("reasoning model must not send fixed temperature: %#v", captured)
			}
		})
	}
}

func TestOllamaOmitsAuthorizationHeaderWhenKeyWhitespace(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"model":"llama3","message":{"role":"assistant","content":"pong"},"done":true}`)
	}))
	defer srv.Close()

	client, err := New(Config{
		Provider:  "ollama",
		BaseURL:   srv.URL,
		APIKey:    "   ",
		Model:     "llama3",
		MaxTokens: 32,
		Timeout:   time.Second,
		Client:    srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "ping"}},
	}); err != nil {
		t.Fatal(err)
	}
	if authHeader != "" {
		t.Fatalf("unexpected Authorization header %q", authHeader)
	}
}

func TestChatStreamAllowsLongRunningActiveStreamAndEnforcesIdleAndHeaderTimeouts(t *testing.T) {
	// Active stream lasting longer than Timeout must succeed because chunks keep arriving.
	activeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, piece := range []string{"a", "b", "c", "d", "e"} {
			_, _ = io.WriteString(
				w,
				"data: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":\""+piece+"\"},\"finish_reason\":null}]}\n\n",
			)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(25 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer activeServer.Close()

	client, err := New(Config{
		Provider:  "openai",
		BaseURL:   activeServer.URL,
		Model:     "gpt-test",
		MaxTokens: 32,
		Timeout:   70 * time.Millisecond,
		Client:    activeServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.ChatStream(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "stream"}},
	})
	if err != nil {
		t.Fatalf("active long stream failed to open: %v", err)
	}
	var out strings.Builder
	for stream.Next() {
		out.WriteString(stream.Chunk().Content)
	}
	streamErr := stream.Err()
	_ = stream.Close()
	if streamErr != nil || out.String() != "abcde" {
		t.Fatalf("active long stream got=%q err=%v want abcde/nil", out.String(), streamErr)
	}

	// Idle stall between chunks exceeding StreamIdleTimeout must fail with ErrorTimeout.
	stalledServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(
			w,
			"data: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n\n",
		)
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer stalledServer.Close()

	idleClient, err := New(Config{
		Provider:          "openai",
		BaseURL:           stalledServer.URL,
		Model:             "gpt-test",
		MaxTokens:         32,
		Timeout:           200 * time.Millisecond,
		StreamIdleTimeout: 50 * time.Millisecond,
		Client:            stalledServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stalledStream, err := idleClient.ChatStream(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "stall"}},
	})
	if err != nil {
		t.Fatalf("stalled stream open failed: %v", err)
	}
	if !stalledStream.Next() || stalledStream.Chunk().Content != "first" {
		t.Fatalf("expected first chunk before stall, err=%v", stalledStream.Err())
	}
	if stalledStream.Next() {
		t.Fatal("expected stalled stream to stop with timeout")
	}
	if !IsKind(stalledStream.Err(), ErrorTimeout) {
		t.Fatalf("stalled stream err=%#v want ErrorTimeout", stalledStream.Err())
	}
	_ = stalledStream.Close()

	// Delayed response header exceeding Timeout must fail with ErrorTimeout.
	slowHeaderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slowHeaderServer.Close()

	headerClient, err := New(Config{
		Provider:  "openai",
		BaseURL:   slowHeaderServer.URL,
		Model:     "gpt-test",
		MaxTokens: 32,
		Timeout:   40 * time.Millisecond,
		Client:    slowHeaderServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := headerClient.ChatStream(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "header-timeout"}},
	}); !IsKind(err, ErrorTimeout) {
		t.Fatalf("slow header stream err=%#v want ErrorTimeout", err)
	}
}

func TestChatStreamPropagatesProviderErrorEventsAndRedactsSecrets(t *testing.T) {
	const secret = "sk-stream-secret-value"
	cases := []struct {
		name     string
		provider string
		body     string
		wantKind ErrorKind
		wantSub  string
	}{
		{
			name:     "anthropic overloaded error event",
			provider: "anthropic",
			body: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded for sk-stream-secret-value\"}}\n\n",
			wantKind: ErrorProvider,
			wantSub:  "Overloaded for [REDACTED]",
		},
		{
			name:     "anthropic rate limit error event",
			provider: "anthropic",
			body:     "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"too fast\"}}\n\n",
			wantKind: ErrorRateLimited,
			wantSub:  "too fast",
		},
		{
			name:     "openai stream error payload",
			provider: "openai",
			body:     "data: {\"error\":{\"type\":\"invalid_api_key\",\"message\":\"bad key sk-stream-secret-value\"}}\n\n",
			wantKind: ErrorUnauthorized,
			wantSub:  "bad key [REDACTED]",
		},
		{
			name:     "ollama ndjson error line",
			provider: "ollama",
			body:     "{\"error\":\"prompt is too long for sk-stream-secret-value\"}\n",
			wantKind: ErrorContextTooLong,
			wantSub:  "prompt is too long for [REDACTED]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			client, err := New(Config{
				Provider:  tc.provider,
				BaseURL:   srv.URL,
				APIKey:    secret,
				Model:     "test-model",
				MaxTokens: 32,
				Timeout:   time.Second,
				Client:    srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := client.ChatStream(context.Background(), ChatRequest{
				Messages: []Message{{Role: "user", Content: "ping"}},
			})
			if err != nil {
				t.Fatalf("ChatStream open err=%v", err)
			}
			defer stream.Close()
			for stream.Next() {
			}
			streamErr := stream.Err()
			if !IsKind(streamErr, tc.wantKind) {
				t.Fatalf("stream.Err() = %#v, want kind %s", streamErr, tc.wantKind)
			}
			if strings.Contains(streamErr.Error(), secret) {
				t.Fatalf("stream error leaked secret: %q", streamErr.Error())
			}
			if !strings.Contains(streamErr.Error(), tc.wantSub) {
				t.Fatalf("stream error %q missing %q", streamErr.Error(), tc.wantSub)
			}
		})
	}
}
