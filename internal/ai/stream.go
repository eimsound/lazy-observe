package ai

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

type responseStream struct {
	body     io.ReadCloser
	scanner  *bufio.Scanner
	protocol protocol
	chunk    ChatChunk
	err      error
	done     bool
	secret   string
	stopNext bool
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
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			s.done = true
			return false
		}
		chunk, emit, done, err := parseChunk(s.protocol, []byte(line))
		if err != nil {
			s.err = protocolError("decode streaming response", err)
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

func parseChunk(protocol protocol, data []byte) (ChatChunk, bool, bool, error) {
	switch protocol {
	case protocolAnthropic:
		var value struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatChunk{}, false, false, err
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
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return ChatChunk{}, false, false, err
		}
		return ChatChunk{
			Content: value.Message.Content,
			Model:   value.Model,
		}, value.Message.Content != "", value.Done, nil
	default:
		var value struct {
			Model   string `json:"model"`
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
		if len(value.Choices) == 0 {
			return ChatChunk{}, false, false, nil
		}
		content := value.Choices[0].Delta.Content
		return ChatChunk{Content: content, Model: value.Model}, content != "", value.Choices[0].FinishReason != nil, nil
	}
}
