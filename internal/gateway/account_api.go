package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v2"
)

const (
	csrfCookieName = "lo_csrf"
	csrfHeaderName = "X-CSRF-Token"
)

func (g *Gateway) buildSessionCookie(token string, expires time.Time) *fiber.Cookie {
	maxAge := int(sessionTTL.Seconds())
	if token == "" {
		maxAge = -1
	}
	return &fiber.Cookie{
		Name: "lo_session", Value: token, Path: "/", HTTPOnly: true,
		Secure: g.secureSessionCookie, SameSite: fiber.CookieSameSiteLaxMode,
		Expires: expires, MaxAge: maxAge,
	}
}

func (g *Gateway) buildCSRFCookie(value string, expires time.Time) *fiber.Cookie {
	maxAge := int(sessionTTL.Seconds())
	if value == "" {
		maxAge = -1
	}
	return &fiber.Cookie{
		Name: csrfCookieName, Value: value, Path: "/", HTTPOnly: false,
		Secure: g.secureSessionCookie, SameSite: fiber.CookieSameSiteLaxMode,
		Expires: expires, MaxAge: maxAge,
	}
}

func newCSRFToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// csrfRequired enforces the double-submit cookie check for mutating
// session-authenticated requests. Bearer-token requests and read-only
// methods are exempt; the token never leaves the browser except through the
// X-CSRF-Token header.
func (g *Gateway) csrfRequired(c *fiber.Ctx) error {
	if !g.requireCSRF {
		return c.Next()
	}
	switch c.Method() {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return c.Next()
	}
	if c.Cookies("lo_session") == "" {
		return c.Next()
	}
	expected := c.Cookies(csrfCookieName)
	actual := strings.TrimSpace(c.Get(csrfHeaderName))
	if expected == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return c.Status(fiber.StatusForbidden).
			JSON(fiber.Map{"code": "csrf_failed", "error": "CSRF token missing or invalid"})
	}
	return c.Next()
}

func (g *Gateway) changePassword(c *fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	_ = c.BodyParser(&body)
	facts, err := g.account.ChangePassword(
		c.UserContext(), accountUser(u), body.CurrentPassword, body.NewPassword, c.Cookies("lo_session"),
	)
	g.auditAccountFacts(c, facts)
	if err != nil {
		return accountAPIError(c, err)
	}
	g.removeInitialAdminPasswordFile(u.Username)
	return c.SendStatus(fiber.StatusNoContent)
}

// removeInitialAdminPasswordFile deletes the bootstrap-generated first-admin
// password file after the admin replaced that password: the plaintext is dead
// and should not stay readable on disk. The field is empty when
// security.admin_password_file names that same file (by path or identity), so a
// deployer-owned file is never removed. An already absent file is a normal
// repeat password change and stays silent; any other removal failure is logged
// without failing the change.
func (g *Gateway) removeInitialAdminPasswordFile(username string) {
	path := g.initialAdminPasswordFile
	if path == "" || username != meta.BootstrapAdminUsername {
		return
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		g.logger.Warn(
			"could not remove initial admin password file",
			"path", path,
			"username", username,
			"err", err,
		)
	}
}

func (g *Gateway) updateMe(c *fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	var body struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
	}
	_ = c.BodyParser(&body)
	updated, facts, err := g.account.UpdateProfile(c.UserContext(), accountUser(u), account.ProfileInput{
		DisplayName: body.DisplayName, Email: body.Email,
	})
	g.auditAccountFacts(c, facts)
	if err != nil {
		return accountAPIError(c, err)
	}
	return c.JSON(updated)
}

func (g *Gateway) listMySessions(c *fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	token := c.Cookies("lo_session")
	sessions, err := g.account.ListSessions(c.UserContext(), u.ID, token)
	if err != nil {
		return accountAPIError(c, err)
	}
	return c.JSON(sessions)
}

func (g *Gateway) deleteMySession(c *fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	id := c.Params("id")
	if strings.TrimSpace(id) == "" {
		return badRequest(c)
	}
	facts, err := g.account.RevokeSession(c.UserContext(), accountUser(u), id)
	g.auditAccountFacts(c, facts)
	if err != nil {
		return accountAPIError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func accountAPIError(c *fiber.Ctx, err error) error {
	switch account.Code(err) {
	case account.CodeInvalidRequest:
		return badRequest(c)
	case account.CodePasswordTooShort:
		return c.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"code": "password_too_short", "error": "new password must be at least 8 characters"})
	case account.CodeInvalidCurrentPassword:
		return c.Status(fiber.StatusUnauthorized).
			JSON(fiber.Map{"code": "invalid_current_password", "error": "current password is incorrect"})
	case account.CodeInvalidEmail:
		return c.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"code": "invalid_email", "error": "email address is invalid"})
	case account.CodeNotFound:
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	case account.CodeConflict:
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "conflict"})
	case account.CodeUnavailable:
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"code": "service_unavailable", "error": "account service unavailable"})
	default:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
}

func (g *Gateway) listAudit(c *fiber.Ctx) error {
	options := meta.AuditListOptions{
		Action:     strings.TrimSpace(c.Query("action")),
		Actor:      strings.TrimSpace(c.Query("actor")),
		TargetType: strings.TrimSpace(c.Query("target_type")),
		BeforeID:   intQuery(c, "before_id"),
		Limit:      intQuery(c, "limit"),
	}
	if projectID := c.Query("project_id"); projectID != "" {
		id, ok := parsePositiveInt(projectID)
		if !ok {
			return badRequest(c)
		}
		options.ProjectID = id
	}
	events, err := g.store.ListAudit(c.UserContext(), options)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(events)
}

func intQuery(c *fiber.Ctx, name string) int {
	value, err := strconv.Atoi(strings.TrimSpace(c.Query(name)))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func parsePositiveInt(value string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	return id, err == nil && id > 0
}
