package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

const (
	llmLogsQLPath          = "/select/logsql/query"
	llmDefaultQueryTimeout = 30 * time.Second
	llmErrorSnippetBytes   = 4096
	llmErrorSnippetRunes   = 500
	llmBodyDrainLimit      = 64 << 10
)

var errLLMResponseTooLarge = errors.New("LLM query response exceeded size limit")

// llmTracesQuerier executes tenant-isolated streaming LogsQL queries directly
// against VictoriaTraces via storage.SignalBackend without coupling to
// ai_assistant.go internals.
type llmTracesQuerier struct {
	storage         storage.SignalBackend
	client          *http.Client
	maxResponseBody int64
	timeout         time.Duration
}

func newLLMTracesQuerier(
	backend storage.SignalBackend,
	client *http.Client,
	maxResponseBody int64,
	timeout time.Duration,
) *llmTracesQuerier {
	if maxResponseBody <= 0 {
		maxResponseBody = llmMaxResponseBody
	}
	if timeout <= 0 {
		timeout = llmDefaultQueryTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &llmTracesQuerier{
		storage:         backend,
		client:          client,
		maxResponseBody: maxResponseBody,
		timeout:         timeout,
	}
}

func (q *llmTracesQuerier) StreamLogsQL(
	ctx context.Context,
	project meta.ProjectInfo,
	query url.Values,
	onRow func(map[string]json.RawMessage) error,
) error {
	if q == nil || q.storage == nil {
		return errors.New("LLM trace storage backend is not configured")
	}
	if project.ID <= 0 {
		return fmt.Errorf("invalid project ID %d for tenant isolation: %w", project.ID, storage.ErrInvalidTenant)
	}
	base := strings.TrimRight(strings.TrimSpace(q.storage.TracesQueryBase()), "/")
	if base == "" {
		return errors.New("VictoriaTraces query URL is not configured")
	}

	params := cloneLLMQueryValues(query)
	// Ask VictoriaTraces (v0.12+, single-node and vtselect) to skip its 30s
	// -search.latencyOffset for LogsQL reads. Older versions ignore the argument.
	params.Set("disable_latency_offset", "true")

	target := base + llmLogsQLPath
	if encoded := params.Encode(); encoded != "" {
		target += "?" + encoded
	}

	queryCtx := ctx
	var cancel context.CancelFunc
	if q.timeout > 0 {
		queryCtx, cancel = context.WithTimeout(ctx, q.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(queryCtx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if err := q.storage.ApplyTenantHeaders(req.Header, project.ID); err != nil {
		return fmt.Errorf("apply tenant headers: %w", err)
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, llmBodyDrainLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, llmErrorSnippetBytes))
		return fmt.Errorf(
			"query backend returned HTTP %d: %s",
			resp.StatusCode,
			truncateLLMErrorSnippet(strings.TrimSpace(string(snippet)), llmErrorSnippetRunes),
		)
	}

	limited := &boundedStreamReader{reader: resp.Body, remaining: q.maxResponseBody}
	return decodeNDJSONStream(limited, onRow)
}

type boundedStreamReader struct {
	reader    io.Reader
	remaining int64
	exceeded  bool
}

func (r *boundedStreamReader) Read(p []byte) (int, error) {
	if r.exceeded {
		return 0, errLLMResponseTooLarge
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.reader.Read(p)
	if int64(n) > r.remaining {
		r.remaining = 0
		r.exceeded = true
		return 0, errLLMResponseTooLarge
	}
	r.remaining -= int64(n)
	return n, err
}

func decodeNDJSONStream(r io.Reader, onRow func(map[string]json.RawMessage) error) error {
	decoder := json.NewDecoder(r)
	for {
		var row map[string]json.RawMessage
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode VictoriaTraces span: %w", err)
		}
		if err := onRow(row); err != nil {
			return err
		}
	}
}

func cloneLLMQueryValues(source url.Values) url.Values {
	result := make(url.Values, len(source)+1)
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func truncateLLMErrorSnippet(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
