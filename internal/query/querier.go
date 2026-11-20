// Package query defines the project-isolated read-only telemetry query port
// shared across gateway handlers, AI Assistant, Issue AI, and MCP.
package query

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

// Signal selects which telemetry storage backend a read-only query targets.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
)

const (
	DefaultMaxResponseBytes int64 = 8 << 20
	DefaultBodyDrainLimit   int64 = 64 << 10
	MaxTraceIDLength              = 32
)

var (
	// MinTimestamp and MaxTimestamp guard query time bounds against epoch
	// overflow or malformed extreme timestamps.
	MinTimestamp = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	MaxTimestamp = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

	// ErrResponseTooLarge indicates an upstream query response exceeded the
	// caller's configured byte budget.
	ErrResponseTooLarge = errors.New("query response exceeded size limit")
)

// MetricsIsolator enforces project_id isolation on metrics query parameters.
type MetricsIsolator func(values url.Values, projectSlug string, defaultMatcher bool) error

// ProjectQuerier is the project-isolated read-only telemetry query port.
// Implementations must fail closed whenever project identity is missing or
// isolation cannot be applied.
type ProjectQuerier interface {
	Query(ctx context.Context, project meta.ProjectInfo, signal Signal, path string, params url.Values) ([]byte, error)
	QueryWithLimit(
		ctx context.Context,
		project meta.ProjectInfo,
		signal Signal,
		path string,
		params url.Values,
		maxBytes int64,
	) ([]byte, error)
	MetricsStringList(ctx context.Context, project meta.ProjectInfo, path string, params url.Values) ([]string, error)
}

// StorageQuerier implements ProjectQuerier directly against the platform's
// SignalBackend with fail-closed project isolation and bounded response reads.
type StorageQuerier struct {
	backend         storage.SignalBackend
	client          *http.Client
	isolateMetrics  MetricsIsolator
	defaultMaxBytes int64
	bodyDrainLimit  int64
}

// StorageQuerierConfig wires a StorageQuerier.
type StorageQuerierConfig struct {
	Backend         storage.SignalBackend
	Client          *http.Client
	IsolateMetrics  MetricsIsolator
	DefaultMaxBytes int64
	BodyDrainLimit  int64
}

// NewStorageQuerier builds a fail-closed ProjectQuerier backed by storage.
func NewStorageQuerier(cfg StorageQuerierConfig) *StorageQuerier {
	maxBytes := cfg.DefaultMaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	drainLimit := cfg.BodyDrainLimit
	if drainLimit <= 0 {
		drainLimit = DefaultBodyDrainLimit
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &StorageQuerier{
		backend:         cfg.Backend,
		client:          client,
		isolateMetrics:  cfg.IsolateMetrics,
		defaultMaxBytes: maxBytes,
		bodyDrainLimit:  drainLimit,
	}
}

func (q *StorageQuerier) Query(
	ctx context.Context,
	project meta.ProjectInfo,
	signal Signal,
	path string,
	params url.Values,
) ([]byte, error) {
	return q.QueryWithLimit(ctx, project, signal, path, params, q.defaultMaxBytes)
}

func (q *StorageQuerier) QueryWithLimit(
	ctx context.Context,
	project meta.ProjectInfo,
	signal Signal,
	path string,
	params url.Values,
	maxBytes int64,
) ([]byte, error) {
	if q == nil || q.backend == nil {
		return nil, errors.New("storage backend is unavailable")
	}
	if maxBytes <= 0 {
		maxBytes = q.defaultMaxBytes
	}
	cloned := CloneValues(params)
	var base string
	switch signal {
	case SignalMetrics:
		base = q.backend.MetricsQueryBase()
	case SignalLogs:
		base = q.backend.LogsQueryBase()
	case SignalTraces:
		base = q.backend.TracesQueryBase()
	default:
		return nil, errors.New("unsupported query signal")
	}
	if signal == SignalTraces {
		DisableTracesLatencyOffset(path, cloned)
	}
	if signal == SignalMetrics {
		if q.isolateMetrics == nil {
			return nil, errors.New("metrics isolator is unavailable")
		}
		defaultMatcher := path != "/api/v1/query" && path != "/api/v1/query_range"
		if err := q.isolateMetrics(cloned, project.Slug, defaultMatcher); err != nil {
			return nil, fmt.Errorf("invalid PromQL: %w", err)
		}
	}
	target := strings.TrimRight(base, "/") + path
	if encoded := cloned.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if signal == SignalLogs || signal == SignalTraces {
		if err := q.backend.ApplyTenantHeaders(req.Header, project.ID); err != nil {
			return nil, fmt.Errorf("invalid project ID for tenant isolation: %w", err)
		}
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, q.bodyDrainLimit))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w (%d MiB)", ErrResponseTooLarge, maxBytes>>20)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf(
			"query backend returned HTTP %d: %s",
			resp.StatusCode,
			ai.FirstRunes(strings.TrimSpace(string(body)), 500),
		)
	}
	return body, nil
}

func (q *StorageQuerier) MetricsStringList(
	ctx context.Context,
	project meta.ProjectInfo,
	path string,
	params url.Values,
) ([]string, error) {
	body, err := q.Query(ctx, project, SignalMetrics, path, params)
	if err != nil {
		return nil, err
	}
	var response struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Status != "success" {
		return nil, errors.New(response.Error)
	}
	return response.Data, nil
}

// DisableTracesLatencyOffset asks VictoriaTraces (v0.12+, single-node and
// vtselect) to skip its 30s -search.latencyOffset for LogsQL trace queries.
func DisableTracesLatencyOffset(path string, values url.Values) {
	if strings.HasPrefix(path, "/select/logsql/") {
		values.Set("disable_latency_offset", "true")
	}
}

// CloneValues makes an independent shallow copy of url.Values so callers' maps
// are never mutated when tenant filters are injected.
func CloneValues(source url.Values) url.Values {
	result := make(url.Values, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

// ValidateTraceID enforces the strict 1-32 hexadecimal character whitelist
// shared by traces proxy, Issue AI, and MCP.
func ValidateTraceID(traceID string) bool {
	if len(traceID) == 0 || len(traceID) > MaxTraceIDLength {
		return false
	}
	for i := 0; i < len(traceID); i++ {
		ch := traceID[i]
		isDigit := ch >= '0' && ch <= '9'
		isLowerHex := ch >= 'a' && ch <= 'f'
		isUpperHex := ch >= 'A' && ch <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}
	return true
}

// IssueEventsLogsQL constructs the deduplicated, newest-first LogsQL query for
// error events belonging to one issue fingerprint.
func IssueEventsLogsQL(fingerprint, eventID string, limit int) string {
	query := `_stream:{event_type="error"} AND fingerprint:=` + strconv.Quote(fingerprint)
	if eventID != "" {
		query += ` AND event_id:=` + strconv.Quote(eventID)
	}
	query += ` | uniq by (event_id, _time, raw_event, symbolicated_event) | sort by (_time) desc`
	if limit > 0 {
		query += ` | limit ` + strconv.Itoa(limit)
	}
	return query
}

// ReadVLogsLines streams newline-delimited VLogs records with an explicit
// per-line buffer ceiling (large symbolicated error events can exceed 64 KiB).
func ReadVLogsLines(r io.Reader, maxLineBytes int, handle func([]byte)) error {
	if maxLineBytes <= 0 {
		maxLineBytes = 4 << 20
	}
	scanner := bufio.NewScanner(r)
	initial := min(64<<10, maxLineBytes)
	scanner.Buffer(make([]byte, initial), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		handle(line)
	}
	return scanner.Err()
}
