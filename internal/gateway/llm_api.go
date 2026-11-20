package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	llmDefaultWindow  = 24 * time.Hour
	llmMaxWindow      = 7 * 24 * time.Hour
	llmMaxSpanRecords = 5000
	llmDefaultLimit   = 100
	llmMaxCallLimit   = 1000
	llmMaxBuckets     = 2000
)

type llmCall struct {
	Time         time.Time `json:"time"`
	Service      string    `json:"service"`
	Model        string    `json:"model"`
	Operation    string    `json:"operation"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Cost         float64   `json:"cost"`
	Unpriced     bool      `json:"unpriced"`
	// Duration follows the Jaeger API convention and is expressed in microseconds.
	Duration int64  `json:"duration"`
	TraceID  string `json:"trace_id"`
	Status   string `json:"status"`
}

type llmAggregate struct {
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Unpriced     bool    `json:"unpriced"`
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
	calls, sourceTruncated, err := g.queryLLMCalls(c.Context(), currentProject(c), start, end)
	if err != nil {
		return llmBackendError(c, err)
	}
	models := make(map[string]*llmAggregate)
	services := make(map[string]*llmAggregate)
	var total llmAggregate
	for _, call := range calls {
		addLLMAggregate(&total, call)
		if models[call.Model] == nil {
			models[call.Model] = &llmAggregate{}
		}
		addLLMAggregate(models[call.Model], call)
		if services[call.Service] == nil {
			services[call.Service] = &llmAggregate{}
		}
		addLLMAggregate(services[call.Service], call)
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
		"total_calls": total.Calls, "total_input_tokens": total.InputTokens,
		"total_output_tokens": total.OutputTokens, "total_cost": total.Cost,
		"unpriced": total.Unpriced, "by_model": byModel, "by_service": byService,
		"truncated": sourceTruncated,
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
	calls, sourceTruncated, err := g.queryLLMCalls(c.Context(), currentProject(c), start, end)
	if err != nil {
		return llmBackendError(c, err)
	}
	service, model := strings.TrimSpace(c.Query("service")), strings.TrimSpace(c.Query("model"))
	filtered := make([]llmCall, 0, min(limit+1, len(calls)))
	for _, call := range calls {
		if service != "" && !strings.EqualFold(service, call.Service) {
			continue
		}
		if model != "" && !strings.EqualFold(model, call.Model) {
			continue
		}
		filtered = append(filtered, call)
	}
	truncated := sourceTruncated || len(filtered) > limit
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return c.JSON(fiber.Map{"calls": filtered, "count": len(filtered), "limit": limit, "truncated": truncated})
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
	calls, sourceTruncated, err := g.queryLLMCalls(c.Context(), currentProject(c), start, end)
	if err != nil {
		return llmBackendError(c, err)
	}
	points := make([]llmTimeseriesPoint, bucketCount)
	for i := range points {
		points[i].Time = firstBucket.Add(time.Duration(i) * step)
	}
	for _, call := range calls {
		index := int(call.Time.Sub(firstBucket) / step)
		if index < 0 || index >= len(points) {
			continue
		}
		switch metric {
		case "calls":
			points[index].Value++
		case "tokens":
			points[index].Value += float64(call.InputTokens + call.OutputTokens)
		case "cost":
			points[index].Value += call.Cost
		}
	}
	return c.JSON(fiber.Map{"metric": metric, "step": step.String(), "points": points, "truncated": sourceTruncated})
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

func (g *Gateway) queryLLMCalls(
	ctx context.Context,
	project meta.ProjectInfo,
	start, end time.Time,
) ([]llmCall, bool, error) {
	query := url.Values{
		"query": {`"span_attr:gen_ai.operation.name":*`},
		"start": {start.Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)},
		"limit": {strconv.Itoa(llmMaxSpanRecords + 1)},
	}
	body, err := g.isolatedQuery(ctx, project, "traces", "/select/logsql/query", query)
	if err != nil {
		return nil, false, err
	}
	calls, err := extractLLMCalls(body, g.llmPricing)
	if err != nil {
		return nil, false, err
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].Time.After(calls[j].Time) })
	truncated := len(calls) > llmMaxSpanRecords
	if truncated {
		calls = calls[:llmMaxSpanRecords]
	}
	return calls, truncated, nil
}

func extractLLMCalls(body []byte, pricing interface {
	Cost(string, int64, int64) (float64, bool)
}) ([]llmCall, error) {
	result := make([]llmCall, 0)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("decode VictoriaTraces span: %w", err)
		}
		operation := rawString(row, "span_attr:gen_ai.operation.name", "span_attr:operation.name")
		if operation == "" {
			continue
		}
		model := rawString(row, "span_attr:gen_ai.response.model", "span_attr:gen_ai.request.model")
		if model == "" {
			model = "unknown"
		}
		service := rawString(row, "resource_attr:service.name", "service.name")
		if service == "" {
			service = "unknown"
		}
		inputTokens := rawInt64(row, "span_attr:gen_ai.usage.input_tokens")
		outputTokens := rawInt64(row, "span_attr:gen_ai.usage.output_tokens")
		cost, unpriced := pricing.Cost(model, inputTokens, outputTokens)
		call := llmCall{
			Service: service, Model: model, Operation: operation,
			InputTokens: inputTokens, OutputTokens: outputTokens, Cost: cost, Unpriced: unpriced,
			Duration: rawInt64(row, "duration") / 1000, TraceID: rawString(row, "trace_id"), Status: llmStatus(row),
		}
		if startNanos := rawInt64(row, "start_time_unix_nano"); startNanos > 0 {
			call.Time = time.Unix(0, startNanos).UTC()
		} else if value := rawString(row, "_time"); value != "" {
			call.Time, _ = time.Parse(time.RFC3339Nano, value)
		}
		result = append(result, call)
	}
	return result, scanner.Err()
}

func rawString(row map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if len(row[key]) != 0 && json.Unmarshal(row[key], &value) == nil {
			return value
		}
	}
	return ""
}

func rawInt64(row map[string]json.RawMessage, key string) int64 {
	raw := row[key]
	if len(raw) == 0 {
		return 0
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, _ := strconv.ParseInt(text, 10, 64)
		return value
	}
	var value int64
	_ = json.Unmarshal(raw, &value)
	return value
}

func llmStatus(row map[string]json.RawMessage) string {
	if rawInt64(row, "status_code") == 2 || rawString(row, "span_attr:error.type", "error.type") != "" ||
		strings.EqualFold(rawString(row, "span_attr:error", "error"), "true") {
		return "error"
	}
	if rawInt64(row, "status_code") == 1 {
		return "ok"
	}
	return "unset"
}

func addLLMAggregate(aggregate *llmAggregate, call llmCall) {
	aggregate.Calls++
	aggregate.InputTokens += call.InputTokens
	aggregate.OutputTokens += call.OutputTokens
	aggregate.Cost += call.Cost
	aggregate.Unpriced = aggregate.Unpriced || call.Unpriced
}

func llmBackendError(c fiber.Ctx, err error) error {
	return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "LLM span query failed", "detail": err.Error()})
}
