package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMetricsQueryTimeout bounds any internal metrics query when the
	// caller does not specify a per-query timeout.
	DefaultMetricsQueryTimeout = 10 * time.Second
	// DefaultMetricsQueryMaxBytes bounds the response payload read from the
	// metrics backend when the caller does not override MaxBytes.
	DefaultMetricsQueryMaxBytes int64 = 8 << 20
	// DefaultMetricsQueryBodyDrainSize caps how many unread bytes are drained
	// before closing a response body to preserve HTTP keep-alive reuse.
	DefaultMetricsQueryBodyDrainSize int64 = 64 << 10
)

var (
	// ErrMetricsResponseTooLarge reports that an upstream metrics response
	// exceeded the configured response byte budget.
	ErrMetricsResponseTooLarge = errors.New("metrics query response exceeded size limit")
	// ErrMetricsBackend reports a sanitized upstream metrics failure.
	ErrMetricsBackend = errors.New("metrics query failed")
	// ErrMetricsTimeout reports that an upstream metrics query timed out.
	ErrMetricsTimeout = errors.New("metrics query timed out")
)

type metricsTimeoutError struct{}

func (metricsTimeoutError) Error() string { return ErrMetricsTimeout.Error() }

func (metricsTimeoutError) Unwrap() []error {
	return []error{ErrMetricsTimeout, context.DeadlineExceeded}
}

// MetricsPoint is a decoded, finite timestamp/value pair returned by a
// Prometheus-compatible metrics query.
type MetricsPoint struct {
	Timestamp float64
	Value     float64
}

// Time converts Timestamp (Unix seconds) into a UTC *time.Time, returning nil
// when Timestamp is non-positive or non-finite.
func (p MetricsPoint) Time() *time.Time {
	if math.IsNaN(p.Timestamp) || math.IsInf(p.Timestamp, 0) || p.Timestamp <= 0 {
		return nil
	}
	sec, frac := math.Modf(p.Timestamp)
	t := time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC()
	return &t
}

// UnixMilli returns Timestamp rounded to milliseconds since the Unix epoch.
func (p MetricsPoint) UnixMilli() int64 {
	return int64(math.Round(p.Timestamp * 1_000))
}

// MetricsVectorSample is one instant vector sample with a finite value.
type MetricsVectorSample struct {
	Metric map[string]string
	Point  MetricsPoint
}

// MetricsMatrixSeries is one range query series containing only finite points.
type MetricsMatrixSeries struct {
	Metric map[string]string
	Points []MetricsPoint
}

// MetricsQueryOptions overrides defaults for a QueryVector call.
type MetricsQueryOptions struct {
	Timeout  time.Duration
	MaxBytes int64
	Time     time.Time
}

// MetricsRangeQueryOptions configures a QueryMatrix call.
type MetricsRangeQueryOptions struct {
	Start    time.Time
	End      time.Time
	Step     time.Duration
	Timeout  time.Duration
	MaxBytes int64
}

// MetricsClientConfig configures a MetricsClient.
type MetricsClientConfig struct {
	Backend        SignalBackend
	HTTPClient     *http.Client
	DefaultTimeout time.Duration
	MaxBytes       int64
	BodyDrainLimit int64
}

// MetricsClient executes project-isolated PromQL/MetricsQL queries against a
// SignalBackend with mandatory timeouts, configurable response body limits,
// connection body draining, non-finite float filtering, and sanitized errors.
type MetricsClient struct {
	backend        SignalBackend
	httpClient     *http.Client
	defaultTimeout time.Duration
	maxBytes       int64
	bodyDrainLimit int64
}

// NewMetricsClient creates a MetricsClient with safe defaults for timeouts,
// response body caps, and body draining.
func NewMetricsClient(cfg MetricsClientConfig) *MetricsClient {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	timeout := cfg.DefaultTimeout
	if timeout <= 0 {
		timeout = DefaultMetricsQueryTimeout
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMetricsQueryMaxBytes
	}
	drainLimit := cfg.BodyDrainLimit
	if drainLimit <= 0 {
		drainLimit = DefaultMetricsQueryBodyDrainSize
	}
	return &MetricsClient{
		backend:        cfg.Backend,
		httpClient:     client,
		defaultTimeout: timeout,
		maxBytes:       maxBytes,
		bodyDrainLimit: drainLimit,
	}
}

// Backend returns the underlying SignalBackend.
func (c *MetricsClient) Backend() SignalBackend {
	if c == nil {
		return nil
	}
	return c.backend
}

type metricsWireSeries struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value"`
	Values [][]json.RawMessage `json:"values"`
}

type metricsWireResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string              `json:"resultType"`
		Result     []metricsWireSeries `json:"result"`
	} `json:"data"`
}

// QueryVector runs an instant PromQL/MetricsQL query scoped to projectSlug and
// returns vector samples with non-finite (NaN/+Inf/-Inf) entries filtered out.
func (c *MetricsClient) QueryVector(
	ctx context.Context,
	projectSlug, expr string,
	opts MetricsQueryOptions,
) ([]MetricsVectorSample, error) {
	values := url.Values{"query": {expr}}
	if !opts.Time.IsZero() {
		values.Set("time", opts.Time.UTC().Format(time.RFC3339Nano))
	}
	series, err := c.doQuery(ctx, projectSlug, "/api/v1/query", "vector", values, opts.Timeout, opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	samples := make([]MetricsVectorSample, 0, len(series))
	for _, item := range series {
		point, finite, decodeErr := decodeMetricsPoint(item.Value)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if !finite {
			continue
		}
		samples = append(samples, MetricsVectorSample{
			Metric: item.Metric,
			Point:  point,
		})
	}
	return samples, nil
}

// QueryMatrix runs a range PromQL/MetricsQL query scoped to projectSlug and
// returns matrix series with non-finite (NaN/+Inf/-Inf) points filtered out.
func (c *MetricsClient) QueryMatrix(
	ctx context.Context,
	projectSlug, expr string,
	opts MetricsRangeQueryOptions,
) ([]MetricsMatrixSeries, error) {
	if opts.Start.IsZero() || opts.End.IsZero() || opts.Step <= 0 {
		return nil, fmt.Errorf("%w: start, end, and positive step are required", ErrMetricsBackend)
	}
	stepSeconds := max(int64(opts.Step/time.Second), 1)
	values := url.Values{
		"query": {expr},
		"start": {opts.Start.UTC().Format(time.RFC3339Nano)},
		"end":   {opts.End.UTC().Format(time.RFC3339Nano)},
		"step":  {strconv.FormatInt(stepSeconds, 10)},
	}
	series, err := c.doQuery(ctx, projectSlug, "/api/v1/query_range", "matrix", values, opts.Timeout, opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	result := make([]MetricsMatrixSeries, 0, len(series))
	for _, item := range series {
		points := make([]MetricsPoint, 0, len(item.Values))
		for _, rawPoint := range item.Values {
			point, finite, decodeErr := decodeMetricsPoint(rawPoint)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if !finite {
				continue
			}
			points = append(points, point)
		}
		result = append(result, MetricsMatrixSeries{
			Metric: item.Metric,
			Points: points,
		})
	}
	return result, nil
}

func (c *MetricsClient) doQuery(
	ctx context.Context,
	projectSlug, path, wantResultType string,
	values url.Values,
	timeoutOverride time.Duration,
	maxBytesOverride int64,
) ([]metricsWireSeries, error) {
	if c == nil || c.backend == nil {
		return nil, fmt.Errorf("%w: metrics backend is not configured", ErrMetricsBackend)
	}
	if err := c.backend.ApplyMetricsQueryIsolation(values, projectSlug, false); err != nil {
		if errors.Is(err, ErrInvalidTenant) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: invalid metrics query", ErrMetricsBackend)
	}

	timeout := timeoutOverride
	if timeout <= 0 {
		timeout = c.defaultTimeout
	}
	if timeout <= 0 {
		timeout = DefaultMetricsQueryTimeout
	}
	maxBytes := maxBytesOverride
	if maxBytes <= 0 {
		maxBytes = c.maxBytes
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMetricsQueryMaxBytes
	}
	drainLimit := c.bodyDrainLimit
	if drainLimit <= 0 {
		drainLimit = DefaultMetricsQueryBodyDrainSize
	}

	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := strings.TrimRight(c.backend.MetricsQueryBase(), "/") + path + "?" + values.Encode()
	req, err := http.NewRequestWithContext(queryCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to construct query request", ErrMetricsBackend)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, sanitizeMetricsTransportError(ctx, queryCtx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, sanitizeMetricsTransportError(ctx, queryCtx, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrMetricsResponseTooLarge, maxBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, sanitizeMetricsHTTPStatusError(resp.StatusCode, body)
	}

	var decoded metricsWireResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("%w: invalid response payload", ErrMetricsBackend)
	}
	if decoded.Status != "success" {
		if errorType := sanitizeErrorType(decoded.ErrorType); errorType != "" {
			return nil, fmt.Errorf("%w: backend error (%s)", ErrMetricsBackend, errorType)
		}
		return nil, fmt.Errorf("%w: backend returned unsuccessful status", ErrMetricsBackend)
	}
	if decoded.Data.ResultType != wantResultType {
		return nil, fmt.Errorf(
			"%w: unexpected result type %q, want %q",
			ErrMetricsBackend,
			decoded.Data.ResultType,
			wantResultType,
		)
	}
	return decoded.Data.Result, nil
}

func decodeMetricsPoint(sample []json.RawMessage) (MetricsPoint, bool, error) {
	if len(sample) != 2 {
		return MetricsPoint{}, false, fmt.Errorf("%w: malformed sample", ErrMetricsBackend)
	}
	ts, err := parseMetricsFloat(sample[0])
	if err != nil {
		return MetricsPoint{}, false, fmt.Errorf("%w: invalid sample timestamp", ErrMetricsBackend)
	}
	val, err := parseMetricsFloat(sample[1])
	if err != nil {
		return MetricsPoint{}, false, fmt.Errorf("%w: invalid sample value", ErrMetricsBackend)
	}
	if math.IsNaN(ts) || math.IsInf(ts, 0) || math.IsNaN(val) || math.IsInf(val, 0) {
		return MetricsPoint{}, false, nil
	}
	return MetricsPoint{Timestamp: ts, Value: val}, true, nil
}

func parseMetricsFloat(raw json.RawMessage) (float64, error) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var unquoted string
		if err := json.Unmarshal(raw, &unquoted); err != nil {
			return 0, err
		}
		trimmed = strings.TrimSpace(unquoted)
	}
	return strconv.ParseFloat(trimmed, 64)
}

func sanitizeMetricsTransportError(parentCtx, queryCtx context.Context, err error) error {
	if parentCtx != nil && errors.Is(parentCtx.Err(), context.Canceled) {
		return context.Canceled
	}
	if (queryCtx != nil && errors.Is(queryCtx.Err(), context.DeadlineExceeded)) ||
		errors.Is(err, context.DeadlineExceeded) {
		return metricsTimeoutError{}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return metricsTimeoutError{}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return fmt.Errorf("%w: upstream unavailable", ErrMetricsBackend)
}

func sanitizeMetricsHTTPStatusError(statusCode int, body []byte) error {
	var wire metricsWireResponse
	if json.Unmarshal(body, &wire) == nil {
		if errorType := sanitizeErrorType(wire.ErrorType); errorType != "" {
			return fmt.Errorf("%w: upstream HTTP %d (%s)", ErrMetricsBackend, statusCode, errorType)
		}
	}
	return fmt.Errorf("%w: upstream HTTP %d", ErrMetricsBackend, statusCode)
}

func sanitizeErrorType(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 64 {
		return ""
	}
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '-' || ch == ' ' {
			continue
		}
		return ""
	}
	return trimmed
}
