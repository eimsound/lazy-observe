package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/eimsound/lazy-observe/internal/aiassistant"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/query"
	"github.com/gofiber/fiber/v3"
)

const (
	assistantSchemaTTL       = aiassistant.SchemaTTL
	assistantMaxResponseBody = query.DefaultMaxResponseBytes
	assistantMaxStatsKeys    = aiassistant.MaxStatsKeys
	assistantMaxStatsKeyLen  = aiassistant.MaxStatsKeyLen
	assistantRateLimit       = aiassistant.RateLimit
)

type (
	assistantSchema     = aiassistant.Schema
	assistantQueryInput = aiassistant.QueryInput
	assistantGenerated  = aiassistant.GeneratedQuery
)

type assistantService struct {
	svc *aiassistant.Service
}

func newAssistantService(g *Gateway) *assistantService {
	return &assistantService{
		svc: aiassistant.New(aiassistant.Config{
			Querier: g.querier,
			Limits:  g.limits,
			Now:     g.now,
		}),
	}
}

func (s *assistantService) allow(userID int) bool {
	return s.svc.Allow(context.Background(), userID)
}

func (s *assistantService) schema(
	ctx context.Context,
	project meta.ProjectInfo,
	signal string,
) (assistantSchema, error) {
	return s.svc.Schema(ctx, project, signal)
}

func (s *assistantService) querySchema(
	ctx context.Context,
	project meta.ProjectInfo,
	signal string,
) (map[string]assistantSchema, error) {
	return s.svc.QuerySchema(ctx, project, signal)
}

func (g *Gateway) aiAssistantQuery(c fiber.Ctx) error {
	state, ok := g.assistantReady(c)
	if !ok {
		return nil
	}
	user, _ := c.Locals(userLocal).(meta.UserInfo)
	if !g.aiAssistant.allow(user.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": "AI assistant rate limit exceeded"})
	}
	var rawInput aiassistant.QueryInput
	if json.Unmarshal(requestBody(c), &rawInput) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	input, err := aiassistant.NormalizeQueryInput(rawInput)
	if err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}
	start, end, err := aiassistant.ResolveTimeRange(input.TimeRange, g.now())
	if err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}
	project := currentProject(c)
	contextSchema, err := g.aiAssistant.querySchema(c.Context(), project, input.Signal)
	if err != nil {
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "schema context collection failed"})
	}
	generated, raw, usage, err := g.aiAssistant.svc.GenerateQuery(c.Context(), state.client, input, contextSchema)
	if err != nil {
		var contractErr *aiassistant.ContractError
		if !errors.As(err, &contractErr) {
			return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI query generation failed"})
		}
		return c.Status(http.StatusUnprocessableEntity).
			JSON(fiber.Map{"error": "AI output did not match the query contract", "raw_output": firstRunes(raw, 500)})
	}
	slog.Info(
		"AI assistant generated query",
		"user_id", user.ID,
		"username", user.Username,
		"project", project.Slug,
		"question", input.Question,
		"signal", generated.Signal,
		"query", generated.Query,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"total_tokens", usage.TotalTokens(),
	)

	result, executionErr := g.executeAssistantQuery(c.Context(), project, generated, start, end)
	response := fiber.Map{"signal": generated.Signal, "query": generated.Query, "explanation": generated.Explanation}
	if executionErr != nil {
		response["execution_error"] = executionErr.Error()
	} else {
		response["result"] = result
	}
	return c.JSON(response)
}

func (g *Gateway) generateAssistantQuery(
	ctx context.Context,
	input assistantQueryInput,
	schema map[string]assistantSchema,
) (assistantGenerated, string, error) {
	state := g.aiState()
	if !state.enabled || state.client == nil {
		return assistantGenerated{}, "", errors.New("AI integration is disabled")
	}
	generated, raw, _, err := g.aiAssistant.svc.GenerateQuery(ctx, state.client, input, schema)
	return generated, raw, err
}

func parseAssistantGenerated(raw string) (assistantGenerated, error) {
	return aiassistant.ParseGenerated(raw)
}

func parseAssistantTime(value string) (time.Time, error) {
	return aiassistant.ParseTime(value)
}

func assistantStep(start, end string) string {
	return aiassistant.ComputeStep(start, end)
}

func (g *Gateway) executeAssistantQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	generated assistantGenerated,
	start, end string,
) (any, error) {
	result, err := g.aiAssistant.svc.ExecuteQuery(ctx, project, generated, start, end)
	if err != nil {
		return nil, err
	}
	if logsRes, ok := result.(aiassistant.LogsResult); ok {
		return fiber.Map{
			"rows":      logsRes.Rows,
			"count":     logsRes.Count,
			"limit":     logsRes.Limit,
			"truncated": logsRes.Truncated,
		}, nil
	}
	return result, nil
}

var errQueryResponseTooLarge = query.ErrResponseTooLarge

func (g *Gateway) isolatedQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	signal, path string,
	params url.Values,
) ([]byte, error) {
	return g.querier.Query(ctx, project, query.Signal(signal), path, params)
}

func (g *Gateway) isolatedQueryWithLimit(
	ctx context.Context,
	project meta.ProjectInfo,
	signal, path string,
	params url.Values,
	maxBytes int64,
) ([]byte, error) {
	return g.querier.QueryWithLimit(ctx, project, query.Signal(signal), path, params, maxBytes)
}

func disableTracesLatencyOffset(path string, params url.Values) {
	query.DisableTracesLatencyOffset(path, params)
}

func cloneQueryValues(source url.Values) url.Values {
	return query.CloneValues(source)
}

func (g *Gateway) aiAssistantExplain(c fiber.Ctx) error {
	state, ok := g.assistantReady(c)
	if !ok {
		return nil
	}
	user, _ := c.Locals(userLocal).(meta.UserInfo)
	if !g.aiAssistant.allow(user.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": "AI assistant rate limit exceeded"})
	}
	var input aiassistant.ExplainInput
	if json.Unmarshal(requestBody(c), &input) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	result, err := g.aiAssistant.svc.Explain(c.Context(), state.client, input)
	if err != nil {
		var validationErr *aiassistant.ValidationError
		if errors.As(err, &validationErr) {
			return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": validationErr.Error()})
		}
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI explanation failed"})
	}
	return c.JSON(fiber.Map{"explanation": result.Explanation})
}

func (g *Gateway) assistantReady(c fiber.Ctx) (aiSnapshot, bool) {
	state := g.aiState()
	if !state.enabled || state.client == nil {
		_ = c.Status(http.StatusConflict).
			JSON(fiber.Map{"error": "AI integration is disabled", "message": "Enable and configure an AI provider in the admin AI settings."})
		return aiSnapshot{}, false
	}
	return state, true
}
