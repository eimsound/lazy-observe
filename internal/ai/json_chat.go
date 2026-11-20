package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const (
	defaultJSONChatAttempts       = 2
	defaultJSONRepairExcerptRunes = 1000
	defaultJSONRepairPrompt       = "Your response violated the JSON contract. Return only one corrected JSON object."
)

// JSONContractError indicates the model responded without transport/provider
// errors, but every attempt failed strict JSON decoding or validation.
type JSONContractError struct {
	Raw   string
	Cause error
}

func (e *JSONContractError) Error() string {
	if e != nil && e.Cause != nil {
		return "AI output violated JSON contract: " + e.Cause.Error()
	}
	return "AI output violated JSON contract"
}

func (e *JSONContractError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ChatJSONRequest configures a typed JSON completion with bounded repair retries.
type ChatJSONRequest[T any] struct {
	System                string
	Messages              []Message
	Temperature           float64
	MaxAttempts           int
	RepairPrompt          string
	AssistantExcerptRunes int
	Validate              func(T) (T, error)
}

// ChatJSONResult carries the decoded value alongside aggregated token usage
// across all attempts (including repair retries).
type ChatJSONResult[T any] struct {
	Value    T
	Raw      string
	Model    string
	Usage    Usage
	Attempts int
}

// DecodeStrictJSONObject scans raw for the first JSON object that decodes into
// T with DisallowUnknownFields enabled and passes validate (when non-nil).
func DecodeStrictJSONObject[T any](raw string, validate func(T) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for i, r := range raw {
		if r != '{' {
			continue
		}
		var candidate T
		decoder := json.NewDecoder(strings.NewReader(raw[i:]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&candidate); err != nil {
			lastErr = err
			continue
		}
		if validate != nil {
			normalized, err := validate(candidate)
			if err != nil {
				lastErr = err
				continue
			}
			return normalized, nil
		}
		return candidate, nil
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, errors.New("no valid JSON object found")
}

// ChatJSON invokes client.Chat and enforces a strict JSON contract for T,
// retrying with a repair turn up to MaxAttempts (default 2) and summing token
// usage across attempts.
func ChatJSON[T any](ctx context.Context, client Client, req ChatJSONRequest[T]) (ChatJSONResult[T], error) {
	var zero ChatJSONResult[T]
	if client == nil {
		return zero, errors.New("AI client is nil")
	}
	attempts := req.MaxAttempts
	if attempts <= 0 {
		attempts = defaultJSONChatAttempts
	}
	excerptLimit := req.AssistantExcerptRunes
	if excerptLimit <= 0 {
		excerptLimit = defaultJSONRepairExcerptRunes
	}
	repairPrompt := strings.TrimSpace(req.RepairPrompt)
	if repairPrompt == "" {
		repairPrompt = defaultJSONRepairPrompt
	}

	messages := append([]Message(nil), req.Messages...)
	var (
		totalUsage Usage
		raw        string
		model      string
		lastErr    error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := client.Chat(ctx, ChatRequest{
			System:      req.System,
			Messages:    messages,
			Temperature: req.Temperature,
		})
		if err != nil {
			return ChatJSONResult[T]{
				Raw:      raw,
				Model:    model,
				Usage:    totalUsage,
				Attempts: attempt + 1,
			}, err
		}
		totalUsage = totalUsage.Add(resp.Usage)
		raw = resp.Content
		if resp.Model != "" {
			model = resp.Model
		}
		parsed, parseErr := DecodeStrictJSONObject(raw, req.Validate)
		if parseErr == nil {
			return ChatJSONResult[T]{
				Value:    parsed,
				Raw:      raw,
				Model:    model,
				Usage:    totalUsage,
				Attempts: attempt + 1,
			}, nil
		}
		lastErr = parseErr
		if attempt+1 < attempts {
			messages = append(
				messages,
				Message{Role: "assistant", Content: FirstRunes(raw, excerptLimit)},
				Message{Role: "user", Content: repairPrompt},
			)
		}
	}
	return ChatJSONResult[T]{
		Raw:      raw,
		Model:    model,
		Usage:    totalUsage,
		Attempts: attempts,
	}, &JSONContractError{Raw: raw, Cause: lastErr}
}
