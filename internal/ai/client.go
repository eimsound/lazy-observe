// Package ai provides a provider-neutral, read-only LLM client.
package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type ChatResponse struct {
	Content string `json:"content"`
	Model   string `json:"model,omitempty"`
	Usage   Usage  `json:"usage,omitempty"`
}

type ChatChunk struct {
	Content string `json:"content"`
	Model   string `json:"model,omitempty"`
}

// Stream is a pull iterator. Callers must close it, including after Next returns false.
type Stream interface {
	Next() bool
	Chunk() ChatChunk
	Err() error
	Close() error
}

type Client interface {
	Chat(context.Context, ChatRequest) (ChatResponse, error)
	ChatStream(context.Context, ChatRequest) (Stream, error)
}

type Config struct {
	Provider  string
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	Timeout   time.Duration
	Client    *http.Client
}

func New(cfg Config) (Client, error) {
	if cfg.MaxTokens <= 0 {
		return nil, errors.New("max tokens must be positive")
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("model is required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	} else {
		clientCopy := *cfg.Client
		clientCopy.Timeout = cfg.Timeout
		cfg.Client = &clientCopy
	}
	b := &providerClient{
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		maxTokens: cfg.MaxTokens,
		client:    cfg.Client,
	}
	switch cfg.Provider {
	case "openai":
		if b.baseURL == "" {
			b.baseURL = "https://api.openai.com"
		}
		b.protocol = protocolOpenAI
	case "deepseek":
		// DeepSeek exposes an OpenAI-compatible chat completions API.
		if b.baseURL == "" {
			b.baseURL = "https://api.deepseek.com"
		}
		b.protocol = protocolOpenAI
	case "anthropic":
		if b.baseURL == "" {
			b.baseURL = "https://api.anthropic.com"
		}
		b.protocol = protocolAnthropic
	case "ollama":
		if b.baseURL == "" {
			b.baseURL = "http://127.0.0.1:11434"
		}
		b.protocol = protocolOllama
	default:
		return nil, fmt.Errorf("unsupported AI provider %q", cfg.Provider)
	}
	return b, nil
}

type ErrorKind string

const (
	ErrorTimeout        ErrorKind = "timeout"
	ErrorUnauthorized   ErrorKind = "unauthorized"
	ErrorRateLimited    ErrorKind = "rate_limited"
	ErrorContextTooLong ErrorKind = "context_too_long"
	ErrorProvider       ErrorKind = "provider"
	ErrorProtocol       ErrorKind = "protocol"
)

// Error normalizes failures that callers may want to present or retry differently.
type Error struct {
	Kind       ErrorKind `json:"kind"`
	StatusCode int       `json:"status_code,omitempty"`
	Message    string    `json:"message"`
	Err        error     `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

func IsKind(err error, kind ErrorKind) bool {
	var target *Error
	return errors.As(err, &target) && target.Kind == kind
}
