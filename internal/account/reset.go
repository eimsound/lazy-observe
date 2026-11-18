package account

import (
	"context"
	"errors"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
)

var errEmptyNewPassword = errors.New("new password is empty")

// StageSessionRevokeAfterReset marks the one reset failure that happens after
// the new password is already stored: the password works, but sessions issued
// with the old password may still be alive.
const StageSessionRevokeAfterReset = "session_revoke_after_reset"

// PasswordResetStore is the metadata subset an operator-facing reset needs. It
// is deliberately narrower than Metadata: a reset never verifies the current
// password and never touches profile fields.
type PasswordResetStore interface {
	UserByUsername(context.Context, string) (meta.UserInfo, error)
	SetPasswordHash(context.Context, int, string, bool) error
}

// ResetPassword replaces username's password without the current one and
// revokes every session of that account. It backs the operator-facing
// `lazy-observe admin reset-password`, so mustChangePassword is decided by the
// caller: a generated password forces a change, an operator-supplied one does
// not.
//
// Ordering is deliberate. The hash is computed before anything else, sessions
// are revoked once before the write, the hash is stored, and sessions are
// revoked again afterwards: a sign-in racing the reset either sees the revoked
// state before the write or is caught by the second sweep, and no error path
// can leave a new password stored without the caller knowing. The one exception
// is StageSessionRevokeAfterReset: the password is applied, so callers must keep
// any generated password file instead of cleaning it up.
//
// Sessions are revoked through the same Sessions adapter the server uses, so
// deployments that keep sessions in Redis lose them too. An empty keepToken
// keeps nothing: both adapters store token digests, so the empty digest can
// never match a live session.
func ResetPassword(
	ctx context.Context,
	metadata PasswordResetStore,
	sessions Sessions,
	username, newPassword string,
	mustChangePassword bool,
) (meta.UserInfo, error) {
	if metadata == nil || sessions == nil {
		panic("account: metadata and sessions are required")
	}
	facts := AuditFacts{
		Action: "auth.password_reset", TargetType: "user", TargetName: username, Success: true,
	}
	if newPassword == "" {
		return meta.UserInfo{}, &Error{
			Code: CodeInvalidRequest, Stage: "reset_password", Facts: facts,
			Cause: errEmptyNewPassword,
		}
	}
	// Hash before revoking: Argon2id is deliberately slow, and doing it after
	// the first sweep would widen the window in which the old password still
	// signs in.
	hash, err := meta.HashPassword(newPassword)
	if err != nil {
		return meta.UserInfo{}, &Error{Code: CodeUnavailable, Stage: "hash_password", Facts: facts, Cause: err}
	}
	user, err := metadata.UserByUsername(ctx, username)
	if err != nil {
		return meta.UserInfo{}, classify("find_user", facts, err)
	}
	if err := sessions.RevokeOthers(ctx, user.ID, ""); err != nil {
		// Nothing was written yet, so the old password still works.
		return meta.UserInfo{}, unavailable("session_revoke", facts, err)
	}
	if err := metadata.SetPasswordHash(ctx, user.ID, hash, mustChangePassword); err != nil {
		return meta.UserInfo{}, classify("reset_password", facts, err)
	}
	user.MustChangePassword = mustChangePassword
	if err := sessions.RevokeOthers(ctx, user.ID, ""); err != nil {
		return user, unavailable(StageSessionRevokeAfterReset, facts, err)
	}
	return user, nil
}

// ClearLoginFailures resets the shared lockout state for username, so a reset
// password is immediately usable after failed sign-in attempts. Gateways keep
// the counter in process memory unless Redis is enabled, so a CLI can only
// clear the Redis-backed one; see docs/deployment.md.
func ClearLoginFailures(ctx context.Context, gate state.LoginGate, username string) error {
	if gate == nil {
		return nil
	}
	return gate.Clear(ctx, loginAccountLimitKey(username))
}
