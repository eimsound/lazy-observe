package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

type idleTimeoutReadCloser struct {
	body        io.ReadCloser
	idleTimeout time.Duration
	timedOut    atomic.Bool
}

func wrapIdleTimeout(body io.ReadCloser, idleTimeout time.Duration) io.ReadCloser {
	if body == nil || idleTimeout <= 0 {
		return body
	}
	return &idleTimeoutReadCloser{body: body, idleTimeout: idleTimeout}
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	if r.timedOut.Load() {
		return 0, context.DeadlineExceeded
	}
	timer := time.AfterFunc(r.idleTimeout, func() {
		r.timedOut.Store(true)
		_ = r.body.Close()
	})
	n, err := r.body.Read(p)
	timer.Stop()
	if r.timedOut.Load() {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	return r.body.Close()
}

type responseStream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	protocol  protocol
	chunk     ChatChunk
	err       error
	done      bool
	secret    string
	stopNext  bool
	eventType string
}

func newStream(body io.ReadCloser, protocol protocol, secret string) Stream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &responseStream{body: body, scanner: scanner, protocol: protocol, secret: secret}
}

func (s *responseStream) Next() bool {
	if s.stopNext {
		s.done = true
		return false
	}
	if s.done || s.err != nil {
		return false
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" {
			s.eventType = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			s.eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			s.done = true
			return false
		}
		eventType := s.eventType
		chunk, emit, done, err := parseChunk(s.protocol, eventType, []byte(line), s.secret)
		if err != nil {
			var typed *Error
			if errors.As(err, &typed) {
				s.err = typed
			} else {
				s.err = protocolError("decode streaming response", err)
			}
			return false
		}
		if emit {
			if s.secret != "" {
				chunk.Content = strings.ReplaceAll(chunk.Content, s.secret, "[REDACTED]")
			}
			s.chunk = chunk
			s.stopNext = done
			return true
		}
		if done {
			s.done = true
			return false
		}
	}
	if err := s.scanner.Err(); err != nil {
		s.err = normalizeTransport(err)
	}
	s.done = true
	return false
}

func (s *responseStream) Chunk() ChatChunk { return s.chunk }
func (s *responseStream) Err() error       { return s.err }
func (s *responseStream) Close() error     { return s.body.Close() }

func parseChunk(protocol protocol, eventType string, data []byte, secret string) (ChatChunk, bool, bool, error) {
	switch protocol {
	case protocolAnthropic:
		var value struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatChunk{}, false, false, err
		}
		if value.Type == "error" || strings.EqualFold(eventType, "error") || value.Error != nil {
			errType, msg := "", ""
			if value.Error != nil {
				errType, msg = value.Error.Type, value.Error.Message
			}
			return ChatChunk{}, false, false, normalizeStreamError(errType, msg, data, secret)
		}
		if value.Type == "message_stop" {
			return ChatChunk{}, false, true, nil
		}
		return ChatChunk{
			Content: value.Delta.Text,
		}, value.Type == "content_block_delta" && value.Delta.Type == "text_delta" &&
			value.Delta.Text != "", false, nil
	case protocolOllama:
		var value struct {
			Model   string  `json:"model"`
			Message Message `json:"message"`
			Done    bool    `json:"done"`
			Error   string  `json:"error"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatChunk{}, false, false, err
		}
		if strings.TrimSpace(value.Error) != "" {
			return ChatChunk{}, false, false, normalizeStreamError("", value.Error, data, secret)
		}
		return ChatChunk{
			Content: value.Message.Content,
			Model:   value.Model,
		}, value.Message.Content != "", value.Done, nil
	default:
		var value struct {
			Model   string          `json:"model"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatChunk{}, false, false, err
		}
		trimmedErr := bytes.TrimSpace(value.Error)
		if (len(trimmedErr) > 0 && !bytes.Equal(trimmedErr, []byte("null"))) || strings.EqualFold(eventType, "error") {
			errType, msg := extractStreamErrorDetails(trimmedErr)
			return ChatChunk{}, false, false, normalizeStreamError(errType, msg, data, secret)
		}
		if len(value.Choices) == 0 {
			return ChatChunk{}, false, false, nil
		}
		content := value.Choices[0].Delta.Content
		return ChatChunk{Content: content, Model: value.Model}, content != "", value.Choices[0].FinishReason != nil, nil
	}
}

func extractStreamErrorDetails(raw []byte) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var nested struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &nested) == nil && (nested.Message != "" || nested.Type != "" || nested.Code != "") {
		errType := nested.Type
		if errType == "" {
			errType = nested.Code
		}
		return errType, nested.Message
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil && plain != "" {
		return "", plain
	}
	return "", strings.TrimSpace(string(raw))
}

func normalizeStreamError(errType, message string, raw []byte, secret string) *Error {
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = strings.TrimSpace(errType)
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = "stream error"
	}
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
	}
	lowerType := strings.ToLower(strings.TrimSpace(errType))
	lowerAll := strings.ToLower(lowerType + " " + msg + " " + string(raw))
	kind := ErrorProvider
	switch {
	case lowerType == "authentication_error" || lowerType == "permission_error" || lowerType == "invalid_api_key":
		kind = ErrorUnauthorized
	case lowerType == "rate_limit_error" || strings.Contains(lowerAll, "rate_limit") || strings.Contains(lowerAll, "rate limit"):
		kind = ErrorRateLimited
	case strings.Contains(lowerAll, "context_length") || strings.Contains(lowerAll, "context length") ||
		strings.Contains(lowerAll, "maximum context") || strings.Contains(lowerAll, "too many tokens") ||
		strings.Contains(lowerAll, "prompt is too long"):
		kind = ErrorContextTooLong
	}
	return &Error{Kind: kind, Message: fmt.Sprintf("AI provider stream: %s", msg)}
}
