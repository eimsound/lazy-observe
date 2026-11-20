package meta

import (
	"context"
	"errors"
	"fmt"

	"entgo.io/ent/dialect"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/user"
)

// ErrLastAdminProtected is returned when an update or deletion would leave the
// platform without any admin account.
var ErrLastAdminProtected = errors.New("cannot remove or demote the last admin")

// IsLastAdminProtected reports whether an error was caused by last-admin
// protection.
func IsLastAdminProtected(err error) bool {
	return errors.Is(err, ErrLastAdminProtected)
}

func userRoleRank(role string) int {
	switch role {
	case string(user.RoleAdmin):
		return 3
	case string(user.RoleMember):
		return 2
	case string(user.RoleViewer):
		return 1
	default:
		return 0
	}
}

func isUserRoleDowngrade(current user.Role, next string) bool {
	if next == "" {
		return false
	}
	return userRoleRank(next) < userRoleRank(string(current))
}

func (s *Store) acquireAdminRoleCoordination(ctx context.Context) (func() error, error) {
	if s.dialect != dialect.Postgres {
		return func() error { return nil }, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve PostgreSQL admin-role connection: %w", err)
	}
	lockKey, err := acquirePostgresAdvisoryLock(
		ctx,
		conn,
		s.startupLockTimeout,
		"admin-role-mutation",
		"PostgreSQL admin role mutation",
	)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() error {
		defer conn.Close()
		return releasePostgresAdvisoryLock(conn, lockKey, "PostgreSQL admin role mutation")
	}, nil
}

func ensureNotLastAdminTx(ctx context.Context, tx *dbent.Tx) error {
	count, err := tx.User.Query().Where(user.RoleEQ(user.RoleAdmin)).Count(ctx)
	if err != nil {
		return err
	}
	if count <= 1 {
		return ErrLastAdminProtected
	}
	return nil
}
