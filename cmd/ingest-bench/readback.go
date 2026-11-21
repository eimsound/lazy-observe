package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReadbackTimeout  = 45 * time.Second
	readbackPollInterval    = 500 * time.Millisecond
	readbackMaxResponseSize = 8 << 20
	projectHeader           = "X-LO-Project"
)

type readbackResult struct {
	RunID            string   `json:"run_id"`
	Verified         bool     `json:"verified"`
	DurationSeconds  float64  `json:"duration_seconds"`
	MetricsVerified  bool     `json:"metrics_verified"`
	ObservedSeries   int64    `json:"observed_series"`
	LogsVerified     bool     `json:"logs_verified"`
	ObservedLogs     int64    `json:"observed_logs"`
	ObservedLogRows  int      `json:"observed_log_sample_rows"`
	TracesVerified   bool     `json:"traces_verified"`
	VerifiedTraceIDs []string `json:"verified_trace_ids,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type readbackConfig struct {
	baseURL         string
	readToken       string
	projectSlug     string
	runID           string
	expectedSeries  int64
	expectedMinLogs int64
	sampleTraceIDs  []string
	traceService    string
	windowStart     time.Time
	timeout         time.Duration
	pollInterval    time.Duration
}

func verifyReadback(ctx context.Context, client *http.Client, cfg readbackConfig) readbackResult {
	started := time.Now()
	res := readbackResult{RunID: cfg.runID}
	if cfg.runID == "" {
		res.Error = "readback run_id is empty"
		res.DurationSeconds = time.Since(started).Seconds()
		return res
	}
	if cfg.expectedSeries <= 0 || cfg.expectedMinLogs <= 0 || len(cfg.sampleTraceIDs) == 0 {
		res.Error = fmt.Sprintf(
			"insufficient ingested data for readback (expected_series=%d expected_logs=%d sample_traces=%d)",
			cfg.expectedSeries,
			cfg.expectedMinLogs,
			len(cfg.sampleTraceIDs),
		)
		res.DurationSeconds = time.Since(started).Seconds()
		return res
	}
	timeout := cfg.timeout
	if timeout <= 0 {
		timeout = defaultReadbackTimeout
	}
	pollInterval := cfg.pollInterval
	if pollInterval <= 0 {
		pollInterval = readbackPollInterval
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastMetricErr, lastLogErr, lastTraceErr error
	for {
		if !res.MetricsVerified {
			observed, err := checkMetricsReadback(
				pollCtx,
				client,
				cfg.baseURL,
				cfg.readToken,
				cfg.projectSlug,
				cfg.runID,
				cfg.expectedSeries,
			)
			res.ObservedSeries = observed
			if err == nil {
				res.MetricsVerified = true
				lastMetricErr = nil
			} else {
				lastMetricErr = err
			}
		}
		if !res.LogsVerified {
			observedTotal, sampleRows, err := checkLogsReadback(
				pollCtx,
				client,
				cfg.baseURL,
				cfg.readToken,
				cfg.projectSlug,
				cfg.runID,
				cfg.windowStart,
				cfg.expectedMinLogs,
			)
			res.ObservedLogs = observedTotal
			res.ObservedLogRows = sampleRows
			if err == nil {
				res.LogsVerified = true
				lastLogErr = nil
			} else {
				lastLogErr = err
			}
		}
		if !res.TracesVerified {
			verifiedIDs, err := checkTracesReadback(
				pollCtx,
				client,
				cfg.baseURL,
				cfg.readToken,
				cfg.projectSlug,
				cfg.runID,
				cfg.sampleTraceIDs,
				cfg.traceService,
				cfg.windowStart,
			)
			res.VerifiedTraceIDs = verifiedIDs
			if err == nil {
				res.TracesVerified = true
				lastTraceErr = nil
			} else {
				lastTraceErr = err
			}
		}
		if res.MetricsVerified && res.LogsVerified && res.TracesVerified {
			res.Verified = true
			res.DurationSeconds = time.Since(started).Seconds()
			return res
		}
		if pollCtx.Err() != nil {
			break
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}

	var errs []string
	if !res.MetricsVerified {
		errs = append(errs, fmt.Sprintf("metrics: %v", lastMetricErr))
	}
	if !res.LogsVerified {
		errs = append(errs, fmt.Sprintf("logs: %v", lastLogErr))
	}
	if !res.TracesVerified {
		errs = append(errs, fmt.Sprintf("traces: %v", lastTraceErr))
	}
	res.Error = strings.Join(errs, "; ")
	res.DurationSeconds = time.Since(started).Seconds()
	return res
}

func checkMetricsReadback(
	ctx context.Context,
	client *http.Client,
	baseURL, token, projectSlug, runID string,
	expectedMinSeries int64,
) (int64, error) {
	query := fmt.Sprintf(`count(%s{%s=%q})`, benchMetricName, benchRunIDAttribute, runID)
	target := baseURL + "/api/v1/query?" + url.Values{"query": {query}, "nocache": {"1"}}.Encode()
	status, body, err := doReadbackGET(ctx, client, target, token, projectSlug)
	if err != nil {
		return 0, err
	}
	if status/100 != 2 {
		return 0, fmt.Errorf("HTTP %d", status)
	}
	return parseMetricSeriesCount(body, expectedMinSeries)
}

func parseMetricSeriesCount(body []byte, expectedMinSeries int64) (int64, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode metrics query response: %w", err)
	}
	if resp.Status != "success" || resp.Data.ResultType != "vector" {
		return 0, fmt.Errorf("unexpected metrics query status=%q resultType=%q", resp.Status, resp.Data.ResultType)
	}
	if len(resp.Data.Result) == 0 {
		return 0, fmt.Errorf("no matching metric series found")
	}
	var maxVal int64
	for _, row := range resp.Data.Result {
		if len(row.Value) != 2 {
			continue
		}
		var text string
		if err := json.Unmarshal(row.Value[1], &text); err != nil {
			var num json.Number
			if err := json.Unmarshal(row.Value[1], &num); err != nil {
				continue
			}
			text = num.String()
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		if int64(f) > maxVal {
			maxVal = int64(f)
		}
	}
	if maxVal < expectedMinSeries {
		return maxVal, fmt.Errorf("observed %d metric series < expected minimum %d", maxVal, expectedMinSeries)
	}
	return maxVal, nil
}

func checkLogsReadback(
	ctx context.Context,
	client *http.Client,
	baseURL, token, projectSlug, runID string,
	windowStart time.Time,
	expectedMinLogs int64,
) (int64, int, error) {
	start := windowStart.Add(-2 * time.Minute).Unix()
	end := time.Now().Add(2 * time.Minute).Unix()
	logQuery := fmt.Sprintf(`%s:=%q`, benchRunIDAttribute, runID)

	hitsURL := baseURL + "/api/v1/logs/hits?" + url.Values{
		"query": {logQuery},
		"start": {strconv.FormatInt(start, 10)},
		"end":   {strconv.FormatInt(end, 10)},
		"step":  {"1h"},
	}.Encode()
	status, hitsBody, err := doReadbackGET(ctx, client, hitsURL, token, projectSlug)
	if err != nil {
		return 0, 0, fmt.Errorf("logs hits request: %w", err)
	}
	if status/100 != 2 {
		return 0, 0, fmt.Errorf("logs hits returned HTTP %d", status)
	}
	observedTotal, err := parseLogsHitsTotal(hitsBody)
	if err != nil {
		return 0, 0, err
	}
	if observedTotal < expectedMinLogs {
		return observedTotal, 0, fmt.Errorf("observed %d logs < expected minimum %d", observedTotal, expectedMinLogs)
	}

	queryURL := baseURL + "/api/v1/logs/query?" + url.Values{
		"query": {logQuery},
		"start": {strconv.FormatInt(start, 10)},
		"end":   {strconv.FormatInt(end, 10)},
		"limit": {"30"},
	}.Encode()
	status, queryBody, err := doReadbackGET(ctx, client, queryURL, token, projectSlug)
	if err != nil {
		return observedTotal, 0, fmt.Errorf("logs query request: %w", err)
	}
	if status/100 != 2 {
		return observedTotal, 0, fmt.Errorf("logs query returned HTTP %d", status)
	}
	sampleRows, err := validateLogsQuerySample(queryBody, runID, expectedMinLogs > 1)
	if err != nil {
		return observedTotal, sampleRows, err
	}
	return observedTotal, sampleRows, nil
}

func parseLogsHitsTotal(body []byte) (int64, error) {
	var resp struct {
		Hits []struct {
			Total json.Number `json:"total"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode logs hits response: %w", err)
	}
	var total int64
	for _, hit := range resp.Hits {
		v, err := hit.Total.Int64()
		if err != nil || v < 0 {
			return 0, fmt.Errorf("invalid logs hits total %q", hit.Total.String())
		}
		total += v
	}
	return total, nil
}

func validateLogsQuerySample(body []byte, runID string, requireVariety bool) (int, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), readbackMaxResponseSize)
	rows := 0
	uniqueMessages := make(map[string]struct{})
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(line, &row); err != nil || row == nil {
			return rows, fmt.Errorf("log sample line %d is not valid JSON", rows+1)
		}
		var msg, attrRunID string
		if err := json.Unmarshal(row["_msg"], &msg); err != nil || !strings.Contains(msg, runID) {
			return rows, fmt.Errorf("log sample line %d missing run_id in _msg", rows+1)
		}
		if err := json.Unmarshal(row[benchRunIDAttribute], &attrRunID); err != nil || attrRunID != runID {
			return rows, fmt.Errorf("log sample line %d missing matching %s attribute", rows+1, benchRunIDAttribute)
		}
		if len(row["_time"]) == 0 {
			return rows, fmt.Errorf("log sample line %d missing _time", rows+1)
		}
		uniqueMessages[msg] = struct{}{}
		rows++
	}
	if err := scanner.Err(); err != nil {
		return rows, fmt.Errorf("scan logs sample: %w", err)
	}
	if rows == 0 {
		return 0, fmt.Errorf("logs query returned 0 sample rows for run_id %s", runID)
	}
	if requireVariety && rows >= 2 && len(uniqueMessages) < 2 {
		return rows, fmt.Errorf(
			"logs sample lacks payload diversity (%d unique messages in %d rows)",
			len(uniqueMessages),
			rows,
		)
	}
	return rows, nil
}

func checkTracesReadback(
	ctx context.Context,
	client *http.Client,
	baseURL, token, projectSlug, runID string,
	sampleTraceIDs []string,
	traceService string,
	windowStart time.Time,
) ([]string, error) {
	verified := make([]string, 0, len(sampleTraceIDs))
	for _, traceID := range sampleTraceIDs {
		target := baseURL + "/api/traces/" + url.PathEscape(traceID)
		status, body, err := doReadbackGET(ctx, client, target, token, projectSlug)
		if err != nil {
			return verified, fmt.Errorf("trace %s request: %w", traceID, err)
		}
		if status/100 != 2 {
			return verified, fmt.Errorf("trace %s returned HTTP %d", traceID, status)
		}
		if err := validateTraceDetailBody(body, traceID, runID); err != nil {
			return verified, fmt.Errorf("trace %s: %w", traceID, err)
		}
		verified = append(verified, traceID)
	}
	if traceService != "" {
		startUS := windowStart.Add(-2 * time.Minute).UnixMicro()
		endUS := time.Now().Add(2 * time.Minute).UnixMicro()
		searchURL := baseURL + "/api/traces/search?" + url.Values{
			"service": {traceService},
			"start":   {strconv.FormatInt(startUS, 10)},
			"end":     {strconv.FormatInt(endUS, 10)},
			"limit":   {"20"},
		}.Encode()
		status, body, err := doReadbackGET(ctx, client, searchURL, token, projectSlug)
		if err != nil {
			return verified, fmt.Errorf("traces search request: %w", err)
		}
		if status/100 != 2 {
			return verified, fmt.Errorf("traces search returned HTTP %d", status)
		}
		if err := validateTraceSearchBody(body); err != nil {
			return verified, fmt.Errorf("traces search: %w", err)
		}
	}
	return verified, nil
}

func validateTraceDetailBody(body []byte, expectedTraceID, expectedRunID string) error {
	type jaegerTag struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	var resp struct {
		Data []struct {
			TraceID string `json:"traceID"`
			Spans   []struct {
				TraceID       string      `json:"traceID"`
				SpanID        string      `json:"spanID"`
				OperationName string      `json:"operationName"`
				ProcessID     string      `json:"processID"`
				Tags          []jaegerTag `json:"tags"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string      `json:"serviceName"`
				Tags        []jaegerTag `json:"tags"`
			} `json:"processes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode trace response: %w", err)
	}
	hasRunTag := func(tags []jaegerTag) bool {
		for _, tag := range tags {
			if tag.Key == benchRunIDAttribute && fmt.Sprint(tag.Value) == expectedRunID {
				return true
			}
		}
		return false
	}
	for _, trace := range resp.Data {
		if trace.TraceID != expectedTraceID || len(trace.Spans) == 0 {
			continue
		}
		validSpans := 0
		matchedRunID := false
		for _, proc := range trace.Processes {
			if hasRunTag(proc.Tags) {
				matchedRunID = true
			}
		}
		for _, span := range trace.Spans {
			if span.TraceID == expectedTraceID && strings.TrimSpace(span.SpanID) != "" &&
				strings.TrimSpace(span.OperationName) != "" {
				validSpans++
			}
			if hasRunTag(span.Tags) {
				matchedRunID = true
			}
		}
		if validSpans > 0 && (expectedRunID == "" || matchedRunID) {
			return nil
		}
	}
	return fmt.Errorf("trace %s with run_id %s not found in response", expectedTraceID, expectedRunID)
}

func validateTraceSearchBody(body []byte) error {
	var resp struct {
		Data []struct {
			TraceID string `json:"traceID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode trace search response: %w", err)
	}
	for _, trace := range resp.Data {
		if strings.TrimSpace(trace.TraceID) != "" {
			return nil
		}
	}
	return fmt.Errorf("trace search returned no traces")
}

func doReadbackGET(
	ctx context.Context,
	client *http.Client,
	target, token, projectSlug string,
) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(projectHeader, projectSlug)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	body, readErr := readBounded(resp.Body, readbackMaxResponseSize)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	if closeErr != nil {
		return resp.StatusCode, nil, closeErr
	}
	return resp.StatusCode, body, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", limit)
	}
	return data, nil
}
