package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/llmcost"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	llmDefaultWindow   = 24 * time.Hour
	llmMaxWindow       = 7 * 24 * time.Hour
	llmDefaultLimit    = 100
	llmMaxCallLimit    = 1000
	llmMaxBuckets      = 2000
	llmMaxStatsGroups  = 10000
	llmMaxResponseBody = 32 << 20
	// Match JavaScript Number.MAX_SAFE_INTEGER so frontend contract validators
	// never reject saturated aggregates.
	llmMaxSafeInt = (int64(1) << 53) - 1
)

const (
	llmBaseFilter = `"span_attr:gen_ai.operation.name":*`

	llmProjectedFields = `_time, start_time_unix_nano, duration, trace_id, span_id, status_code, ` +
		`"resource_attr:service.name", "service.name", ` +
		`"span_attr:gen_ai.operation.name", "span_attr:gen_ai.provider.name", "span_attr:gen_ai.system", ` +
		`"span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model", ` +
		`"span_attr:gen_ai.usage.input_tokens", "span_attr:gen_ai.usage.output_tokens", ` +
		`"span_attr:gen_ai.usage.cache_read.input_tokens", "span_attr:gen_ai.usage.cache_creation.input_tokens", ` +
		`"span_attr:gen_ai.usage.reasoning_tokens", "span_attr:gen_ai.usage.missing", ` +
		`"span_attr:error.type", "error.type", "span_attr:error", error`

	llmStatsGroupByFields = `"resource_attr:service.name", "service.name", ` +
		`"span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model", ` +
		`"span_attr:gen_ai.provider.name", "span_attr:gen_ai.system", ` +
		`"span_attr:gen_ai.usage.missing"`

	llmCostGroupByFields = `"span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model", ` +
		`"span_attr:gen_ai.provider.name", "span_attr:gen_ai.system", ` +
		`"span_attr:gen_ai.usage.missing"`

	llmTokenSumStats = `sum("span_attr:gen_ai.usage.input_tokens") if ("span_attr:gen_ai.usage.input_tokens":>0) as input_tokens, ` +
		`sum("span_attr:gen_ai.usage.output_tokens") if ("span_attr:gen_ai.usage.output_tokens":>0) as output_tokens, ` +
		`sum("span_attr:gen_ai.usage.cache_read.input_tokens") if ("span_attr:gen_ai.usage.cache_read.input_tokens":>0) as cache_read_tokens, ` +
		`sum("span_attr:gen_ai.usage.cache_creation.input_tokens") if ("span_attr:gen_ai.usage.cache_creation.input_tokens":>0) as cache_creation_tokens, ` +
		`sum("span_attr:gen_ai.usage.reasoning_tokens") if ("span_attr:gen_ai.usage.reasoning_tokens":>0) as reasoning_tokens`
)

type llmPricingLookup interface {
	CostWithUsage(model string, usage llmcost.Usage) (float64, bool)
}

type llmCall struct {
	Time                time.Time `json:"time"`
	Service             string    `json:"service"`
	Model               string    `json:"model"`
	Operation           string    `json:"operation"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64     `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens     int64     `json:"reasoning_tokens,omitempty"`
	UsageMissing        bool      `json:"usage_missing,omitempty"`
	Cost                float64   `json:"cost"`
	Unpriced            bool      `json:"unpriced"`
	// Duration follows the Jaeger API convention and is expressed in microseconds.
	Duration int64  `json:"duration"`
	TraceID  string `json:"trace_id"`
	SpanID   string `json:"span_id,omitempty"`
	Status   string `json:"status"`
}

type llmAggregate struct {
	Calls         int64   `json:"calls"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	Cost          float64 `json:"cost"`
	Unpriced      bool    `json:"unpriced"`
	UnpricedCalls int64   `json:"unpriced_calls"`
}

type llmModelAggregate struct {
	Model string `json:"model"`
	llmAggregate
}

type llmServiceAggregate struct {
	Service string `json:"service"`
	llmAggregate
}

func (g *Gateway) llmSummary(c fiber.Ctx) error {
	start, end, ok := g.llmRange(c)
	if !ok {
		return nil
	}
	expr := fmt.Sprintf(
		"%s | stats by (%s) count() as calls, %s | limit %d",
		llmBaseFilter,
		llmStatsGroupByFields,
		llmTokenSumStats,
		llmMaxStatsGroups+1,
	)
	query := url.Values{
		"query": {expr},
		"start": {start.Format(time.RFC3339Nano)},
		"end":   {end.Format(time.RFC3339Nano)},
	}

	models := make(map[string]*llmAggregate)
	services := make(map[string]*llmAggregate)
	var total llmAggregate
	groupCount := 0
	truncated := false

	err := g.streamLLMTracesQuery(c.Context(), currentProject(c), query, func(row map[string]json.RawMessage) error {
		groupCount++
		if groupCount > llmMaxStatsGroups {
			truncated = true
			return nil
		}
		calls := rawNonNegativeInt64(row, "calls")
		if calls <= 0 {
			return nil
		}
		service := rawString(row, "resource_attr:service.name", "service.name")
		if service == "" {
			service = "unknown"
		}
		model := rawString(row, "span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model")
		if model == "" {
			model = "unknown"
		}
		provider := rawString(row, "span_attr:gen_ai.provider.name", "span_attr:gen_ai.system")
		usageMissing := rawBool(row, "span_attr:gen_ai.usage.missing")
		inputTokens := rawNonNegativeInt64(row, "input_tokens")
		outputTokens := rawNonNegativeInt64(row, "output_tokens")
		cacheReadTokens := rawNonNegativeInt64(row, "cache_read_tokens")
		cacheCreationTokens := rawNonNegativeInt64(row, "cache_creation_tokens")
		reasoningTokens := rawNonNegativeInt64(row, "reasoning_tokens")
		cost, unpriced := computeLLMCost(
			g.llmPricing,
			model,
			provider,
			usageMissing,
			inputTokens,
			outputTokens,
			cacheReadTokens,
			cacheCreationTokens,
			reasoningTokens,
		)
		addLLMAggregateDelta(&total, calls, inputTokens, outputTokens, cost, unpriced)
		if models[model] == nil {
			models[model] = &llmAggregate{}
		}
		addLLMAggregateDelta(models[model], calls, inputTokens, outputTokens, cost, unpriced)
		if services[service] == nil {
			services[service] = &llmAggregate{}
		}
		addLLMAggregateDelta(services[service], calls, inputTokens, outputTokens, cost, unpriced)
		return nil
	})
	if err != nil {
		return llmBackendError(c, err)
	}

	byModel := make([]llmModelAggregate, 0, len(models))
	for model, aggregate := range models {
		byModel = append(byModel, llmModelAggregate{Model: model, llmAggregate: *aggregate})
	}
	sort.Slice(byModel, func(i, j int) bool {
		return byModel[i].Calls > byModel[j].Calls ||
			(byModel[i].Calls == byModel[j].Calls && byModel[i].Model < byModel[j].Model)
	})
	byService := make([]llmServiceAggregate, 0, len(services))
	for service, aggregate := range services {
		byService = append(byService, llmServiceAggregate{Service: service, llmAggregate: *aggregate})
	}
	sort.Slice(byService, func(i, j int) bool {
		return byService[i].Calls > byService[j].Calls ||
			(byService[i].Calls == byService[j].Calls && byService[i].Service < byService[j].Service)
	})
	return c.JSON(fiber.Map{
		"total_calls":         total.Calls,
		"total_input_tokens":  total.InputTokens,
		"total_output_tokens": total.OutputTokens,
		"total_cost":          total.Cost,
		"unpriced":            total.Unpriced,
		"unpriced_calls":      total.UnpricedCalls,
		"by_model":            byModel,
		"by_service":          byService,
		"truncated":           truncated,
	})
}

func (g *Gateway) llmCalls(c fiber.Ctx) error {
	start, end, ok := g.llmRange(c)
	if !ok {
		return nil
	}
	limit, err := parseBoundedInt(c.Query("limit"), llmDefaultLimit, 1, llmMaxCallLimit)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	service, model := strings.TrimSpace(c.Query("service")), strings.TrimSpace(c.Query("model"))
	expr := buildLLMCallsQuery(service, model, limit+1)
	query := url.Values{
		"query": {expr},
		"start": {start.Format(time.RFC3339Nano)},
		"end":   {end.Format(time.RFC3339Nano)},
	}

	filtered := make([]llmCall, 0, limit+1)
	err = g.streamLLMTracesQuery(c.Context(), currentProject(c), query, func(row map[string]json.RawMessage) error {
		call, valid := extractLLMCallRow(row, g.llmPricing)
		if !valid {
			return nil
		}
		if service != "" && !strings.EqualFold(service, call.Service) {
			return nil
		}
		if model != "" && !strings.EqualFold(model, call.Model) {
			return nil
		}
		filtered = append(filtered, call)
		return nil
	})
	if err != nil {
		return llmBackendError(c, err)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].Time.After(filtered[j].Time)
	})
	truncated := len(filtered) > limit
	if truncated {
		filtered = filtered[:limit]
	}
	return c.JSON(fiber.Map{
		"calls":     filtered,
		"count":     len(filtered),
		"limit":     limit,
		"truncated": truncated,
	})
}

type llmTimeseriesPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

func (g *Gateway) llmTimeseries(c fiber.Ctx) error {
	start, end, ok := g.llmRange(c)
	if !ok {
		return nil
	}
	metric := strings.ToLower(strings.TrimSpace(c.Query("metric")))
	if metric == "" {
		metric = "calls"
	}
	if metric != "calls" && metric != "tokens" && metric != "cost" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "metric must be calls, tokens, or cost"})
	}
	step := time.Hour
	if value := strings.TrimSpace(c.Query("step")); value != "" {
		var err error
		step, err = time.ParseDuration(value)
		if err != nil {
			return c.Status(http.StatusBadRequest).
				JSON(fiber.Map{"error": "step must be a Go duration such as 5m or 1h"})
		}
	}
	if step < time.Minute || step > 24*time.Hour {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "step must be between 1m and 24h"})
	}
	firstBucket := start.Truncate(step)
	bucketCount := int((end.Sub(firstBucket) + step - 1) / step)
	if bucketCount <= 0 || bucketCount > llmMaxBuckets {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "time range produces too many buckets"})
	}

	expr := buildLLMTimeseriesQuery(metric, step)
	query := url.Values{
		"query": {expr},
		"start": {start.Format(time.RFC3339Nano)},
		"end":   {end.Format(time.RFC3339Nano)},
	}
	points := make([]llmTimeseriesPoint, bucketCount)
	for i := range points {
		points[i].Time = firstBucket.Add(time.Duration(i) * step)
	}

	err := g.streamLLMTracesQuery(c.Context(), currentProject(c), query, func(row map[string]json.RawMessage) error {
		bucketTime, ok := parseRowTime(row, false)
		if !ok {
			return nil
		}
		index, inRange := llmBucketIndex(bucketTime, firstBucket, step, len(points))
		if !inRange {
			return nil
		}
		switch metric {
		case "calls":
			calls := rawNonNegativeInt64(row, "calls")
			if calls > 0 {
				points[index].Value += float64(calls)
			}
		case "tokens":
			inTok := rawNonNegativeInt64(row, "input_tokens")
			outTok := rawNonNegativeInt64(row, "output_tokens")
			points[index].Value += float64(saturatingAddInt64(inTok, outTok))
		case "cost":
			model := rawString(row, "span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model")
			if model == "" {
				model = "unknown"
			}
			provider := rawString(row, "span_attr:gen_ai.provider.name", "span_attr:gen_ai.system")
			usageMissing := rawBool(row, "span_attr:gen_ai.usage.missing")
			inputTokens := rawNonNegativeInt64(row, "input_tokens")
			outputTokens := rawNonNegativeInt64(row, "output_tokens")
			cacheReadTokens := rawNonNegativeInt64(row, "cache_read_tokens")
			cacheCreationTokens := rawNonNegativeInt64(row, "cache_creation_tokens")
			reasoningTokens := rawNonNegativeInt64(row, "reasoning_tokens")
			cost, _ := computeLLMCost(
				g.llmPricing,
				model,
				provider,
				usageMissing,
				inputTokens,
				outputTokens,
				cacheReadTokens,
				cacheCreationTokens,
				reasoningTokens,
			)
			points[index].Value += cost
		}
		return nil
	})
	if err != nil {
		return llmBackendError(c, err)
	}
	return c.JSON(fiber.Map{
		"metric":    metric,
		"step":      step.String(),
		"points":    points,
		"truncated": false,
	})
}

func buildLLMCallsQuery(service, model string, limit int) string {
	var b strings.Builder
	b.WriteString(llmBaseFilter)
	if service != "" {
		b.WriteString(" AND ")
		b.WriteString(buildServiceFilter(service))
	}
	if model != "" {
		b.WriteString(" AND ")
		b.WriteString(buildModelFilter(model))
	}
	b.WriteString(" | fields ")
	b.WriteString(llmProjectedFields)
	b.WriteString(" | sort by (_time) desc | limit ")
	b.WriteString(strconv.Itoa(limit))
	return b.String()
}

func buildServiceFilter(service string) string {
	quoted := strconv.Quote(service)
	if strings.EqualFold(service, "unknown") {
		return `(("resource_attr:service.name":="" AND "service.name":="") OR ` +
			`"resource_attr:service.name":=` + quoted + ` OR "service.name":=` + quoted + `)`
	}
	return `("resource_attr:service.name":=` + quoted +
		` OR ("resource_attr:service.name":="" AND "service.name":=` + quoted + `))`
}

func buildModelFilter(model string) string {
	quoted := strconv.Quote(model)
	if strings.EqualFold(model, "unknown") {
		return `(("span_attr:gen_ai.response.model":="" AND "span_attr:gen_ai.request.model":="") OR ` +
			`"span_attr:gen_ai.response.model":=` + quoted + ` OR "span_attr:gen_ai.request.model":=` + quoted + `)`
	}
	return `("span_attr:gen_ai.response.model":=` + quoted +
		` OR ("span_attr:gen_ai.response.model":="" AND "span_attr:gen_ai.request.model":=` + quoted + `))`
}

func buildLLMTimeseriesQuery(metric string, step time.Duration) string {
	stepExpr := formatLogsQLStep(step)
	switch metric {
	case "calls":
		return fmt.Sprintf("%s | stats by (_time:%s) count() as calls", llmBaseFilter, stepExpr)
	case "tokens":
		return fmt.Sprintf(
			`%s | stats by (_time:%s) `+
				`sum("span_attr:gen_ai.usage.input_tokens") if ("span_attr:gen_ai.usage.input_tokens":>0) as input_tokens, `+
				`sum("span_attr:gen_ai.usage.output_tokens") if ("span_attr:gen_ai.usage.output_tokens":>0) as output_tokens`,
			llmBaseFilter,
			stepExpr,
		)
	default:
		return fmt.Sprintf(
			"%s | stats by (_time:%s, %s) %s",
			llmBaseFilter,
			stepExpr,
			llmCostGroupByFields,
			llmTokenSumStats,
		)
	}
}

func formatLogsQLStep(step time.Duration) string {
	if step%time.Second == 0 {
		return strconv.FormatInt(int64(step/time.Second), 10) + "s"
	}
	return strconv.FormatInt(step.Milliseconds(), 10) + "ms"
}

func llmBucketIndex(at, firstBucket time.Time, step time.Duration, bucketCount int) (int, bool) {
	if at.IsZero() || at.Before(firstBucket) || step <= 0 || bucketCount <= 0 {
		return 0, false
	}
	diff := at.Sub(firstBucket)
	index := int(diff / step)
	if index < 0 || index >= bucketCount {
		return 0, false
	}
	return index, true
}

func (g *Gateway) llmRange(c fiber.Ctx) (time.Time, time.Time, bool) {
	end := g.now().UTC()
	start := end.Add(-llmDefaultWindow)
	var err error
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		end, err = parseLLMTime(raw)
		if err != nil {
			_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid end: use RFC3339 or a Unix timestamp"})
			return time.Time{}, time.Time{}, false
		}
	}
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		start, err = parseLLMTime(raw)
		if err != nil {
			_ = c.Status(http.StatusBadRequest).
				JSON(fiber.Map{"error": "invalid start: use RFC3339 or a Unix timestamp"})
			return time.Time{}, time.Time{}, false
		}
	} else if strings.TrimSpace(c.Query("end")) != "" {
		start = end.Add(-llmDefaultWindow)
	}
	if !end.After(start) || end.Sub(start) > llmMaxWindow {
		_ = c.Status(http.StatusBadRequest).
			JSON(fiber.Map{"error": "time range must be positive and no longer than 7d"})
		return time.Time{}, time.Time{}, false
	}
	return start.UTC(), end.UTC(), true
}

func parseLLMTime(raw string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC(), nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	switch len(strings.TrimLeft(raw, "-")) {
	case 10:
		return time.Unix(value, 0).UTC(), nil
	case 13:
		return time.UnixMilli(value).UTC(), nil
	case 16:
		return time.UnixMicro(value).UTC(), nil
	case 19:
		return time.Unix(0, value).UTC(), nil
	default:
		return time.Time{}, errors.New("unsupported Unix timestamp precision")
	}
}

func parseBoundedInt(raw string, fallback, low, high int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < low || value > high {
		return 0, fmt.Errorf("limit must be between %d and %d", low, high)
	}
	return value, nil
}

type boundedStreamReader struct {
	reader    io.Reader
	remaining int64
	exceeded  bool
}

func (r *boundedStreamReader) Read(p []byte) (int, error) {
	if r.exceeded {
		return 0, errors.New("LLM query response exceeded size limit")
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.reader.Read(p)
	if int64(n) > r.remaining {
		r.remaining = 0
		r.exceeded = true
		return 0, errors.New("LLM query response exceeded size limit")
	}
	r.remaining -= int64(n)
	return n, err
}

func (g *Gateway) streamLLMTracesQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	query url.Values,
	onRow func(map[string]json.RawMessage) error,
) error {
	if project.ID <= 0 {
		return errors.New("invalid project ID for tenant isolation")
	}
	params := cloneQueryValues(query)
	const path = "/select/logsql/query"
	disableTracesLatencyOffset(path, params)
	target := strings.TrimRight(g.storage.TracesQueryBase(), "/") + path
	if encoded := params.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	g.storage.SetTenantHeaders(req.Header, project.ID)
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf(
			"query backend returned HTTP %d: %s",
			resp.StatusCode,
			firstRunes(strings.TrimSpace(string(snippet)), 500),
		)
	}
	limited := &boundedStreamReader{reader: resp.Body, remaining: llmMaxResponseBody}
	return decodeNDJSONStream(limited, onRow)
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

func extractLLMCalls(body []byte, pricing llmPricingLookup) ([]llmCall, error) {
	result := make([]llmCall, 0)
	err := decodeNDJSONStream(bytes.NewReader(body), func(row map[string]json.RawMessage) error {
		if call, ok := extractLLMCallRow(row, pricing); ok {
			result = append(result, call)
		}
		return nil
	})
	return result, err
}

func extractLLMCallRow(row map[string]json.RawMessage, pricing llmPricingLookup) (llmCall, bool) {
	operation := rawString(row, "span_attr:gen_ai.operation.name")
	if operation == "" {
		return llmCall{}, false
	}
	callTime, ok := parseRowTime(row, true)
	if !ok {
		return llmCall{}, false
	}
	model := rawString(row, "span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model")
	if model == "" {
		model = "unknown"
	}
	service := rawString(row, "resource_attr:service.name", "service.name")
	if service == "" {
		service = "unknown"
	}
	provider := rawString(row, "span_attr:gen_ai.provider.name", "span_attr:gen_ai.system")
	inputTokens := rawNonNegativeInt64(row, "span_attr:gen_ai.usage.input_tokens")
	outputTokens := rawNonNegativeInt64(row, "span_attr:gen_ai.usage.output_tokens")
	cacheReadTokens := rawNonNegativeInt64(row, "span_attr:gen_ai.usage.cache_read.input_tokens")
	cacheCreationTokens := rawNonNegativeInt64(row, "span_attr:gen_ai.usage.cache_creation.input_tokens")
	reasoningTokens := rawNonNegativeInt64(row, "span_attr:gen_ai.usage.reasoning_tokens")
	usageMissing := rawBool(row, "span_attr:gen_ai.usage.missing")
	cost, unpriced := computeLLMCost(
		pricing,
		model,
		provider,
		usageMissing,
		inputTokens,
		outputTokens,
		cacheReadTokens,
		cacheCreationTokens,
		reasoningTokens,
	)
	durationMicros := rawNonNegativeInt64(row, "duration") / 1000
	return llmCall{
		Time:                callTime,
		Service:             service,
		Model:               model,
		Operation:           operation,
		InputTokens:         inputTokens,
		OutputTokens:        outputTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
		ReasoningTokens:     reasoningTokens,
		UsageMissing:        usageMissing,
		Cost:                cost,
		Unpriced:            unpriced,
		Duration:            durationMicros,
		TraceID:             normalizeTraceID(rawString(row, "trace_id")),
		SpanID:              rawString(row, "span_id"),
		Status:              llmStatus(row),
	}, true
}

func computeLLMCost(
	pricing llmPricingLookup,
	model, provider string,
	usageMissing bool,
	inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, reasoningTokens int64,
) (float64, bool) {
	if usageMissing {
		return 0, true
	}
	if pricing == nil {
		return 0, true
	}
	cost, unpriced := pricing.CostWithUsage(model, llmcost.Usage{
		Provider:            provider,
		InputTokens:         inputTokens,
		OutputTokens:        outputTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
		ReasoningTokens:     reasoningTokens,
	})
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return 0, true
	}
	return cost, unpriced
}

func parseRowTime(row map[string]json.RawMessage, preferStartNano bool) (time.Time, bool) {
	if preferStartNano {
		if startNanos := rawInt64(row, "start_time_unix_nano"); startNanos > 0 {
			return time.Unix(0, startNanos).UTC(), true
		}
	}
	if value := rawString(row, "_time"); value != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC(), true
		}
	}
	if !preferStartNano {
		if startNanos := rawInt64(row, "start_time_unix_nano"); startNanos > 0 {
			return time.Unix(0, startNanos).UTC(), true
		}
	}
	return time.Time{}, false
}

func rawString(row map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw := row[key]
		if len(raw) == 0 {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func rawBool(row map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		raw := row[key]
		if len(raw) == 0 {
			continue
		}
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			if b {
				return true
			}
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if strings.EqualFold(strings.TrimSpace(text), "true") {
				return true
			}
		}
	}
	return false
}

func rawInt64(row map[string]json.RawMessage, key string) int64 {
	raw := row[key]
	if len(raw) == 0 {
		return 0
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return parseInt64Text(text)
	}
	var intVal int64
	if json.Unmarshal(raw, &intVal) == nil {
		return intVal
	}
	var floatVal float64
	if json.Unmarshal(raw, &floatVal) == nil {
		return floatToInt64(floatVal)
	}
	return 0
}

func rawNonNegativeInt64(row map[string]json.RawMessage, key string) int64 {
	v := rawInt64(row, key)
	if v < 0 {
		return 0
	}
	if v > llmMaxSafeInt {
		return llmMaxSafeInt
	}
	return v
}

func parseInt64Text(text string) int64 {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	if value, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return value
	}
	floatVal, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	return floatToInt64(floatVal)
}

func floatToInt64(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if v <= float64(math.MinInt64) {
		return math.MinInt64
	}
	return int64(math.Trunc(v))
}

func normalizeTraceID(raw string) string {
	const fallbackZeroTraceID = "00000000000000000000000000000000"
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	cleaned = strings.TrimPrefix(cleaned, "0x")
	if strings.Contains(cleaned, "-") {
		stripped := strings.ReplaceAll(cleaned, "-", "")
		if len(stripped) == 32 {
			cleaned = stripped
		}
	}
	if cleaned == "" || len(cleaned) > 32 || !isHexString(cleaned) {
		return fallbackZeroTraceID
	}
	if len(cleaned) == 16 || len(cleaned) == 32 {
		return cleaned
	}
	if len(cleaned) < 16 {
		return strings.Repeat("0", 16-len(cleaned)) + cleaned
	}
	return strings.Repeat("0", 32-len(cleaned)) + cleaned
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) > 0
}

func llmStatus(row map[string]json.RawMessage) string {
	if rawInt64(row, "status_code") == 2 || rawString(row, "span_attr:error.type", "error.type") != "" ||
		rawBool(row, "span_attr:error", "error") {
		return "error"
	}
	if rawInt64(row, "status_code") == 1 {
		return "ok"
	}
	return "unset"
}

func saturatingAddInt64(a, b int64) int64 {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	if a >= llmMaxSafeInt-b {
		return llmMaxSafeInt
	}
	return a + b
}

func addLLMAggregateDelta(
	aggregate *llmAggregate,
	calls, inputTokens, outputTokens int64,
	cost float64,
	unpriced bool,
) {
	aggregate.Calls = saturatingAddInt64(aggregate.Calls, calls)
	aggregate.InputTokens = saturatingAddInt64(aggregate.InputTokens, inputTokens)
	aggregate.OutputTokens = saturatingAddInt64(aggregate.OutputTokens, outputTokens)
	if !math.IsNaN(cost) && !math.IsInf(cost, 0) && cost > 0 {
		aggregate.Cost += cost
	}
	if unpriced {
		aggregate.Unpriced = true
		aggregate.UnpricedCalls = saturatingAddInt64(aggregate.UnpricedCalls, calls)
	}
}

func llmBackendError(c fiber.Ctx, err error) error {
	slog.Warn("LLM span query failed", "error", err)
	return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "LLM span query failed"})
}
