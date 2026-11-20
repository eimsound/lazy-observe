package gateway

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	sessionTTL            = 7 * 24 * time.Hour
	maxLoginBodyBytes     = 8 << 10
	loginAuditNoiseWindow = time.Minute
	loginAuditNoisePerIP  = 1
)

type projectResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

type tokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Revoked    bool       `json:"revoked"`
}

type issuedTokenResponse struct {
	tokenResponse
	Value string `json:"value"`
}

type projectAlertConfigRequest struct {
	NewIssue                    bool    `json:"new_issue"`
	Regression                  bool    `json:"regression"`
	ErrorRateThresholdPerMinute int     `json:"error_rate_threshold_per_min"`
	ChannelIDs                  []apiID `json:"channel_ids"`
}

func (request projectAlertConfigRequest) input() alert.ProjectAlertConfigInput {
	channelIDs := make([]int, 0, len(request.ChannelIDs))
	for _, id := range request.ChannelIDs {
		channelIDs = append(channelIDs, int(id))
	}
	return alert.ProjectAlertConfigInput{
		NewIssue: request.NewIssue, Regression: request.Regression,
		ErrorRateThresholdPerMinute: request.ErrorRateThresholdPerMinute, ChannelIDs: channelIDs,
	}
}

type projectAlertConfigResponse struct {
	ProjectID                   int      `json:"project_id"`
	NewIssue                    bool     `json:"new_issue"`
	Regression                  bool     `json:"regression"`
	ErrorRateThresholdPerMinute int      `json:"error_rate_threshold_per_min"`
	ChannelIDs                  []string `json:"channel_ids"`
}

func projectAlertConfigJSON(config alert.ProjectAlertConfigView) projectAlertConfigResponse {
	numericChannelIDs := append([]int(nil), config.ChannelIDs...)
	sort.Ints(numericChannelIDs)
	channelIDs := make([]string, 0, len(numericChannelIDs))
	for _, id := range numericChannelIDs {
		channelIDs = append(channelIDs, strconv.Itoa(id))
	}
	return projectAlertConfigResponse{
		ProjectID: config.ProjectID, NewIssue: config.NewIssue, Regression: config.Regression,
		ErrorRateThresholdPerMinute: config.ErrorRateThresholdPerMinute, ChannelIDs: channelIDs,
	}
}

func projectJSON(project management.ProjectView) projectResponse {
	return projectResponse{
		ID:        strconv.Itoa(project.ID),
		Name:      project.Name,
		Slug:      project.Slug,
		CreatedAt: project.CreatedAt,
	}
}

func tokenJSON(token management.ProjectTokenView) tokenResponse {
	return tokenResponse{
		ID:         strconv.Itoa(token.ID),
		Name:       token.Name,
		Prefix:     token.Prefix,
		CreatedAt:  token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
		Revoked:    token.Revoked,
	}
}

func userAPITokenJSON(token account.Token) tokenResponse {
	return tokenResponse{
		ID:         strconv.Itoa(token.ID),
		Name:       token.Name,
		Prefix:     token.Prefix,
		CreatedAt:  token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
		Revoked:    token.Revoked,
	}
}

func (g *Gateway) login(c fiber.Ctx) error {
	if len(requestBody(c)) > maxLoginBodyBytes {
		return c.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"code": "invalid_request", "error": "invalid login request"})
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	_ = c.Bind().Body(&body)
	result, err := g.account.Login(c.Context(), account.LoginInput{
		Username: body.Username, Password: body.Password, SourceIP: c.IP(),
	})
	if err != nil {
		if g.shouldAuditLoginFailure(c, account.Code(err)) {
			g.auditAccountFacts(c, account.Facts(err))
		}
		return g.loginError(c, err)
	}
	if g.requireCSRF {
		csrf, csrfErr := newCSRFToken()
		if csrfErr != nil {
			_, _ = g.account.Logout(c.Context(), result.Token, result.User)
			return g.loginServiceUnavailable(c, "csrf_token", csrfErr)
		}
		c.Cookie(g.buildCSRFCookie(csrf, result.ExpiresAt))
	}
	c.Cookie(g.buildSessionCookie(result.Token, result.ExpiresAt))
	g.auditAccountFacts(c, result.Facts)
	u := result.User
	return c.JSON(fiber.Map{
		"username": u.Username, "role": u.Role, "display_name": u.DisplayName, "email": u.Email,
		"must_change_password": u.MustChangePassword,
	})
}

func (g *Gateway) shouldAuditLoginFailure(c fiber.Ctx, code account.ErrorCode) bool {
	switch code {
	case account.CodeRateLimited, account.CodeInvalidRequest:
		if g.limits == nil {
			return false
		}
		allowed, err := g.limits.Allow(
			c.Context(),
			"audit:login:"+string(code)+":"+c.IP(),
			loginAuditNoisePerIP,
			loginAuditNoiseWindow,
		)
		return err == nil && allowed
	default:
		return true
	}
}

func (g *Gateway) loginServiceUnavailable(c fiber.Ctx, stage string, err error) error {
	if retryAfter := account.RetryAfter(err); retryAfter > 0 {
		seconds := int(retryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(seconds))
	}
	if err != nil {
		g.logger.Error("authentication service unavailable", "stage", stage, "error", err)
	} else {
		g.logger.Error("authentication service unavailable", "stage", stage)
	}
	return c.Status(fiber.StatusServiceUnavailable).
		JSON(fiber.Map{"code": "service_unavailable", "error": "authentication service unavailable"})
}

func (g *Gateway) loginError(c fiber.Ctx, err error) error {
	switch account.Code(err) {
	case account.CodeInvalidRequest:
		return c.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"code": "invalid_request", "error": "invalid login request"})
	case account.CodeInvalidCredentials:
		return c.Status(fiber.StatusUnauthorized).
			JSON(fiber.Map{"code": "invalid_credentials", "error": "invalid username or password"})
	case account.CodeRateLimited:
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(int(account.RetryAfter(err).Seconds())))
		return c.Status(fiber.StatusTooManyRequests).
			JSON(fiber.Map{"code": "rate_limited", "error": "too many login attempts"})
	case account.CodeLockedOut:
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(int(account.RetryAfter(err).Seconds())))
		return c.Status(fiber.StatusTooManyRequests).
			JSON(fiber.Map{"code": "locked_out", "error": "account is temporarily locked"})
	default:
		return g.loginServiceUnavailable(c, account.Stage(err), err)
	}
}

func (g *Gateway) me(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	full, err := g.account.CurrentUser(c.Context(), u.ID)
	if err != nil {
		return accountAPIError(c, err)
	}
	return c.JSON(fiber.Map{
		"id": full.ID, "username": full.Username, "role": full.Role,
		"display_name": full.DisplayName, "email": full.Email, "last_login_at": full.LastLoginAt,
		"must_change_password": full.MustChangePassword,
	})
}

func (g *Gateway) logout(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	facts, err := g.account.Logout(c.Context(), c.Cookies("lo_session"), accountUser(u))
	if err != nil {
		// The server-side session still exists (ADR 0006): report the failure
		// and keep the cookie so the client can retry the logout.
		g.auditAccountFacts(c, facts)
		return accountAPIError(c, err)
	}
	expired := g.now().Add(-time.Hour)
	c.Cookie(g.buildSessionCookie("", expired))
	if g.requireCSRF {
		c.Cookie(g.buildCSRFCookie("", expired))
	}
	g.auditAccountFacts(c, facts)
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listCurrentUserTokens(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	tokens, err := g.account.ListUserTokens(c.Context(), u.ID)
	if err != nil {
		return accountAPIError(c, err)
	}
	response := make([]tokenResponse, 0, len(tokens))
	for _, token := range tokens {
		response = append(response, userAPITokenJSON(token))
	}
	return c.JSON(response)
}

func (g *Gateway) createCurrentUserToken(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	var body struct {
		Name string `json:"name"`
	}
	if c.Bind().Body(&body) != nil || strings.TrimSpace(body.Name) == "" {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "user_api_token.create",
				Actor:         u,
				TargetType:    "user_api_token",
				TargetName:    strings.TrimSpace(body.Name),
				Success:       false,
				FailureReason: "invalid_request",
			},
		)
		return badRequest(c)
	}
	name := strings.TrimSpace(body.Name)
	issued, err := g.account.IssueUserToken(c.Context(), accountUser(u), name)
	if err != nil {
		g.auditAccountFacts(c, account.Facts(err))
		return accountAPIError(c, err)
	}
	g.auditAccountFacts(c, issued.Facts)
	return c.Status(fiber.StatusCreated).
		JSON(issuedTokenResponse{tokenResponse: userAPITokenJSON(issued.Token), Value: issued.Value})
}

func (g *Gateway) revokeCurrentUserToken(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	tokenID, ok := pathID(c, "tokenID")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "user_api_token.revoke",
				Actor:         u,
				TargetType:    "user_api_token",
				Success:       false,
				FailureReason: "invalid_target_id",
			},
		)
		return nil
	}
	facts, err := g.account.RevokeUserToken(c.Context(), accountUser(u), tokenID)
	g.auditAccountFacts(c, facts)
	if err != nil {
		return accountAPIError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listProjects(c fiber.Ctx) error {
	projects, err := g.management.ListProjects(c.Context())
	if err != nil {
		return managementAPIError(c, err)
	}
	response := make([]projectResponse, 0, len(projects))
	for _, project := range projects {
		response = append(response, projectJSON(project))
	}
	return c.JSON(response)
}

func (g *Gateway) getProject(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	p, err := g.management.GetProject(c.Context(), id)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.JSON(projectJSON(p))
}

func (g *Gateway) getProjectAlertConfig(c fiber.Ctx) error {
	if g.alerts == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "alert service unavailable"})
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	config, err := g.alerts.GetProjectAlertConfig(c.Context(), id)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(projectAlertConfigJSON(config))
}

func (g *Gateway) putProjectAlertConfig(c fiber.Ctx) error {
	if g.alerts == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "alert service unavailable"})
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	var body projectAlertConfigRequest
	if c.Bind().Body(&body) != nil || body.ErrorRateThresholdPerMinute < 0 {
		return badRequest(c)
	}
	config, err := g.alerts.PutProjectAlertConfig(c.Context(), id, body.input())
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(projectAlertConfigJSON(config))
}

func (g *Gateway) createProject(c fiber.Ctx) error {
	var body struct{ Name, Slug string }
	if c.Bind().Body(&body) != nil {
		g.auditManagementFacts(c, management.AuditFacts{
			Action: "project.create", TargetType: "project", Success: false, FailureReason: "invalid_request",
		})
		return badRequest(c)
	}
	p, facts, err := g.management.CreateProject(
		c.Context(),
		management.CreateProjectInput{Name: body.Name, Slug: body.Slug},
	)
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(projectJSON(p))
}

func (g *Gateway) updateProject(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "project.update",
				TargetType:    "project",
				Success:       false,
				FailureReason: "invalid_target_id",
			},
		)
		return nil
	}
	var body struct{ Name, Slug string }
	invalid := c.Bind().Body(&body) != nil
	p, facts, err := g.management.UpdateProject(c.Context(), management.UpdateProjectInput{
		ID: id, Name: body.Name, Slug: body.Slug, Invalid: invalid,
	})
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.JSON(projectJSON(p))
}

func (g *Gateway) deleteProject(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "project.delete",
				TargetType:    "project",
				Success:       false,
				FailureReason: "invalid_target_id",
			},
		)
		return nil
	}
	facts, err := g.management.DeleteProject(c.Context(), id)
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listProjectAPITokens(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	tokens, err := g.management.ListProjectTokens(c.Context(), id)
	if err != nil {
		return managementAPIError(c, err)
	}
	response := make([]tokenResponse, 0, len(tokens))
	for _, token := range tokens {
		response = append(response, tokenJSON(token))
	}
	return c.JSON(response)
}

func (g *Gateway) createProjectAPIToken(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "project_api_token.create",
				TargetType:    "project_api_token",
				Success:       false,
				FailureReason: "invalid_project_id",
			},
		)
		return nil
	}
	var body struct {
		Name string `json:"name"`
	}
	invalid := len(requestBody(c)) != 0 && c.Bind().Body(&body) != nil
	issued, facts, err := g.management.IssueProjectToken(c.Context(), management.IssueProjectTokenInput{
		ProjectID: id, Name: body.Name, Invalid: invalid,
	})
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(issuedTokenResponse{
		tokenResponse: tokenJSON(issued.Token),
		Value:         issued.Value,
	})
}

func (g *Gateway) revokeProjectAPIToken(c fiber.Ctx) error {
	projectID, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "project_api_token.revoke",
				TargetType:    "project_api_token",
				Success:       false,
				FailureReason: "invalid_project_id",
			},
		)
		return nil
	}
	tokenID, err := strconv.Atoi(c.Params("tokenID"))
	if err != nil || tokenID <= 0 {
		tokenID = 0
	}
	facts, err := g.management.RevokeProjectToken(c.Context(), projectID, tokenID)
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listSourceMapUploadTokens(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	tokens, err := g.management.ListSourceMapUploadTokens(c.Context(), id)
	if err != nil {
		return managementAPIError(c, err)
	}
	response := make([]tokenResponse, 0, len(tokens))
	for _, token := range tokens {
		response = append(response, tokenJSON(token))
	}
	return c.JSON(response)
}

func (g *Gateway) createSourceMapUploadToken(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "source_map_upload_token.create",
				TargetType:    "source_map_upload_token",
				Success:       false,
				FailureReason: "invalid_project_id",
			},
		)
		return nil
	}
	var body struct {
		Name string `json:"name"`
	}
	invalid := len(requestBody(c)) != 0 && c.Bind().Body(&body) != nil
	issued, facts, err := g.management.IssueSourceMapUploadToken(
		c.Context(),
		management.IssueSourceMapUploadTokenInput{ProjectID: id, Name: body.Name, Invalid: invalid},
	)
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(issuedTokenResponse{
		tokenResponse: tokenJSON(issued.Token),
		Value:         issued.Value,
	})
}

func (g *Gateway) revokeSourceMapUploadToken(c fiber.Ctx) error {
	projectID, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{
				Action:        "source_map_upload_token.revoke",
				TargetType:    "source_map_upload_token",
				Success:       false,
				FailureReason: "invalid_project_id",
			},
		)
		return nil
	}
	tokenID, err := strconv.Atoi(c.Params("tokenID"))
	if err != nil || tokenID <= 0 {
		tokenID = 0
	}
	facts, err := g.management.RevokeSourceMapUploadToken(c.Context(), projectID, tokenID)
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) listUsers(c fiber.Ctx) error {
	users, err := g.management.ListUsers(c.Context())
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.JSON(users)
}

func (g *Gateway) getUser(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	u, err := g.management.GetUser(c.Context(), id)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.JSON(u)
}

func (g *Gateway) createUser(c fiber.Ctx) error {
	var body struct{ Username, Password, Role, DisplayName, Email string }
	if c.Bind().Body(&body) != nil {
		g.auditManagementFacts(c, management.AuditFacts{
			Action: "user.create", TargetType: "user", Success: false, FailureReason: "invalid_request",
		})
		return badRequest(c)
	}
	u, facts, err := g.management.CreateUser(c.Context(), management.CreateUserInput{
		Username:    body.Username,
		Password:    body.Password,
		Role:        body.Role,
		DisplayName: body.DisplayName,
		Email:       body.Email,
	})
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(u)
}

func (g *Gateway) updateUser(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{Action: "user.update", TargetType: "user", Success: false, FailureReason: "invalid_target_id"},
		)
		return nil
	}
	var body struct {
		Username    string  `json:"username"`
		Password    string  `json:"password"`
		Role        string  `json:"role"`
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
	}
	if c.Bind().Body(&body) != nil {
		g.auditManagementFacts(c, management.AuditFacts{
			Action: "user.update", TargetType: "user", TargetID: id, Success: false, FailureReason: "invalid_request",
		})
		return badRequest(c)
	}
	u, facts, err := g.management.UpdateUser(c.Context(), management.UpdateUserInput{
		ID: id, Username: body.Username, Password: body.Password, Role: body.Role,
		DisplayName: body.DisplayName, Email: body.Email,
	})
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.JSON(u)
}

func (g *Gateway) deleteUser(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		g.auditManagement(
			c,
			auditEvent{Action: "user.delete", TargetType: "user", Success: false, FailureReason: "invalid_target_id"},
		)
		return nil
	}
	facts, err := g.management.DeleteUser(c.Context(), id)
	g.auditManagementFacts(c, facts)
	if err != nil {
		return managementAPIError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func pathID(c fiber.Ctx, name string) (int, bool) {
	id, err := strconv.Atoi(c.Params(name))
	if err != nil || id <= 0 {
		_ = badRequest(c)
		return 0, false
	}
	return id, true
}

func badRequest(c fiber.Ctx) error {
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad request"})
}

func apiError(c fiber.Ctx, err error) error {
	if meta.IsNotFound(err) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if meta.IsConstraintError(err) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "conflict"})
	}
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func managementAPIError(c fiber.Ctx, err error) error {
	switch {
	case management.IsErrorKind(err, management.ErrorInvalidInput):
		if err.Error() == "bad request" {
			return badRequest(c)
		}
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case management.IsErrorKind(err, management.ErrorNotFound):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	case management.IsErrorKind(err, management.ErrorConflict):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	case management.IsErrorKind(err, management.ErrorProtected):
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	default:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
}
