package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/outbound"
)

type protocol uint8

const (
	protocolOpenAI protocol = iota
	protocolAnthropic
	protocolOllama
)

type providerClient struct {
	protocol          protocol
	baseURL           string
	apiKey            string
	model             string
	maxTokens         int
	client            *http.Client
	streamClient      *http.Client
	streamIdleTimeout time.Duration
}

func (p *providerClient) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	body, err := p.requestBody(request, false)
	if err != nil {
		return ChatResponse{}, err
	}
	resp, err := p.do(ctx, p.client, body)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ChatResponse{}, normalizeTransport(err)
	}
	result, err := p.parseResponse(data)
	if p.apiKey != "" {
		result.Content = strings.ReplaceAll(result.Content, p.apiKey, "[REDACTED]")
	}
	return result, err
}

func (p *providerClient) ChatStream(ctx context.Context, request ChatRequest) (Stream, error) {
	body, err := p.requestBody(request, true)
	if err != nil {
		return nil, err
	}
	httpClient := p.streamClient
	if httpClient == nil {
		httpClient = p.client
	}
	resp, err := p.do(ctx, httpClient, body)
	if err != nil {
		return nil, err
	}
	streamBody := wrapIdleTimeout(resp.Body, p.streamIdleTimeout)
	return newStream(streamBody, p.protocol, p.apiKey), nil
}

func (p *providerClient) do(ctx context.Context, httpClient *http.Client, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Kind: ErrorProvider, Message: "build provider request", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	switch p.protocol {
	case protocolAnthropic:
		if p.apiKey != "" {
			req.Header.Set("x-api-key", p.apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, normalizeTransport(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	errBody := wrapIdleTimeout(resp.Body, p.streamIdleTimeout)
	defer errBody.Close()
	data, _ := io.ReadAll(io.LimitReader(errBody, 1<<20))
	return nil, normalizeHTTP(resp.StatusCode, data, p.apiKey)
}

func (p *providerClient) endpoint() string {
	switch p.protocol {
	case protocolAnthropic:
		return appendEndpoint(p.baseURL, "/v1/messages")
	case protocolOllama:
		return appendEndpoint(p.baseURL, "/api/chat")
	default:
		return appendEndpoint(p.baseURL, "/v1/chat/completions")
	}
}

func appendEndpoint(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		return base + strings.TrimPrefix(path, "/v1")
	}
	if strings.HasSuffix(base, "/api") && strings.HasPrefix(path, "/api/") {
		return base + strings.TrimPrefix(path, "/api")
	}
	return base + path
}

func (p *providerClient) requestBody(request ChatRequest, stream bool) ([]byte, error) {
	var payload any
	switch p.protocol {
	case protocolAnthropic:
		payload = struct {
			Model       string    `json:"model"`
			System      string    `json:"system,omitempty"`
			Messages    []Message `json:"messages"`
			MaxTokens   int       `json:"max_tokens"`
			Temperature float64   `json:"temperature"`
			Stream      bool      `json:"stream"`
		}{p.model, request.System, request.Messages, p.maxTokens, request.Temperature, stream}
	case protocolOllama:
		messages := withSystem(request)
		payload = struct {
			Model    string         `json:"model"`
			Messages []Message      `json:"messages"`
			Stream   bool           `json:"stream"`
			Options  map[string]any `json:"options"`
		}{p.model, messages, stream, map[string]any{"temperature": request.Temperature, "num_predict": p.maxTokens}}
	default:
		if isOpenAIReasoningModel(p.model) {
			payload = struct {
				Model               string    `json:"model"`
				Messages            []Message `json:"messages"`
				MaxCompletionTokens int       `json:"max_completion_tokens"`
				Stream              bool      `json:"stream"`
			}{p.model, withSystem(request), p.maxTokens, stream}
		} else {
			payload = struct {
				Model       string    `json:"model"`
				Messages    []Message `json:"messages"`
				MaxTokens   int       `json:"max_tokens"`
				Temperature float64   `json:"temperature"`
				Stream      bool      `json:"stream"`
			}{p.model, withSystem(request), p.maxTokens, request.Temperature, stream}
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, &Error{Kind: ErrorProtocol, Message: "encode provider request", Err: err}
	}
	return data, nil
}

func isOpenAIReasoningModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
		if name == prefix || strings.HasPrefix(name, prefix+"-") || strings.HasPrefix(name, prefix+".") {
			return true
		}
	}
	return false
}

func withSystem(request ChatRequest) []Message {
	result := make([]Message, 0, len(request.Messages)+1)
	if request.System != "" {
		result = append(result, Message{Role: "system", Content: request.System})
	}
	return append(result, request.Messages...)
}

func (p *providerClient) parseResponse(data []byte) (ChatResponse, error) {
	switch p.protocol {
	case protocolAnthropic:
		var value struct {
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatResponse{}, protocolError("decode Anthropic response", err)
		}
		var content strings.Builder
		for _, part := range value.Content {
			if part.Type == "text" {
				content.WriteString(part.Text)
			}
		}
		return ChatResponse{
			Content: content.String(),
			Model:   value.Model,
			Usage:   Usage{InputTokens: value.Usage.Input, OutputTokens: value.Usage.Output},
		}, nil
	case protocolOllama:
		var value struct {
			Model      string  `json:"model"`
			Message    Message `json:"message"`
			PromptEval int     `json:"prompt_eval_count"`
			Eval       int     `json:"eval_count"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatResponse{}, protocolError("decode Ollama response", err)
		}
		return ChatResponse{
			Content: value.Message.Content,
			Model:   value.Model,
			Usage:   Usage{InputTokens: value.PromptEval, OutputTokens: value.Eval},
		}, nil
	default:
		var value struct {
			Model   string `json:"model"`
			Choices []struct {
				Message Message `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatResponse{}, protocolError("decode OpenAI response", err)
		}
		if len(value.Choices) == 0 {
			return ChatResponse{}, protocolError("OpenAI response has no choices", nil)
		}
		return ChatResponse{
			Content: value.Choices[0].Message.Content,
			Model:   value.Model,
			Usage:   Usage{InputTokens: value.Usage.Prompt, OutputTokens: value.Usage.Completion},
		}, nil
	}
}

func normalizeTransport(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, outbound.ErrBlockedAddress) || errors.Is(err, outbound.ErrRedirectBlocked) ||
		errors.Is(err, outbound.ErrInvalidTarget) {
		return &Error{Kind: ErrorProvider, Message: outbound.SanitizeError(err), Err: err}
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &Error{Kind: ErrorTimeout, Message: "AI provider request timed out", Err: err}
	}
	return &Error{Kind: ErrorProvider, Message: "AI provider request failed", Err: err}
}

func normalizeHTTP(status int, data []byte, secret string) error {
	message := strings.TrimSpace(string(data))
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) != 0 {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
			message = nested.Message
		} else {
			var plain string
			if json.Unmarshal(envelope.Error, &plain) == nil && plain != "" {
				message = plain
			}
		}
	}
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	if message == "" {
		message = http.StatusText(status)
	}
	kind := ErrorProvider
	lower := strings.ToLower(message + " " + string(data))
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = ErrorUnauthorized
	case status == http.StatusTooManyRequests:
		kind = ErrorRateLimited
	case status == http.StatusRequestEntityTooLarge || strings.Contains(lower, "context_length") || strings.Contains(lower, "context length") || strings.Contains(lower, "maximum context") || strings.Contains(lower, "too many tokens") || strings.Contains(lower, "prompt is too long"):
		kind = ErrorContextTooLong
	}
	return &Error{Kind: kind, StatusCode: status, Message: fmt.Sprintf("AI provider: %s", message)}
}

func protocolError(message string, err error) error {
	return &Error{Kind: ErrorProtocol, Message: message, Err: err}
}
