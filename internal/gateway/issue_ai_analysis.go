package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/issueai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	issueAIContextBudget      = issueai.ContextBudget
	issueAIRateLimit          = issueai.ManualRateLimit
	issueAIAutoRateLimit      = issueai.AutoRateLimit
	issueAIAutoDailyBudgetMax = issueai.MaxAutoDailyBudget
	issueAIProjectQueueCap    = issueai.ProjectQueueCap
)

type (
	issueAIAnalysis = issueai.Analysis
	issueAIEvent    = issueai.Event
	contextSection  = ai.UntrustedSection
)

type issueAIService struct {
	svc *issueai.Service
}

func newIssueAIService(g *Gateway, defaultDailyBudget int) *issueAIService {
	dynamicAI := g.ai != nil && g.ai.store != nil
	return &issueAIService{
		svc: issueai.New(issueai.Config{
			Projects: g.store,
			Issues:   g.issues,
			Querier:  g.querier,
			Limits:   g.limits,
			ClientProvider: func() issueai.ClientSnapshot {
				state := g.aiState()
				return issueai.ClientSnapshot{Enabled: state.enabled, Client: state.client}
			},
			DynamicAI:          dynamicAI,
			DefaultDailyBudget: defaultDailyBudget,
			MaxErrorLineBytes:  maxVLogsErrorLineBytes,
			Now:                g.now,
			Logger:             g.logger,
			MeterProvider:      g.meterProvider,
		}),
	}
}

func (s *issueAIService) HandleIssueChange(ctx context.Context, change errortrack.IssueChange) error {
	if s == nil {
		return nil
	}
	return s.svc.HandleIssueChange(ctx, change)
}

func (s *issueAIService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.svc.Close(ctx)
}

func (s *issueAIService) allow(ctx context.Context, projectID int) bool {
	return s.svc.AllowManual(ctx, projectID)
}

func (s *issueAIService) allowManual(ctx context.Context, projectID int) bool {
	return s.svc.AllowManual(ctx, projectID)
}

func (s *issueAIService) allowAuto(ctx context.Context, projectID int) bool {
	return s.svc.AllowAuto(ctx, projectID)
}

func (s *issueAIService) allowAutoDaily(ctx context.Context, projectID, budget int) bool {
	return s.svc.AllowAutoDaily(ctx, projectID, budget)
}

func (g *Gateway) HandleIssueChange(ctx context.Context, change errortrack.IssueChange) error {
	if g.issueAI != nil {
		_ = g.issueAI.HandleIssueChange(ctx, change)
	}
	if g.alerts != nil {
		return g.alerts.HandleIssueChange(ctx, change)
	}
	return nil
}

// HandleErrorEvents records one completed error Inbox batch into the durable
// error_rate_spike window with a single metadata write transaction.
func (g *Gateway) HandleErrorEvents(ctx context.Context, occurrences []errortrack.ErrorOccurrence) error {
	if g.alerts == nil {
		return nil
	}
	return g.alerts.RecordErrorEvents(ctx, occurrences)
}

type projectAIConfigWire struct {
	AutoAnalysisEnabled *bool `json:"auto_analysis_enabled"`
	DailyBudget         *int  `json:"daily_budget"`
}

func (g *Gateway) getProjectAIConfig(c fiber.Ctx) error {
	if g.issueAI == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "issue AI service unavailable"})
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.store != nil {
		if _, err := g.store.GetProject(c.Context(), id); err != nil {
			return apiError(c, err)
		}
	}
	return c.JSON(g.issueAI.svc.ProjectPolicy(id))
}

func (g *Gateway) putProjectAIConfig(c fiber.Ctx) error {
	if g.issueAI == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "issue AI service unavailable"})
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.store != nil {
		if _, err := g.store.GetProject(c.Context(), id); err != nil {
			return apiError(c, err)
		}
	}
	var body projectAIConfigWire
	if c.Bind().Body(&body) != nil {
		return badRequest(c)
	}
	policy := g.issueAI.svc.ProjectPolicy(id)
	if body.AutoAnalysisEnabled != nil {
		policy.AutoAnalysisEnabled = *body.AutoAnalysisEnabled
	}
	if body.DailyBudget != nil {
		if *body.DailyBudget < 0 || *body.DailyBudget > issueAIAutoDailyBudgetMax {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{
				"error": fmt.Sprintf("daily_budget must be between 0 and %d", issueAIAutoDailyBudgetMax),
			})
		}
		policy.DailyBudget = *body.DailyBudget
	}
	g.issueAI.svc.SetProjectPolicy(id, policy)
	return c.JSON(policy)
}

func (g *Gateway) getIssueAIAnalysis(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	row, err := g.issues.GetIssue(c.Context(), currentProject(c).ID, id)
	if err != nil {
		return apiError(c, err)
	}
	if row.AIAnalysisAt == nil || row.AIAnalysis == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "AI analysis not found"})
	}
	g.auditIssueAI(c, id, true, false)
	return c.JSON(issueAIResponse(row, true))
}

func (g *Gateway) postIssueAIAnalysis(c fiber.Ctx) error {
	state, ok := g.assistantReady(c)
	if !ok || !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	project := currentProject(c)
	refresh := c.Query("refresh") == "1"
	res, err := g.issueAI.svc.AnalyzeManual(
		c.Context(),
		issueai.ClientSnapshot{Enabled: state.enabled, Client: state.client},
		project,
		id,
		refresh,
	)
	if err != nil {
		if errors.Is(err, issueai.ErrRateLimited) {
			return c.Status(http.StatusTooManyRequests).
				JSON(fiber.Map{"error": "Issue AI analysis rate limit exceeded"})
		}
		var contractErr *issueai.ContractError
		if errors.As(err, &contractErr) {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": "AI output did not match the analysis contract", "raw_output": firstRunes(res.Raw, 500)})
		}
		if errors.Is(err, context.DeadlineExceeded) || ai.IsKind(err, ai.ErrorTimeout) {
			return c.Status(http.StatusGatewayTimeout).JSON(fiber.Map{"error": "AI issue analysis timed out"})
		}
		if errors.Is(err, context.Canceled) {
			return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI issue analysis cancelled"})
		}
		if meta.IsNotFound(err) {
			return apiError(c, err)
		}
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "AI issue analysis failed"})
	}
	g.auditIssueAI(c, id, res.Cached, refresh)
	return c.JSON(issueAIResponse(res.Issue, res.Cached))
}

func (g *Gateway) auditIssueAI(c fiber.Ctx, issueID int, cached, refresh bool) {
	user, _ := c.Locals(userLocal).(meta.UserInfo)
	project := currentProject(c)
	g.logger.Info(
		"issue AI analysis accessed",
		"user_id", user.ID,
		"username", user.Username,
		"project_id", project.ID,
		"project", project.Slug,
		"issue_id", issueID,
		"cached", cached,
		"refresh", refresh,
	)
}

func issueAIResponse(row meta.IssueView, cached bool) fiber.Map {
	response := fiber.Map{"cached": cached, "ai_analysis_at": row.AIAnalysisAt}
	for key, value := range row.AIAnalysis {
		response[key] = value
	}
	if row.AIAnalysisAt != nil && row.LastSeen.After(*row.AIAnalysisAt) {
		response["stale"] = true
	}
	return response
}

func parseIssueAIAnalysis(raw string) (issueAIAnalysis, error) {
	return issueai.ParseAnalysis(raw)
}

func fitContextSections(sections []contextSection, budget int) string {
	return issueai.FitContextSections(sections, budget)
}

func issueAIStacks(events []issueAIEvent) string {
	return issueai.FormatStacks(events)
}

func compactMetricResponse(body []byte, maxPoints int) any {
	return issueai.CompactMetricResponse(body, maxPoints)
}

func (g *Gateway) issueAILogs(ctx context.Context, project meta.ProjectInfo, traceID string, at time.Time) string {
	return g.issueAI.svc.TraceLogs(ctx, project, traceID, at)
}

func (g *Gateway) issueAITrace(ctx context.Context, project meta.ProjectInfo, traceID string) string {
	return g.issueAI.svc.TraceSummary(ctx, project, traceID)
}
