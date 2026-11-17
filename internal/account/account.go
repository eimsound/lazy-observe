package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
)

const (
	sessionTTL            = 7 * 24 * time.Hour
	loginAttemptLimit     = 10
	loginAttemptWindow    = time.Minute
	loginLockoutThreshold = 5
	loginLockoutWindow    = 15 * time.Minute
	loginLockoutFor       = 15 * time.Minute
	minPasswordLength     = 8
	loginFailureMinDelay  = 500 * time.Millisecond
	loginFailureMaxDelay  = time.Second
)

type ErrorCode string

const (
	CodeInvalidCredentials     ErrorCode = "invalid_credentials"
	CodeRateLimited            ErrorCode = "rate_limited"
	CodeLockedOut              ErrorCode = "locked_out"
	CodeUnavailable            ErrorCode = "service_unavailable"
	CodeNotFound               ErrorCode = "not_found"
	CodeConflict               ErrorCode = "conflict"
	CodeInvalidRequest         ErrorCode = "invalid_request"
	CodePasswordTooShort       ErrorCode = "password_too_short"
	CodeInvalidCurrentPassword ErrorCode = "invalid_current_password"
	CodeInvalidEmail           ErrorCode = "invalid_email"
	CodePasswordChangeRequired ErrorCode = "password_change_required"
)

type Error struct {
	Code       ErrorCode
	RetryAfter time.Duration
	Stage      string
	Facts      AuditFacts
	Cause      error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return string(e.Code) + ": " + e.Cause.Error()
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

func Code(err error) ErrorCode {
	var accountErr *Error
	if errors.As(err, &accountErr) {
		return accountErr.Code
	}
	return CodeUnavailable
}

func Facts(err error) AuditFacts {
	var accountErr *Error
	if errors.As(err, &accountErr) {
		return accountErr.Facts
	}
	return AuditFacts{Success: false, FailureReason: "internal_error"}
}

func RetryAfter(err error) time.Duration {
	var accountErr *Error
	if errors.As(err, &accountErr) {
		return accountErr.RetryAfter
	}
	return 0
}

func Stage(err error) string {
	var accountErr *Error
	if errors.As(err, &accountErr) {
		return accountErr.Stage
	}
	return "account"
}

type User struct {
	ID          int        `json:"id"`
	Username    string     `json:"username"`
	Role        string     `json:"role"`
	DisplayName string     `json:"display_name,omitempty"`
	Email       string     `json:"email,omitempty"`
	CreatedAt   time.Time  `json:"created_at,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// MustChangePassword limits the user to reading the current user, changing
	// the own password, and logout until the password is changed.
	MustChangePassword bool `json:"must_change_password"`
}

type Token struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Revoked    bool       `json:"revoked"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type IssuedToken struct {
	Token Token
	Value string
	Facts AuditFacts
}

type AuditFacts struct {
	Action        string
	ActorUsername string
	TargetType    string
	TargetID      int
	TargetName    string
	TargetRole    string
	ChangedFields []string
	Success       bool
	FailureReason string
}

type LoginInput struct {
	Username string
	Password string
	SourceIP string
}

type LoginResult struct {
	Token     string
	ExpiresAt time.Time
	User      User
	Facts     AuditFacts
}

type ProfileInput struct {
	DisplayName *string
	Email       *string
}

type Metadata interface {
	AuthenticateCredentials(context.Context, string, string) (meta.UserInfo, error)
	MarkLogin(context.Context, int) error
	GetUser(context.Context, int) (meta.UserInfo, error)
	ChangePassword(context.Context, int, string, string) error
	UpdateUserProfile(context.Context, int, *string, *string) (meta.UserInfo, error)
	AuthenticateUserAPIToken(context.Context, string) (meta.UserInfo, error)
	ListUserAPITokens(context.Context, int) ([]meta.UserAPITokenInfo, error)
	CreateUserAPIToken(context.Context, int, string) (meta.UserAPITokenInfo, string, error)
	RevokeUserAPITokenByID(context.Context, int, int) error
}

type Behavior interface {
	Login(context.Context, LoginInput) (LoginResult, error)
	AuthenticateSession(context.Context, string) (User, error)
	AuthenticateUserToken(context.Context, string) (User, error)
	Logout(context.Context, string, User) (AuditFacts, error)
	CurrentUser(context.Context, int) (User, error)
	ChangePassword(context.Context, User, string, string, string) (AuditFacts, error)
	UpdateProfile(context.Context, User, ProfileInput) (User, AuditFacts, error)
	ListSessions(context.Context, int, string) ([]SessionRef, error)
	RevokeSession(context.Context, User, string) (AuditFacts, error)
	ListUserTokens(context.Context, int) ([]Token, error)
	IssueUserToken(context.Context, User, string) (IssuedToken, error)
	RevokeUserToken(context.Context, User, int) (AuditFacts, error)
}

type Options struct {
	Now               func() time.Time
	LoginFailureDelay func() time.Duration
	Limits            state.Limiter
	LoginGate         state.LoginGate
}

type Manager struct {
	metadata          Metadata
	sessions          Sessions
	now               func() time.Time
	loginFailureDelay func() time.Duration
	limits            state.Limiter
	loginGate         state.LoginGate
}

func New(metadata Metadata, sessions Sessions, options Options) *Manager {
	if metadata == nil || sessions == nil {
		panic("account: metadata and sessions are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.LoginFailureDelay == nil {
		options.LoginFailureDelay = randomLoginFailureDelay
	}
	if options.Limits == nil {
		options.Limits = state.NewMemoryLimiterWithClock(options.Now)
	}
	if options.LoginGate == nil {
		options.LoginGate = state.NewMemoryLoginGateWithClock(options.Now)
	}
	return &Manager{
		metadata: metadata, sessions: sessions, now: options.Now, loginFailureDelay: options.LoginFailureDelay,
		limits: options.Limits, loginGate: options.LoginGate,
	}
}

func (m *Manager) Login(ctx context.Context, input LoginInput) (LoginResult, error) {
	username := strings.TrimSpace(input.Username)
	facts := AuditFacts{
		Action: "auth.login", ActorUsername: username, TargetType: "user", TargetName: username, Success: false,
	}
	allowed, err := m.limits.Allow(ctx, "login:ip:"+input.SourceIP, loginAttemptLimit, loginAttemptWindow)
	if err != nil {
		return LoginResult{}, unavailable("ip_rate_limit", facts, err)
	}
	if !allowed {
		facts.FailureReason = "rate_limited"
		return LoginResult{}, &Error{Code: CodeRateLimited, RetryAfter: time.Minute, Facts: facts}
	}
	if username == "" || input.Password == "" {
		return LoginResult{}, m.invalidLogin(ctx, username, facts)
	}
	accountKey := loginAccountLimitKey(username)
	locked, err := m.loginGate.Locked(ctx, accountKey)
	if err != nil {
		return LoginResult{}, unavailable("account_lockout", facts, err)
	}
	if locked {
		facts.FailureReason = "locked_out"
		return LoginResult{}, &Error{Code: CodeLockedOut, RetryAfter: loginLockoutFor, Facts: facts}
	}
	allowed, err = m.limits.Allow(ctx, accountKey, loginAttemptLimit, loginAttemptWindow)
	if err != nil {
		return LoginResult{}, unavailable("account_rate_limit", facts, err)
	}
	if !allowed {
		facts.FailureReason = "rate_limited"
		return LoginResult{}, &Error{Code: CodeRateLimited, RetryAfter: time.Minute, Facts: facts}
	}
	stored, err := m.metadata.AuthenticateCredentials(ctx, username, input.Password)
	if err != nil {
		if meta.IsInvalidCredentials(err) {
			return LoginResult{}, m.invalidLogin(ctx, username, facts)
		}
		return LoginResult{}, unavailable("credentials", facts, err)
	}
	now := m.now()
	token, err := m.sessions.Create(ctx, stored.ID, now, now.Add(sessionTTL))
	if err != nil {
		return LoginResult{}, unavailable("session_create", facts, err)
	}
	if err := m.metadata.MarkLogin(ctx, stored.ID); err != nil {
		cleanupErr := m.sessions.Delete(ctx, token)
		return LoginResult{}, unavailable("login_audit", facts, errors.Join(err, cleanupErr))
	}
	if err := m.loginGate.Clear(ctx, accountKey); err != nil {
		_ = m.sessions.Delete(ctx, token)
		return LoginResult{}, unavailable("account_lockout_clear", facts, err)
	}
	stored.LastLoginAt = timePointer(now.UTC())
	user := userFromMeta(stored)
	return LoginResult{
		Token: token, ExpiresAt: now.Add(sessionTTL), User: user,
		Facts: AuditFacts{
			Action: "auth.login", ActorUsername: user.Username, TargetType: "user", TargetID: user.ID,
			TargetName: user.Username, TargetRole: user.Role, Success: true,
		},
	}, nil
}

func (m *Manager) invalidLogin(ctx context.Context, username string, facts AuditFacts) error {
	if username != "" {
		_, _ = m.loginGate.ConsumeFailure(
			ctx, loginAccountLimitKey(username), loginLockoutThreshold, loginLockoutWindow, loginLockoutFor,
		)
	}
	if delay := m.loginFailureDelay(); delay > 0 {
		time.Sleep(delay)
	}
	facts.FailureReason = "invalid_credentials"
	return &Error{Code: CodeInvalidCredentials, Facts: facts}
}

func (m *Manager) AuthenticateSession(ctx context.Context, token string) (User, error) {
	userID, err := m.sessions.Authenticate(ctx, strings.TrimSpace(token), m.now())
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return User{}, &Error{Code: CodeInvalidCredentials, Cause: err}
		}
		return User{}, unavailable("session_authenticate", AuditFacts{}, err)
	}
	stored, err := m.metadata.GetUser(ctx, userID)
	if err != nil {
		if meta.IsNotFound(err) {
			return User{}, &Error{Code: CodeInvalidCredentials, Cause: err}
		}
		return User{}, unavailable("current_user", AuditFacts{}, err)
	}
	return userFromMeta(stored), nil
}

func (m *Manager) AuthenticateUserToken(ctx context.Context, token string) (User, error) {
	stored, err := m.metadata.AuthenticateUserAPIToken(ctx, token)
	if err != nil {
		if errors.Is(err, meta.ErrInvalidUserAPIToken) || meta.IsNotFound(err) {
			return User{}, &Error{Code: CodeInvalidCredentials, Cause: err}
		}
		return User{}, unavailable("user_api_token", AuditFacts{}, err)
	}
	return userFromMeta(stored), nil
}

func (m *Manager) Logout(ctx context.Context, token string, user User) (AuditFacts, error) {
	facts := userFacts("auth.logout", user, "user")
	if err := m.sessions.Delete(ctx, token); err != nil {
		facts.Success = false
		facts.FailureReason = "session_delete_failed"
		return facts, unavailable("session_delete", facts, err)
	}
	return facts, nil
}

func (m *Manager) CurrentUser(ctx context.Context, userID int) (User, error) {
	stored, err := m.metadata.GetUser(ctx, userID)
	if err != nil {
		return User{}, classify("current_user", AuditFacts{}, err)
	}
	return userFromMeta(stored), nil
}

func (m *Manager) ChangePassword(
	ctx context.Context,
	user User,
	currentPassword, newPassword, currentToken string,
) (AuditFacts, error) {
	facts := userFacts("auth.password_change", user, "user")
	if currentPassword == "" || newPassword == "" {
		return failure(facts, CodeInvalidRequest, "invalid_request", nil)
	}
	if len(newPassword) < minPasswordLength {
		return failure(facts, CodePasswordTooShort, "password_too_short", nil)
	}
	if err := m.metadata.ChangePassword(ctx, user.ID, currentPassword, newPassword); err != nil {
		if meta.IsInvalidCredentials(err) {
			return failure(facts, CodeInvalidCurrentPassword, "invalid_current_password", err)
		}
		return facts, classify("change_password", facts, err)
	}
	if err := m.sessions.RevokeOthers(ctx, user.ID, currentToken); err != nil {
		facts.Success = false
		facts.FailureReason = "session_revoke_failed"
		return facts, unavailable("session_revoke_others", facts, err)
	}
	return facts, nil
}

func (m *Manager) UpdateProfile(ctx context.Context, user User, input ProfileInput) (User, AuditFacts, error) {
	facts := userFacts("auth.profile_update", user, "user")
	if input.DisplayName == nil && input.Email == nil {
		failedFacts, err := failure(facts, CodeInvalidRequest, "invalid_request", nil)
		return User{}, failedFacts, err
	}
	if input.DisplayName != nil {
		trimmed := strings.TrimSpace(*input.DisplayName)
		if len(trimmed) > 120 {
			failedFacts, err := failure(facts, CodeInvalidRequest, "invalid_request", nil)
			return User{}, failedFacts, err
		}
		input.DisplayName = &trimmed
		facts.ChangedFields = append(facts.ChangedFields, "display_name")
	}
	if input.Email != nil {
		trimmed := strings.TrimSpace(*input.Email)
		if trimmed != "" && !strings.Contains(trimmed, "@") {
			failedFacts, err := failure(facts, CodeInvalidEmail, "invalid_email", nil)
			return User{}, failedFacts, err
		}
		input.Email = &trimmed
		facts.ChangedFields = append(facts.ChangedFields, "email")
	}
	stored, err := m.metadata.UpdateUserProfile(ctx, user.ID, input.DisplayName, input.Email)
	if err != nil {
		facts.Success = false
		facts.FailureReason = errorReason(err)
		return User{}, facts, classify("profile_update", facts, err)
	}
	updated := userFromMeta(stored)
	facts.TargetName = updated.Username
	return updated, facts, nil
}

func (m *Manager) ListSessions(ctx context.Context, userID int, currentToken string) ([]SessionRef, error) {
	refs, err := m.sessions.List(ctx, userID, currentToken, m.now())
	if err != nil {
		return nil, unavailable("session_list", AuditFacts{}, err)
	}
	return refs, nil
}

func (m *Manager) RevokeSession(ctx context.Context, user User, reference string) (AuditFacts, error) {
	facts := AuditFacts{
		Action: "auth.session_revoke", TargetType: "session", TargetName: reference, Success: true,
	}
	if strings.TrimSpace(reference) == "" {
		return failure(facts, CodeInvalidRequest, "invalid_request", nil)
	}
	if err := m.sessions.Revoke(ctx, user.ID, reference); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return failure(facts, CodeNotFound, "not_found", err)
		}
		facts.Success = false
		facts.FailureReason = "service_unavailable"
		return facts, unavailable("session_revoke", facts, err)
	}
	return facts, nil
}

func (m *Manager) ListUserTokens(ctx context.Context, userID int) ([]Token, error) {
	stored, err := m.metadata.ListUserAPITokens(ctx, userID)
	if err != nil {
		return nil, classify("user_api_token_list", AuditFacts{}, err)
	}
	result := make([]Token, 0, len(stored))
	for _, token := range stored {
		result = append(result, tokenFromMeta(token))
	}
	return result, nil
}

func (m *Manager) IssueUserToken(ctx context.Context, user User, name string) (IssuedToken, error) {
	name = strings.TrimSpace(name)
	facts := AuditFacts{
		Action: "user_api_token.create", TargetType: "user_api_token", TargetName: name, Success: true,
	}
	if name == "" {
		_, err := failure(facts, CodeInvalidRequest, "invalid_request", nil)
		return IssuedToken{}, err
	}
	stored, value, err := m.metadata.CreateUserAPIToken(ctx, user.ID, name)
	if err != nil {
		facts.Success = false
		facts.FailureReason = errorReason(err)
		return IssuedToken{}, classify("user_api_token_create", facts, err)
	}
	facts.TargetID = stored.ID
	facts.TargetName = stored.Name
	return IssuedToken{Token: tokenFromMeta(stored), Value: value, Facts: facts}, nil
}

func (m *Manager) RevokeUserToken(ctx context.Context, user User, tokenID int) (AuditFacts, error) {
	facts := AuditFacts{
		Action: "user_api_token.revoke", TargetType: "user_api_token", TargetID: tokenID, Success: true,
	}
	if tokenID <= 0 {
		return failure(facts, CodeInvalidRequest, "invalid_target_id", nil)
	}
	if err := m.metadata.RevokeUserAPITokenByID(ctx, user.ID, tokenID); err != nil {
		facts.Success = false
		facts.FailureReason = errorReason(err)
		return facts, classify("user_api_token_revoke", facts, err)
	}
	return facts, nil
}

func loginAccountLimitKey(username string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	return "login:account:" + hex.EncodeToString(sum[:])
}

func randomLoginFailureDelay() time.Duration {
	span := int64(loginFailureMaxDelay-loginFailureMinDelay) + 1
	offset, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return loginFailureMaxDelay
	}
	return loginFailureMinDelay + time.Duration(offset.Int64())
}

func unavailable(stage string, facts AuditFacts, cause error) error {
	facts.Success = false
	facts.FailureReason = "service_unavailable"
	return &Error{Code: CodeUnavailable, Stage: stage, Facts: facts, Cause: cause}
}

func classify(stage string, facts AuditFacts, err error) error {
	switch {
	case meta.IsNotFound(err):
		facts.Success = false
		facts.FailureReason = "not_found"
		return &Error{Code: CodeNotFound, Stage: stage, Facts: facts, Cause: err}
	case meta.IsConstraintError(err):
		facts.Success = false
		facts.FailureReason = "conflict"
		return &Error{Code: CodeConflict, Stage: stage, Facts: facts, Cause: err}
	default:
		return unavailable(stage, facts, err)
	}
}

func failure(facts AuditFacts, code ErrorCode, reason string, cause error) (AuditFacts, error) {
	facts.Success = false
	facts.FailureReason = reason
	return facts, &Error{Code: code, Facts: facts, Cause: cause}
}

func errorReason(err error) string {
	if meta.IsNotFound(err) {
		return "not_found"
	}
	if meta.IsConstraintError(err) {
		return "conflict"
	}
	return "internal_error"
}

func userFacts(action string, user User, targetType string) AuditFacts {
	return AuditFacts{
		Action: action, ActorUsername: user.Username, TargetType: targetType, TargetID: user.ID,
		TargetName: user.Username, TargetRole: user.Role, Success: true,
	}
}

func userFromMeta(user meta.UserInfo) User {
	return User{
		ID: user.ID, Username: user.Username, Role: user.Role, DisplayName: user.DisplayName, Email: user.Email,
		CreatedAt: user.CreatedAt, LastLoginAt: user.LastLoginAt, MustChangePassword: user.MustChangePassword,
	}
}

func tokenFromMeta(token meta.UserAPITokenInfo) Token {
	return Token{
		ID: token.ID, Name: token.Name, Prefix: token.Prefix, Revoked: token.Revoked, CreatedAt: token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
	}
}

func timePointer(value time.Time) *time.Time { return &value }
