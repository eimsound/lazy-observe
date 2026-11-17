package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v2"
)

const (
	performanceDefaultWindow = 24 * time.Hour
	performanceMaxWindow     = 7 * 24 * time.Hour
	performanceDefaultStep   = 5 * time.Minute
	performanceMaxBuckets    = 2_000

	performanceDurationBucketMetric = "http.server.request.duration_bucket"
	performanceDurationCountMetric  = "http.server.request.duration_count"
	performanceEndpointLabels       = `service\.name, http\.request\.method, http\.route`
	performancePrometheusLabels     = `"service.name", "http.request.method", "http.route"`
)

type performancePoint struct {
	Time       int64   `json:"time"`
	Throughput float64 `json:"throughput"`
	P50        float64 `json:"p50"`
	P95        float64 `json:"p95"`
	P99        float64 `json:"p99"`
	ErrorRate  float64 `json:"error_rate"`
}

type performanceEndpoint struct {
	Service    string             `json:"service"`
	Method     string             `json:"method"`
	Route      string             `json:"route"`
	Throughput float64            `json:"throughput"`
	P50        float64            `json:"p50"`
	P95        float64            `json:"p95"`
	P99        float64            `json:"p99"`
	ErrorRate  float64            `json:"error_rate"`
	Series     []performancePoint `json:"series"`
}

type performanceResponse struct {
	Start       int64                 `json:"start"`
	End         int64                 `json:"end"`
	StepSeconds int64                 `json:"step_seconds"`
	Endpoints   []performanceEndpoint `json:"endpoints"`
	Truncated   bool                  `json:"truncated"`
}

type performanceMetricSeries struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value"`
	Values [][]json.RawMessage `json:"values"`
}

type performanceMetricResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string                    `json:"resultType"`
		Result     []performanceMetricSeries `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

type performanceMetricKind int

const (
	performanceQuantiles performanceMetricKind = iota
	performanceThroughput
	performanceErrorRate
)

type performanceMetricQuery struct {
	kind       performanceMetricKind
	rangeQuery bool
	expression string
}

type performanceMetricResult struct {
	query  performanceMetricQuery
	series []performanceMetricSeries
	err    error
}

type performanceKey struct {
	service string
	method  string
	route   string
}

type performanceMetricAccumulator struct {
	endpoint performanceEndpoint
	points   map[int64]*performancePoint
}

func (g *Gateway) performanceEndpoints(c *fiber.Ctx) error {
	start, end, step, ok := g.performanceRange(c)
	if !ok {
		return nil
	}
	response, err := g.queryPerformanceMetrics(c.UserContext(), currentProject(c), start, end, step)
	if err != nil {
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{
			"error":  "performance metrics query failed",
			"detail": err.Error(),
		})
	}
	return c.JSON(response)
}

func (g *Gateway) performanceRange(c *fiber.Ctx) (time.Time, time.Time, time.Duration, bool) {
	end := g.now().UTC()
	start := end.Add(-performanceDefaultWindow)
	var err error
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		end, err = parseLLMTime(raw)
		if err != nil {
			_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid end: use RFC3339 or a Unix timestamp"})
			return time.Time{}, time.Time{}, 0, false
		}
	}
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		start, err = parseLLMTime(raw)
		if err != nil {
			_ = c.Status(http.StatusBadRequest).
				JSON(fiber.Map{"error": "invalid start: use RFC3339 or a Unix timestamp"})
			return time.Time{}, time.Time{}, 0, false
		}
	} else if strings.TrimSpace(c.Query("end")) != "" {
		start = end.Add(-performanceDefaultWindow)
	}
	if !end.After(start) || end.Sub(start) > performanceMaxWindow {
		_ = c.Status(http.StatusBadRequest).
			JSON(fiber.Map{"error": "time range must be positive and no longer than 7d"})
		return time.Time{}, time.Time{}, 0, false
	}
	step := performanceDefaultStep
	if raw := strings.TrimSpace(c.Query("step")); raw != "" {
		seconds, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || seconds < 1 || seconds > int64((24*time.Hour)/time.Second) {
			_ = c.Status(http.StatusBadRequest).
				JSON(fiber.Map{"error": "step must be an integer between 1 and 86400 seconds"})
			return time.Time{}, time.Time{}, 0, false
		}
		step = time.Duration(seconds) * time.Second
	}
	if buckets := int(math.Ceil(float64(end.Sub(start)) / float64(step))); buckets > performanceMaxBuckets {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "time range produces too many buckets"})
		return time.Time{}, time.Time{}, 0, false
	}
	return start.UTC(), end.UTC(), step, true
}

func (g *Gateway) queryPerformanceMetrics(
	ctx context.Context,
	project meta.ProjectInfo,
	start, end time.Time,
	step time.Duration,
) (performanceResponse, error) {
	window := end.Sub(start)
	trendWindow := performanceTrendRateWindow(window, step)
	metricsKind := g.storage.MetricsMode()
	queries := []performanceMetricQuery{
		{kind: performanceQuantiles, expression: performanceQuantilesQuery(window, metricsKind)},
		{kind: performanceThroughput, expression: performanceThroughputQuery(window, metricsKind)},
		{kind: performanceErrorRate, expression: performanceErrorRateQuery(window, metricsKind)},
		{
			kind:       performanceQuantiles,
			rangeQuery: true,
			expression: performanceQuantilesQuery(trendWindow, metricsKind),
		},
		{
			kind:       performanceThroughput,
			rangeQuery: true,
			expression: performanceThroughputQuery(trendWindow, metricsKind),
		},
		{
			kind:       performanceErrorRate,
			rangeQuery: true,
			expression: performanceErrorRateQuery(trendWindow, metricsKind),
		},
	}

	queryContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan performanceMetricResult, len(queries))
	for _, query := range queries {
		go func(query performanceMetricQuery) {
			values := url.Values{"query": {query.expression}}
			path := "/api/v1/query"
			resultType := "vector"
			if query.rangeQuery {
				path = "/api/v1/query_range"
				resultType = "matrix"
				values.Set("start", start.Format(time.RFC3339Nano))
				values.Set("end", end.Format(time.RFC3339Nano))
				values.Set("step", strconv.FormatInt(int64(step/time.Second), 10))
			} else {
				values.Set("time", end.Format(time.RFC3339Nano))
			}
			body, err := g.isolatedQuery(queryContext, project, "metrics", path, values)
			if err != nil {
				results <- performanceMetricResult{query: query, err: err}
				return
			}
			series, err := decodePerformanceMetricResponse(body, resultType)
			results <- performanceMetricResult{query: query, series: series, err: err}
		}(query)
	}

	accumulators := make(map[performanceKey]*performanceMetricAccumulator)
	var firstErr error
	for range queries {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		if result.query.rangeQuery {
			if err := applyPerformanceMatrix(
				accumulators,
				result.query.kind,
				result.series,
			); err != nil &&
				firstErr == nil {
				firstErr = err
				cancel()
			}
			continue
		}
		if err := applyPerformanceVector(
			accumulators,
			result.query.kind,
			result.series,
		); err != nil &&
			firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	if firstErr != nil {
		return performanceResponse{}, firstErr
	}
	return buildPerformanceResponse(accumulators, start, end, step), nil
}

func performanceTrendRateWindow(window, step time.Duration) time.Duration {
	rateWindow := 4 * step
	if rateWindow < time.Minute {
		rateWindow = time.Minute
	}
	if rateWindow > window {
		return window
	}
	return rateWindow
}

func performanceQuantilesQuery(window time.Duration, metricsKind storagepkg.MetricsKind) string {
	histogram := fmt.Sprintf(
		`sum by (le, %s) (rate({__name__=%q}[%s]))`,
		performanceLabels(metricsKind),
		performanceDurationBucketMetric,
		performanceDurationLiteral(window),
	)
	if metricsKind == storagepkg.MetricsVM {
		return fmt.Sprintf(
			`histogram_quantiles("quantile", 0.5, 0.95, 0.99, `+
				`%s)`,
			histogram,
		)
	}
	queries := make([]string, 0, 3)
	for _, quantile := range []string{"0.5", "0.95", "0.99"} {
		queries = append(
			queries,
			fmt.Sprintf(
				`label_replace(histogram_quantile(%s, %s), "quantile", %q, "", "")`,
				quantile,
				histogram,
				quantile,
			),
		)
	}
	return strings.Join(queries, " or ")
}

func performanceThroughputQuery(window time.Duration, metricsKind storagepkg.MetricsKind) string {
	return fmt.Sprintf(
		`sum by (%s) (rate({__name__=%q}[%s]))`,
		performanceLabels(metricsKind),
		performanceDurationCountMetric,
		performanceDurationLiteral(window),
	)
}

func performanceErrorRateQuery(window time.Duration, metricsKind storagepkg.MetricsKind) string {
	duration := performanceDurationLiteral(window)
	total := fmt.Sprintf(
		`sum by (%s) (rate({__name__=%q}[%s]))`,
		performanceLabels(metricsKind),
		performanceDurationCountMetric,
		duration,
	)
	errors := fmt.Sprintf(
		`sum by (%s) (rate({__name__=%q,%s=~"5.."}[%s]))`,
		performanceLabels(metricsKind),
		performanceDurationCountMetric,
		performanceStatusCodeLabel(metricsKind),
		duration,
	)
	return fmt.Sprintf(`(%s / %s) or (%s * 0)`, errors, total, total)
}

func performanceLabels(metricsKind storagepkg.MetricsKind) string {
	if metricsKind == storagepkg.MetricsPrometheus {
		return performancePrometheusLabels
	}
	return performanceEndpointLabels
}

func performanceStatusCodeLabel(metricsKind storagepkg.MetricsKind) string {
	if metricsKind == storagepkg.MetricsPrometheus {
		return `"http.response.status_code"`
	}
	return `http\.response\.status_code`
}

func performanceDurationLiteral(duration time.Duration) string {
	seconds := max(int64(duration/time.Second), 1)
	return strconv.FormatInt(seconds, 10) + "s"
}

func decodePerformanceMetricResponse(body []byte, resultType string) ([]performanceMetricSeries, error) {
	var response performanceMetricResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode metrics response: %w", err)
	}
	if response.Status != "success" {
		if response.Error == "" {
			response.Error = "unknown metrics backend error"
		}
		return nil, fmt.Errorf("metrics backend: %s", response.Error)
	}
	if response.Data.ResultType != resultType {
		return nil, fmt.Errorf("metrics backend returned result type %q, want %q", response.Data.ResultType, resultType)
	}
	return response.Data.Result, nil
}

func applyPerformanceVector(
	accumulators map[performanceKey]*performanceMetricAccumulator,
	kind performanceMetricKind,
	series []performanceMetricSeries,
) error {
	for _, item := range series {
		key, ok := performanceMetricKey(item.Metric)
		if !ok {
			continue
		}
		_, value, valid, err := decodePerformanceSample(item.Value)
		if err != nil {
			return err
		}
		if !valid {
			continue
		}
		accumulator := performanceAccumulator(accumulators, key)
		assignPerformanceMetric(&accumulator.endpoint, nil, kind, item.Metric["quantile"], value)
	}
	return nil
}

func applyPerformanceMatrix(
	accumulators map[performanceKey]*performanceMetricAccumulator,
	kind performanceMetricKind,
	series []performanceMetricSeries,
) error {
	for _, item := range series {
		key, ok := performanceMetricKey(item.Metric)
		if !ok {
			continue
		}
		accumulator := performanceAccumulator(accumulators, key)
		for _, sample := range item.Values {
			timestamp, value, valid, err := decodePerformanceSample(sample)
			if err != nil {
				return err
			}
			if !valid {
				continue
			}
			point := accumulator.points[timestamp]
			if point == nil {
				point = &performancePoint{Time: timestamp}
				accumulator.points[timestamp] = point
			}
			assignPerformanceMetric(nil, point, kind, item.Metric["quantile"], value)
		}
	}
	return nil
}

func performanceMetricKey(labels map[string]string) (performanceKey, bool) {
	key := performanceKey{
		service: strings.TrimSpace(labels["service.name"]),
		method:  strings.ToUpper(strings.TrimSpace(labels["http.request.method"])),
		route:   strings.TrimSpace(labels["http.route"]),
	}
	if key.route == "" || key.route == "-" {
		return performanceKey{}, false
	}
	if key.service == "" || key.service == "-" {
		key.service = "unknown"
	}
	if key.method == "" || key.method == "-" {
		key.method = "UNKNOWN"
	}
	return key, true
}

func performanceAccumulator(
	accumulators map[performanceKey]*performanceMetricAccumulator,
	key performanceKey,
) *performanceMetricAccumulator {
	accumulator := accumulators[key]
	if accumulator == nil {
		accumulator = &performanceMetricAccumulator{
			endpoint: performanceEndpoint{
				Service: key.service,
				Method:  key.method,
				Route:   key.route,
				Series:  []performancePoint{},
			},
			points: make(map[int64]*performancePoint),
		}
		accumulators[key] = accumulator
	}
	return accumulator
}

func decodePerformanceSample(sample []json.RawMessage) (int64, float64, bool, error) {
	if len(sample) != 2 {
		return 0, 0, false, fmt.Errorf("metrics backend returned malformed sample")
	}
	timestamp, err := parsePerformanceFloat(sample[0])
	if err != nil {
		return 0, 0, false, fmt.Errorf("decode metrics timestamp: %w", err)
	}
	value, err := parsePerformanceFloat(sample[1])
	if err != nil {
		return 0, 0, false, fmt.Errorf("decode metrics value: %w", err)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, 0, false, nil
	}
	return int64(math.Round(timestamp * 1_000)), value, true, nil
}

func parsePerformanceFloat(raw json.RawMessage) (float64, error) {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strconv.ParseFloat(value, 64)
}

func assignPerformanceMetric(
	endpoint *performanceEndpoint,
	point *performancePoint,
	kind performanceMetricKind,
	quantile string,
	value float64,
) {
	if kind == performanceThroughput {
		if endpoint != nil {
			endpoint.Throughput = value
		} else {
			point.Throughput = value
		}
		return
	}
	if kind == performanceErrorRate {
		if endpoint != nil {
			endpoint.ErrorRate = value
		} else {
			point.ErrorRate = value
		}
		return
	}
	value *= float64(time.Second / time.Millisecond)
	switch quantile {
	case "0.5":
		if endpoint != nil {
			endpoint.P50 = value
		} else {
			point.P50 = value
		}
	case "0.95":
		if endpoint != nil {
			endpoint.P95 = value
		} else {
			point.P95 = value
		}
	case "0.99":
		if endpoint != nil {
			endpoint.P99 = value
		} else {
			point.P99 = value
		}
	}
}

func buildPerformanceResponse(
	accumulators map[performanceKey]*performanceMetricAccumulator,
	start, end time.Time,
	step time.Duration,
) performanceResponse {
	endpoints := make([]performanceEndpoint, 0, len(accumulators))
	for _, accumulator := range accumulators {
		timestamps := make([]int64, 0, len(accumulator.points))
		for timestamp := range accumulator.points {
			timestamps = append(timestamps, timestamp)
		}
		sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
		for _, timestamp := range timestamps {
			accumulator.endpoint.Series = append(accumulator.endpoint.Series, *accumulator.points[timestamp])
		}
		endpoints = append(endpoints, accumulator.endpoint)
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].P95 != endpoints[j].P95 {
			return endpoints[i].P95 > endpoints[j].P95
		}
		if endpoints[i].Service != endpoints[j].Service {
			return endpoints[i].Service < endpoints[j].Service
		}
		if endpoints[i].Route != endpoints[j].Route {
			return endpoints[i].Route < endpoints[j].Route
		}
		return endpoints[i].Method < endpoints[j].Method
	})
	return performanceResponse{
		Start:       start.UnixMilli(),
		End:         end.UnixMilli(),
		StepSeconds: int64(step / time.Second),
		Endpoints:   endpoints,
		Truncated:   false,
	}
}
