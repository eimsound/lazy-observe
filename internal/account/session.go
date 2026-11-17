// Package account centralizes current-user authentication and account behavior.
package account

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
)

var ErrSessionNotFound = errors.New("account session not found")

// SessionRef is safe to return to a user. ID is an opaque revocation
// reference, not an authentication credential.
type SessionRef struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

// Sessions is the backend-neutral session interface used by account behavior.
type Sessions interface {
	Create(context.Context, int, time.Time, time.Time) (string, error)
	Authenticate(context.Context, string, time.Time) (int, error)
	Delete(context.Context, string) error
	List(context.Context, int, string, time.Time) ([]SessionRef, error)
	Revoke(context.Context, int, string) error
	RevokeOthers(context.Context, int, string) error
}

type metadataSessionStore interface {
	CreateAccountSession(context.Context, meta.AccountSession) error
	AuthenticateAccountSession(context.Context, string, time.Time) (int, error)
	DeleteAccountSession(context.Context, string) error
	ListAccountSessions(context.Context, int, string, time.Time) ([]meta.AccountSessionRef, error)
	DeleteAccountSessionByRef(context.Context, int, string) error
	DeleteOtherAccountSessions(context.Context, int, string) error
}

type metadataSessions struct{ store metadataSessionStore }

func NewMetadataSessions(store metadataSessionStore) Sessions { return &metadataSessions{store: store} }

func (s *metadataSessions) Create(
	ctx context.Context,
	userID int,
	createdAt, expiresAt time.Time,
) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateAccountSession(ctx, meta.AccountSession{
		Token: token, UserID: userID, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}); err != nil {
		return "", err
	}
	return token, nil
}

func (s *metadataSessions) Authenticate(ctx context.Context, token string, now time.Time) (int, error) {
	userID, err := s.store.AuthenticateAccountSession(ctx, token, now)
	if err != nil && (meta.IsNotFound(err) || meta.IsInvalidCredentials(err)) {
		return 0, ErrSessionNotFound
	}
	return userID, err
}

func (s *metadataSessions) Delete(ctx context.Context, token string) error {
	return s.store.DeleteAccountSession(ctx, token)
}

func (s *metadataSessions) List(
	ctx context.Context,
	userID int,
	currentToken string,
	now time.Time,
) ([]SessionRef, error) {
	rows, err := s.store.ListAccountSessions(ctx, userID, currentToken, now)
	if err != nil {
		return nil, err
	}
	result := make([]SessionRef, 0, len(rows))
	for _, row := range rows {
		result = append(result, SessionRef(row))
	}
	return result, nil
}

func (s *metadataSessions) Revoke(ctx context.Context, userID int, reference string) error {
	err := s.store.DeleteAccountSessionByRef(ctx, userID, reference)
	if meta.IsNotFound(err) {
		return ErrSessionNotFound
	}
	return err
}

func (s *metadataSessions) RevokeOthers(ctx context.Context, userID int, keepToken string) error {
	return s.store.DeleteOtherAccountSessions(ctx, userID, keepToken)
}

type stateSessions struct{ store state.SessionStore }

func NewStateSessions(store state.SessionStore) Sessions { return &stateSessions{store: store} }

func (s *stateSessions) Create(
	ctx context.Context,
	userID int,
	createdAt, expiresAt time.Time,
) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	if err := s.store.Create(ctx, state.Session{
		Token: token, UserID: userID, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}); err != nil {
		return "", err
	}
	return token, nil
}

func (s *stateSessions) Authenticate(ctx context.Context, token string, _ time.Time) (int, error) {
	session, err := s.store.Get(ctx, token)
	if errors.Is(err, state.ErrNotFound) {
		return 0, ErrSessionNotFound
	}
	return session.UserID, err
}

func (s *stateSessions) Delete(ctx context.Context, token string) error {
	return s.store.Delete(ctx, token)
}

func (s *stateSessions) List(
	ctx context.Context,
	userID int,
	currentToken string,
	_ time.Time,
) ([]SessionRef, error) {
	refs, err := s.store.ListByUser(ctx, userID, currentToken)
	if err != nil {
		return nil, err
	}
	result := make([]SessionRef, 0, len(refs))
	for _, ref := range refs {
		result = append(result, SessionRef(ref))
	}
	return result, nil
}

func (s *stateSessions) Revoke(ctx context.Context, userID int, reference string) error {
	err := s.store.DeleteByRef(ctx, userID, reference)
	if errors.Is(err, state.ErrNotFound) {
		return ErrSessionNotFound
	}
	return err
}

func (s *stateSessions) RevokeOthers(ctx context.Context, userID int, keepToken string) error {
	return s.store.DeleteOtherByUser(ctx, userID, keepToken)
}

func newSessionToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return "los_" + base64.RawURLEncoding.EncodeToString(value), nil
}
