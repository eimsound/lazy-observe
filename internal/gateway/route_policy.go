package gateway

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	"github.com/gofiber/fiber/v3"
)

const routeMethodAll = "ALL"

type bearerKind uint8

const (
	bearerNone bearerKind = iota
	bearerUserToken
	bearerUploadToken
	bearerProjectToken
	bearerOther
)

type bearerCred struct {
	kind  bearerKind
	token string
}

// classifyBearer is the single entry point that parses and categorizes the
// HTTP Authorization Bearer header across all gateway routes.
func classifyBearer(c fiber.Ctx) bearerCred {
	token := bearerCredential(c)
	if token == "" {
		return bearerCred{kind: bearerNone}
	}
	switch {
	case strings.HasPrefix(token, "lou_"):
		return bearerCred{kind: bearerUserToken, token: token}
	case strings.HasPrefix(token, meta.SourceMapUploadTokenPrefix):
		return bearerCred{kind: bearerUploadToken, token: token}
	case strings.HasPrefix(token, "lo_"):
		return bearerCred{kind: bearerProjectToken, token: token}
	default:
		return bearerCred{kind: bearerOther, token: token}
	}
}

func bearerCredential(c fiber.Ctx) string {
	authorization := strings.TrimSpace(c.Get("Authorization"))
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return ""
	}
	return token
}

type authMode uint8

const (
	authPublic authMode = iota
	authProjectToken
	authUploadToken
	authMetricsScrapeToken
	authSessionOnly
	authUser
	authUserTokenOnly
)

func (m authMode) isUserBearing() bool {
	return m == authSessionOnly || m == authUser || m == authUserTokenOnly
}

func (m authMode) supportsSessionCookie() bool {
	return m == authSessionOnly || m == authUser
}

type routeRole uint8

const (
	roleAny routeRole = iota
	roleMember
	roleAdmin
)

// routePolicy declaratively expresses the orthogonal security attributes of
// one gateway route right at its registration point.
type routePolicy struct {
	auth                authMode
	role                routeRole
	csrf                bool
	projectScope        bool
	allowWhenMustChange bool
	browserIngestCORS   bool
	ingestSignal        selfmetrics.Signal
	hasIngestSignal     bool
}

func projectIngestPolicy(signal selfmetrics.Signal) routePolicy {
	return routePolicy{
		auth:            authProjectToken,
		ingestSignal:    signal,
		hasIngestSignal: true,
	}
}

func browserIngestPolicy(signal selfmetrics.Signal) routePolicy {
	return routePolicy{
		auth:              authProjectToken,
		browserIngestCORS: true,
		ingestSignal:      signal,
		hasIngestSignal:   true,
	}
}

type registeredRoute struct {
	method string
	path   string
	policy routePolicy
}

func isMutatingMethod(method string) bool {
	switch method {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
		return true
	default:
		return false
	}
}

func validateRoutePolicy(method, routePath string, p routePolicy) {
	if p.role != roleAny && !p.auth.isUserBearing() {
		panic(fmt.Sprintf("gateway: route %s %s requires a role without user authentication", method, routePath))
	}
	if p.csrf && !p.auth.supportsSessionCookie() {
		panic(fmt.Sprintf("gateway: route %s %s enables CSRF without session-capable auth", method, routePath))
	}
	if p.auth == authSessionOnly && isMutatingMethod(method) && !p.csrf {
		panic(fmt.Sprintf("gateway: mutating session-only route %s %s must enable CSRF", method, routePath))
	}
	if p.allowWhenMustChange && !p.auth.isUserBearing() {
		panic(fmt.Sprintf("gateway: route %s %s sets allowWhenMustChange without user auth", method, routePath))
	}
	if p.projectScope &&
		(p.auth == authPublic || p.auth == authProjectToken || p.auth == authUploadToken || p.auth == authMetricsScrapeToken) {
		panic(fmt.Sprintf("gateway: route %s %s sets projectScope with incompatible auth mode", method, routePath))
	}
	if p.browserIngestCORS && p.auth != authProjectToken {
		panic(
			fmt.Sprintf("gateway: route %s %s enables browserIngestCORS without project token auth", method, routePath),
		)
	}
	if p.auth == authProjectToken && !p.hasIngestSignal {
		panic(fmt.Sprintf("gateway: project-token ingest route %s %s must declare an ingestSignal", method, routePath))
	}
	if p.hasIngestSignal && p.auth != authProjectToken {
		panic(fmt.Sprintf("gateway: route %s %s declares ingestSignal without project token auth", method, routePath))
	}
}

func (g *Gateway) recordRoute(method, routePath string, policy routePolicy) {
	key := method + " " + routePath
	if g.registeredIndex == nil {
		g.registeredIndex = make(map[string]routePolicy)
	}
	if _, exists := g.registeredIndex[key]; exists {
		panic(fmt.Sprintf("gateway: duplicate route registration %s", key))
	}
	g.registeredIndex[key] = policy
	g.registeredRoutes = append(g.registeredRoutes, registeredRoute{
		method: method,
		path:   routePath,
		policy: policy,
	})
}

func (g *Gateway) route(method, routePath string, policy routePolicy, handlers ...fiber.Handler) {
	if len(handlers) == 0 {
		panic(fmt.Sprintf("gateway: route %s %s has no handlers", method, routePath))
	}
	validateRoutePolicy(method, routePath, policy)
	g.recordRoute(method, routePath, policy)

	if policy.browserIngestCORS {
		g.recordRoute(fiber.MethodOptions, routePath, routePolicy{auth: authPublic})
		g.app.Options(routePath, browserIngestCORS)
	}

	chain := g.buildRouteChain(routePath, policy, handlers)
	g.app.Add([]string{method}, routePath, chain[0], toAnyHandlers(chain[1:])...)
}

func (g *Gateway) routeAll(routePath string, policy routePolicy, handlers ...fiber.Handler) {
	if len(handlers) == 0 {
		panic(fmt.Sprintf("gateway: route ALL %s has no handlers", routePath))
	}
	validateRoutePolicy(routeMethodAll, routePath, policy)
	g.recordRoute(routeMethodAll, routePath, policy)

	chain := g.buildRouteChain(routePath, policy, handlers)
	g.app.All(routePath, chain[0], toAnyHandlers(chain[1:])...)
}

func (g *Gateway) buildRouteChain(routePath string, policy routePolicy, handlers []fiber.Handler) []fiber.Handler {
	surface := surfaceForRoute(routePath, policy)
	chain := make([]fiber.Handler, 0, len(handlers)+3)
	chain = append(chain, func(c fiber.Ctx) error {
		c.Locals(surfaceLocal, surface)
		return c.Next()
	})
	if policy.browserIngestCORS {
		chain = append(chain, browserIngestCORS)
	}
	if mw := g.policyMiddleware(policy); mw != nil {
		chain = append(chain, mw)
	}
	chain = append(chain, handlers...)
	return chain
}

func toAnyHandlers(handlers []fiber.Handler) []any {
	out := make([]any, len(handlers))
	for i, h := range handlers {
		out[i] = h
	}
	return out
}

// policyMiddleware compiles one declarative routePolicy into a single
// deterministic security pipeline:
//  1. Credential classification & authentication (+ must_change_password gate)
//  2. Double-submit CSRF enforcement (when authenticated via session cookie)
//  3. Whitelist RBAC role check (before project lookup to fail closed)
//  4. Project context resolution (X-LO-Project, fail-closed 404/503)
func (g *Gateway) policyMiddleware(policy routePolicy) fiber.Handler {
	if policy.auth == authPublic {
		return nil
	}
	return func(c fiber.Ctx) error {
		switch policy.auth {
		case authProjectToken:
			return g.authenticateAndLimitIngest(c, policy)
		case authUploadToken:
			return g.uploadAuth.UploadMiddleware(c)
		case authMetricsScrapeToken:
			return g.metricsScrapeTokenMiddleware(c)
		}

		user, authBySession, err := g.authenticateUser(c, policy)
		if err != nil {
			if errors.Is(err, errResponseWritten) {
				return nil
			}
			return err
		}
		if user.MustChangePassword && !policy.allowWhenMustChange {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"code":  string(account.CodePasswordChangeRequired),
				"error": "change the initial password before using the API",
			})
		}
		c.Locals(userLocal, user)

		if policy.csrf && authBySession {
			if !g.checkCSRF(c) {
				return nil
			}
		}

		if !roleSatisfies(user.Role, policy.role) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}

		if policy.projectScope {
			return g.resolveRequestProject(c)
		}
		return c.Next()
	}
}

func (g *Gateway) authenticateUser(c fiber.Ctx, policy routePolicy) (meta.UserInfo, bool, error) {
	switch policy.auth {
	case authSessionOnly:
		u, err := g.authenticateSessionCookie(c)
		return u, true, err

	case authUserTokenOnly:
		cred := classifyBearer(c)
		switch cred.kind {
		case bearerNone:
			return meta.UserInfo{}, false, unauthorized(c)
		case bearerUserToken:
			u, err := g.authenticateUserToken(c, cred.token)
			return u, false, err
		default:
			return meta.UserInfo{}, false, rejectNonQueryBearer(c, cred)
		}

	case authUser:
		cred := classifyBearer(c)
		switch cred.kind {
		case bearerUserToken:
			u, err := g.authenticateUserToken(c, cred.token)
			return u, false, err
		case bearerNone:
			u, err := g.authenticateSessionCookie(c)
			return u, true, err
		default:
			return meta.UserInfo{}, false, rejectNonQueryBearer(c, cred)
		}

	default:
		return meta.UserInfo{}, false, unauthorized(c)
	}
}

func (g *Gateway) authenticateUserToken(c fiber.Ctx, token string) (meta.UserInfo, error) {
	u, err := g.account.AuthenticateUserToken(c.Context(), token)
	if err != nil {
		if account.Code(err) == account.CodeInvalidCredentials {
			return meta.UserInfo{}, unauthorized(c)
		}
		_ = c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "authentication temporarily unavailable"})
		return meta.UserInfo{}, errResponseWritten
	}
	return accountUserInfo(u), nil
}

func (g *Gateway) authenticateSessionCookie(c fiber.Ctx) (meta.UserInfo, error) {
	token := strings.TrimSpace(c.Cookies("lo_session"))
	if token == "" {
		return meta.UserInfo{}, unauthorized(c)
	}
	u, err := g.account.AuthenticateSession(c.Context(), token)
	if err != nil {
		if account.Code(err) == account.CodeUnavailable {
			_ = c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "authentication temporarily unavailable"})
			return meta.UserInfo{}, errResponseWritten
		}
		return meta.UserInfo{}, unauthorized(c)
	}
	return accountUserInfo(u), nil
}

func (g *Gateway) checkCSRF(c fiber.Ctx) bool {
	if !g.requireCSRF {
		return true
	}
	switch c.Method() {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	expected := c.Cookies(csrfCookieName)
	actual := strings.TrimSpace(c.Get(csrfHeaderName))
	if expected == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		_ = c.Status(fiber.StatusForbidden).
			JSON(fiber.Map{"code": "csrf_failed", "error": "CSRF token missing or invalid"})
		return false
	}
	return true
}

func roleSatisfies(actual string, required routeRole) bool {
	switch required {
	case roleAny:
		return true
	case roleMember:
		return actual == "admin" || actual == "member"
	case roleAdmin:
		return actual == "admin"
	default:
		return false
	}
}

func rejectNonQueryBearer(c fiber.Ctx, cred bearerCred) error {
	if cred.kind == bearerUploadToken {
		_ = uploadOnlyToken(c)
		return errResponseWritten
	}
	_ = ingestOnlyToken(c)
	return errResponseWritten
}

// ingestOnlyToken rejects a project API token on a read route. Project tokens
// are embedded in browser pages and SDK configuration, so they are ingest-only.
func ingestOnlyToken(c fiber.Ctx) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  "ingest_only_token",
		"error": "project API tokens can only ingest telemetry; use a session or a user API token to query",
	})
}

// uploadOnlyToken rejects a source map upload token anywhere but the upload route
// (ADR 0018). It is confidential but narrowly scoped: it can neither ingest nor read.
func uploadOnlyToken(c fiber.Ctx) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  "upload_only_token",
		"error": "source map upload tokens can only upload source maps to their project",
	})
}

// sourceMapUploadTokenRequired rejects a credential that is not an upload token on
// the upload route: the project API token is public (ADR 0010) and must not be
// able to overwrite a release's source maps (ADR 0018).
func sourceMapUploadTokenRequired(c fiber.Ctx) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  "source_map_upload_token_required",
		"error": "source map upload requires a source map upload token (los_...); create one in the project settings",
	})
}

func requestedProjectSlug(c fiber.Ctx) string {
	slug := strings.TrimSpace(c.Get("X-LO-Project"))
	if slug == "" {
		slug = "default"
	}
	return slug
}

func (g *Gateway) resolveRequestProject(c fiber.Ctx) error {
	if g.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "project store unavailable"})
	}
	slug := requestedProjectSlug(c)
	p, err := g.store.ProjectBySlug(c.Context(), slug)
	if err != nil {
		if meta.IsNotFound(err) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"code":  "project_not_found",
				"error": "project not found",
			})
		}
		g.logger.Warn("project lookup failed", "project_slug", slug, "error", err)
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "project lookup temporarily unavailable"})
	}
	if p.ID <= 0 {
		return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
	}
	c.Locals(projectLocal, p)
	return c.Next()
}
