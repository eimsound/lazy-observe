// Package mcp exposes lazy-observe's existing read-only HTTP query API as MCP
// tools. It intentionally knows only HTTP query endpoints and never imports a
// storage or ingestion package.
package mcp

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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	MaxLogRows         = 100
	MaxTraceSpans      = 200
	maxTraceSearch     = 100
	maxMetricSeries    = 500
	maxMetricSamples   = 5000
	maxResponseBytes   = 8 << 20
	defaultHTTPTimeout = 30 * time.Second
)

func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Timeout: defaultHTTPTimeout, Transport: transport}
}

// QueryClient is the project-isolated read-only platform query port consumed by
// MCP tool handlers. APIClient implements it over HTTP against the gateway's
// query API; tests or in-process embedders can supply alternative implementations
// without changing tool contracts.
type QueryClient interface {
	JSON(ctx context.Context, path string, query url.Values) (any, error)
	Metrics(ctx context.Context, path string, query url.Values) (map[string]any, error)
	Logs(ctx context.Context, query url.Values, requested int) (map[string]any, error)
}

type APIClient struct {
	BaseURL string
	// Token is a user API token (lou_…). Project API tokens are ingest-only and
	// are rejected by every query endpoint.
	Token string
	// Project selects the queried project slug via X-LO-Project. Empty uses the
	// platform default project.
	Project string
	HTTP    *http.Client
}

func NewAPIClient(baseURL, token string, client *http.Client) (*APIClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid platform URL")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("token is required")
	}
	if client == nil {
		client = defaultHTTPClient()
	}
	return &APIClient{BaseURL: baseURL, Token: token, HTTP: client}, nil
}

func ParseDSN(raw string) (*APIClient, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("DSN must be http(s)://host[:port]/p/<slug>@<token>")
	}
	const prefix = "/p/"
	if !strings.HasPrefix(u.EscapedPath(), prefix) {
		return nil, errors.New("DSN path must be /p/<slug>@<token>")
	}
	projectAndToken, err := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), prefix))
	if err != nil {
		return nil, errors.New("invalid DSN path escaping")
	}
	separator := strings.IndexByte(projectAndToken, '@')
	if separator <= 0 || separator == len(projectAndToken)-1 || strings.Contains(projectAndToken[:separator], "/") {
		return nil, errors.New("DSN path must be /p/<slug>@<token>")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment, u.User = "", "", "", "", nil
	api, err := NewAPIClient(strings.TrimRight(u.String(), "/"), projectAndToken[separator+1:], nil)
	if err != nil {
		return nil, err
	}
	api.Project = projectAndToken[:separator]
	return api, nil
}

func (c *APIClient) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	target := c.BaseURL + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if c.Project != "" {
		req.Header.Set("X-LO-Project", c.Project)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("platform query failed: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("read platform response: %w", readErr)
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("platform response exceeded 8 MiB safety limit")
	}
	if resp.StatusCode/100 != 2 {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error == "" {
			apiErr.Error = strings.TrimSpace(string(body))
		}
		if apiErr.Error == "" {
			apiErr.Error = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("platform returned HTTP %d: %s", resp.StatusCode, apiErr.Error)
	}
	return body, nil
}

// JSON performs an authenticated GET request and decodes the JSON body.
func (c *APIClient) JSON(ctx context.Context, path string, query url.Values) (any, error) {
	return c.json(ctx, path, query)
}

// Metrics performs a bounded Prometheus instant/range query against path.
func (c *APIClient) Metrics(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	return c.metrics(ctx, path, query)
}

// Logs performs a bounded LogsQL query with explicit truncation detection.
func (c *APIClient) Logs(ctx context.Context, query url.Values, requested int) (map[string]any, error) {
	return c.logs(ctx, query, requested)
}

func (c *APIClient) json(ctx context.Context, path string, query url.Values) (any, error) {
	body, err := c.get(ctx, path, query)
	if err != nil {
		return nil, err
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode platform response: %w", err)
	}
	return result, nil
}

func (c *APIClient) metrics(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	v, err := c.json(ctx, path, query)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("unexpected metrics response")
	}
	truncated := false
	if data, ok := root["data"].(map[string]any); ok {
		if rows, ok := data["result"].([]any); ok {
			if len(rows) > maxMetricSeries {
				rows, truncated = rows[:maxMetricSeries], true
			}
			samples := 0
			for i, raw := range rows {
				row, _ := raw.(map[string]any)
				values, _ := row["values"].([]any)
				if len(values) == 0 {
					continue
				}
				remaining := maxMetricSamples - samples
				if remaining <= 0 {
					rows = rows[:i]
					truncated = true
					break
				}
				if len(values) > remaining {
					row["values"] = values[:remaining]
					truncated = true
					samples = maxMetricSamples
				} else {
					samples += len(values)
				}
			}
			data["result"] = rows
		}
	}
	root["truncated"] = truncated
	root["limits"] = map[string]any{"series": maxMetricSeries, "samples": maxMetricSamples}
	return root, nil
}

func (c *APIClient) logs(ctx context.Context, query url.Values, requested int) (map[string]any, error) {
	limit := requested
	if limit <= 0 || limit > MaxLogRows {
		limit = MaxLogRows
	}
	// Ask for one extra row so truncation is explicit rather than guessed.
	query.Set("limit", strconv.Itoa(limit+1))
	body, err := c.get(ctx, "/api/v1/logs/query", query)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, limit+1)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("decode log row: %w", err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read log rows: %w", err)
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return map[string]any{"rows": rows, "count": len(rows), "truncated": truncated, "limit": limit}, nil
}

type traceSpan struct {
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Name         string `json:"name"`
	Service      string `json:"service,omitempty"`
	DurationUS   int64  `json:"duration_us"`
	Depth        int    `json:"depth"`
	Error        bool   `json:"error"`
	StartTimeUS  int64  `json:"start_time_us"`
}

func compactTrace(raw any) (map[string]any, error) {
	root, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("unexpected trace response")
	}
	data, _ := root["data"].([]any)
	if len(data) == 0 {
		return nil, errors.New("trace not found")
	}
	trace, _ := data[0].(map[string]any)
	rawSpans, _ := trace["spans"].([]any)
	processes, _ := trace["processes"].(map[string]any)
	parents := make(map[string]string, len(rawSpans))
	spans := make([]traceSpan, 0, min(len(rawSpans), MaxTraceSpans))
	for _, item := range rawSpans {
		s, _ := item.(map[string]any)
		id, _ := s["spanID"].(string)
		parent := ""
		if refs, ok := s["references"].([]any); ok {
			for _, rr := range refs {
				r, _ := rr.(map[string]any)
				if r["refType"] == "CHILD_OF" {
					parent, _ = r["spanID"].(string)
					break
				}
			}
		}
		parents[id] = parent
		if len(spans) >= MaxTraceSpans {
			continue
		}
		service := ""
		if pid, _ := s["processID"].(string); pid != "" {
			if p, _ := processes[pid].(map[string]any); p != nil {
				service, _ = p["serviceName"].(string)
			}
		}
		spans = append(
			spans,
			traceSpan{
				SpanID:       id,
				ParentSpanID: parent,
				Name:         stringValue(s["operationName"]),
				Service:      service,
				DurationUS:   int64(numberValue(s["duration"])),
				Error:        spanHasError(s),
				StartTimeUS:  int64(numberValue(s["startTime"])),
			},
		)
	}
	for i := range spans {
		seen := map[string]bool{spans[i].SpanID: true}
		for parent := spans[i].ParentSpanID; parent != "" && !seen[parent]; parent = parents[parent] {
			seen[parent] = true
			spans[i].Depth++
		}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartTimeUS < spans[j].StartTimeUS })
	return map[string]any{
		"trace_id":            trace["traceID"],
		"spans":               spans,
		"span_count":          len(spans),
		"original_span_count": len(rawSpans),
		"truncated":           len(rawSpans) > MaxTraceSpans,
		"limit":               MaxTraceSpans,
	}, nil
}

func spanHasError(span map[string]any) bool {
	tags, _ := span["tags"].([]any)
	for _, raw := range tags {
		tag, _ := raw.(map[string]any)
		key := stringValue(tag["key"])
		value := tag["value"]
		if key == "error" && (value == true || strings.EqualFold(stringValue(value), "true")) ||
			key == "otel.status_code" && strings.EqualFold(stringValue(value), "error") ||
			key == "http.status_code" && numberValue(value) >= 500 {
			return true
		}
	}
	return false
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func numberValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}
