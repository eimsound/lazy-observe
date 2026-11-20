package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
	"golang.org/x/sync/errgroup"
)

const (
	performanceDefaultWindow   = 24 * time.Hour
	performanceMinWindow       = time.Second
	performanceMaxWindow       = 7 * 24 * time.Hour
	performanceDefaultStep     = 5 * time.Minute
	performanceMaxBuckets      = 2_000
	performanceQueryTimeout    = 15 * time.Second
	performanceMaxResponseBody = 16 << 20
	performanceMaxEndpoints    = 200

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
	query     performanceMetricQuery
	vector    []storagepkg.MetricsVectorSample
	matrix    []storagepkg.MetricsMatrixSeries
	truncated bool
}

type performanceKey struct {
	service string
	method  string
	route   string
}

type rawPerformanceKey struct {
	service string
	method  string
	route   string
}

type rawMetricValues struct {
	throughput    float64
	hasThroughput bool
	errorRate     float64
	hasErrorRate  bool
	p50           float64
	p95           float64
	p99           float64
}

type rawSeriesAccumulator struct {
	normalized performanceKey
	summary    rawMetricValues
	points     map[int64]*rawMetricValues
}

type mergedMetricAccumulator struct {
	throughputSum        float64
	errorRateWeightedSum float64
	errorRateWeightSum   float64
	errorRateMax         float64
	p50Max               float64
	p95Max               float64
	p99Max               float64
}

func (m *mergedMetricAccumulator) add(v rawMetricValues) {
	if v.hasThroughput {
		m.throughputSum += v.throughput
	}
	if v.hasErrorRate {
		if v.hasThroughput && v.throughput > 0 {
			m.errorRateWeightedSum += v.errorRate * v.throughput
			m.errorRateWeightSum += v.throughput
		}
		if v.errorRate > m.errorRateMax {
			m.errorRateMax = v.errorRate
		}
	}
	if v.p50 > m.p50Max {
		m.p50Max = v.p50
	}
	if v.p95 > m.p95Max {
		m.p95Max = v.p95
	}
	if v.p99 > m.p99Max {
		m.p99Max = v.p99
	}
}

func (m *mergedMetricAccumulator) errorRate() float64 {
	if m.errorRateWeightSum > 0 {
		return m.errorRateWeightedSum / m.errorRateWeightSum
	}
	return m.errorRateMax
}

type performanceMetricAccumulator struct {
	endpoint performanceEndpoint
	summary  mergedMetricAccumulator
	points   map[int64]*mergedMetricAccumulator
}

func (g *Gateway) performanceEndpoints(c fiber.Ctx) error {
	start, end, step, ok := g.performanceRange(c)
	if !ok {
		return nil
	}
	response, err := g.queryPerformanceMetrics(c.Context(), currentProject(c), start, end, step)
	if err != nil {
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{
			"error":  "performance metrics query failed",
			"detail": err.Error(),
		})
	}
	return c.JSON(response)
}

func (g *Gateway) performanceRange(c fiber.Ctx) (time.Time, time.Time, time.Duration, bool) {
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
	window := end.Sub(start)
	if window < performanceMinWindow || window > performanceMaxWindow {
		_ = c.Status(http.StatusBadRequest).
			JSON(fiber.Map{"error": "time range must be between 1s and 7d"})
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
	if buckets := int(math.Ceil(float64(window) / float64(step))); buckets > performanceMaxBuckets {
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

	queryContext, cancel := context.WithTimeout(ctx, performanceQueryTimeout)
	defer cancel()

	results := make([]performanceMetricResult, len(queries))
	eg, groupCtx := errgroup.WithContext(queryContext)
	for i, q := range queries {
		idx, query := i, q
		eg.Go(func() error {
			res, err := g.executePerformanceQuery(groupCtx, project, query, start, end, step)
			if err != nil {
				return err
			}
			results[idx] = res
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return performanceResponse{}, err
	}

	rawAccumulators := make(map[rawPerformanceKey]*rawSeriesAccumulator)
	truncated := false
	for _, result := range results {
		if result.truncated {
			truncated = true
		}
		if result.query.rangeQuery {
			applyPerformanceMatrix(rawAccumulators, result.query.kind, result.matrix)
			continue
		}
		applyPerformanceVector(rawAccumulators, result.query.kind, result.vector)
	}
	accumulators := mergeRawPerformanceAccumulators(rawAccumulators)
	return buildPerformanceResponse(accumulators, start, end, step, truncated), nil
}

func (g *Gateway) executePerformanceQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	query performanceMetricQuery,
	start, end time.Time,
	step time.Duration,
) (performanceMetricResult, error) {
	mc := g.metricsQueryClient()
	run := func(expression string) (performanceMetricResult, error) {
		if query.rangeQuery {
			matrix, err := mc.QueryMatrix(ctx, project.Slug, expression, storagepkg.MetricsRangeQueryOptions{
				Start:    start,
				End:      end,
				Step:     step,
				Timeout:  performanceQueryTimeout,
				MaxBytes: performanceMaxResponseBody,
			})
			if err != nil {
				return performanceMetricResult{}, err
			}
			return performanceMetricResult{query: query, matrix: matrix}, nil
		}
		vector, err := mc.QueryVector(ctx, project.Slug, expression, storagepkg.MetricsQueryOptions{
			Time:     end,
			Timeout:  performanceQueryTimeout,
			MaxBytes: performanceMaxResponseBody,
		})
		if err != nil {
			return performanceMetricResult{}, err
		}
		return performanceMetricResult{query: query, vector: vector}, nil
	}

	res, err := run(query.expression)
	if errors.Is(err, storagepkg.ErrMetricsResponseTooLarge) {
		// Degrade gracefully on high-cardinality responses by bounding returned
		// series with topk and flagging Truncated instead of failing with 502.
		topkLimit := performanceMaxEndpoints
		if query.kind == performanceQuantiles {
			topkLimit = performanceMaxEndpoints * 3
		}
		boundedExpr := fmt.Sprintf("topk(%d, %s)", topkLimit, query.expression)
		res, err = run(boundedExpr)
		if err != nil {
			return performanceMetricResult{}, err
		}
		res.truncated = true
		return res, nil
	}
	return res, err
}

func performanceTrendRateWindow(window, step time.Duration) time.Duration {
	rateWindow := step
	if rateWindow < time.Minute {
		rateWindow = time.Minute
	}
	if rateWindow > window {
		rateWindow = window
	}
	if rateWindow < performanceMinWindow {
		return performanceMinWindow
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
	errorsExpr := fmt.Sprintf(
		`sum by (%s) (rate({__name__=%q,%s=~"5.."}[%s]))`,
		performanceLabels(metricsKind),
		performanceDurationCountMetric,
		performanceStatusCodeLabel(metricsKind),
		duration,
	)
	return fmt.Sprintf(`(%s / %s) or (%s * 0)`, errorsExpr, total, total)
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

func applyPerformanceVector(
	accumulators map[rawPerformanceKey]*rawSeriesAccumulator,
	kind performanceMetricKind,
	series []storagepkg.MetricsVectorSample,
) {
	for _, item := range series {
		rawKey, normKey, ok := performanceMetricKeys(item.Metric)
		if !ok {
			continue
		}
		acc := rawPerformanceAccumulator(accumulators, rawKey, normKey)
		assignRawMetric(&acc.summary, kind, item.Metric["quantile"], item.Point.Value)
	}
}

func applyPerformanceMatrix(
	accumulators map[rawPerformanceKey]*rawSeriesAccumulator,
	kind performanceMetricKind,
	series []storagepkg.MetricsMatrixSeries,
) {
	for _, item := range series {
		rawKey, normKey, ok := performanceMetricKeys(item.Metric)
		if !ok {
			continue
		}
		acc := rawPerformanceAccumulator(accumulators, rawKey, normKey)
		for _, sample := range item.Points {
			timestamp := sample.UnixMilli()
			point := acc.points[timestamp]
			if point == nil {
				point = &rawMetricValues{}
				acc.points[timestamp] = point
			}
			assignRawMetric(point, kind, item.Metric["quantile"], sample.Value)
		}
	}
}

func performanceMetricKeys(labels map[string]string) (rawPerformanceKey, performanceKey, bool) {
	rawKey := rawPerformanceKey{
		service: labels["service.name"],
		method:  labels["http.request.method"],
		route:   labels["http.route"],
	}
	key := performanceKey{
		service: strings.TrimSpace(rawKey.service),
		method:  strings.ToUpper(strings.TrimSpace(rawKey.method)),
		route:   strings.TrimSpace(rawKey.route),
	}
	if key.route == "" || key.route == "-" {
		return rawPerformanceKey{}, performanceKey{}, false
	}
	if key.service == "" || key.service == "-" {
		key.service = "unknown"
	}
	if key.method == "" || key.method == "-" {
		key.method = "UNKNOWN"
	}
	return rawKey, key, true
}

func rawPerformanceAccumulator(
	accumulators map[rawPerformanceKey]*rawSeriesAccumulator,
	rawKey rawPerformanceKey,
	normKey performanceKey,
) *rawSeriesAccumulator {
	acc := accumulators[rawKey]
	if acc == nil {
		acc = &rawSeriesAccumulator{
			normalized: normKey,
			points:     make(map[int64]*rawMetricValues),
		}
		accumulators[rawKey] = acc
	}
	return acc
}

func mergeRawPerformanceAccumulators(
	rawAccumulators map[rawPerformanceKey]*rawSeriesAccumulator,
) map[performanceKey]*performanceMetricAccumulator {
	merged := make(map[performanceKey]*performanceMetricAccumulator, len(rawAccumulators))
	for _, raw := range rawAccumulators {
		acc := merged[raw.normalized]
		if acc == nil {
			acc = &performanceMetricAccumulator{
				endpoint: performanceEndpoint{
					Service: raw.normalized.service,
					Method:  raw.normalized.method,
					Route:   raw.normalized.route,
					Series:  []performancePoint{},
				},
				points: make(map[int64]*mergedMetricAccumulator),
			}
			merged[raw.normalized] = acc
		}
		acc.summary.add(raw.summary)
		for ts, pt := range raw.points {
			mpt := acc.points[ts]
			if mpt == nil {
				mpt = &mergedMetricAccumulator{}
				acc.points[ts] = mpt
			}
			mpt.add(*pt)
		}
	}
	return merged
}

func assignRawMetric(
	dst *rawMetricValues,
	kind performanceMetricKind,
	quantile string,
	value float64,
) {
	if kind == performanceThroughput {
		dst.throughput += value
		dst.hasThroughput = true
		return
	}
	if kind == performanceErrorRate {
		dst.errorRate = value
		dst.hasErrorRate = true
		return
	}
	value *= float64(time.Second / time.Millisecond)
	switch quantile {
	case "0.5":
		if value > dst.p50 {
			dst.p50 = value
		}
	case "0.95":
		if value > dst.p95 {
			dst.p95 = value
		}
	case "0.99":
		if value > dst.p99 {
			dst.p99 = value
		}
	}
}

func buildPerformanceResponse(
	accumulators map[performanceKey]*performanceMetricAccumulator,
	start, end time.Time,
	step time.Duration,
	truncated bool,
) performanceResponse {
	endpoints := make([]performanceEndpoint, 0, len(accumulators))
	for _, accumulator := range accumulators {
		accumulator.endpoint.Throughput = accumulator.summary.throughputSum
		accumulator.endpoint.ErrorRate = accumulator.summary.errorRate()
		accumulator.endpoint.P50 = accumulator.summary.p50Max
		accumulator.endpoint.P95 = accumulator.summary.p95Max
		accumulator.endpoint.P99 = accumulator.summary.p99Max

		timestamps := make([]int64, 0, len(accumulator.points))
		for timestamp := range accumulator.points {
			timestamps = append(timestamps, timestamp)
		}
		sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
		for _, timestamp := range timestamps {
			pt := accumulator.points[timestamp]
			accumulator.endpoint.Series = append(accumulator.endpoint.Series, performancePoint{
				Time:       timestamp,
				Throughput: pt.throughputSum,
				P50:        pt.p50Max,
				P95:        pt.p95Max,
				P99:        pt.p99Max,
				ErrorRate:  pt.errorRate(),
			})
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
	if len(endpoints) > performanceMaxEndpoints {
		endpoints = endpoints[:performanceMaxEndpoints]
		truncated = true
	}
	return performanceResponse{
		Start:       start.UnixMilli(),
		End:         end.UnixMilli(),
		StepSeconds: int64(step / time.Second),
		Endpoints:   endpoints,
		Truncated:   truncated,
	}
}
