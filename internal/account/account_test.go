package account_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/meta"
)

type revokeSessionFailure struct{ account.Sessions }

func (revokeSessionFailure) Revoke(context.Context, int, string) error {
	return errors.New("session backend unavailable")
}

func TestCurrentUserProfileAndTokenBehavior(t *testing.T) {
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stored, err := store.CreateUser(t.Context(), "account-owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateUser(t.Context(), "other-owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	manager := account.New(store, account.NewMetadataSessions(store), account.Options{
		LoginFailureDelay: noDelay,
	})
	user, err := manager.CurrentUser(t.Context(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}

	invalidEmail := "not-an-email"
	if _, _, err := manager.UpdateProfile(
		t.Context(), user, account.ProfileInput{Email: &invalidEmail},
	); account.Code(err) != account.CodeInvalidEmail {
		t.Fatalf("invalid email err=%v", err)
	}
	displayName, email := "  Ada  ", " ada@example.com "
	updated, facts, err := manager.UpdateProfile(t.Context(), user, account.ProfileInput{
		DisplayName: &displayName, Email: &email,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Ada" || updated.Email != "ada@example.com" {
		t.Fatalf("updated user=%#v", updated)
	}
	if !facts.Success || strings.Join(facts.ChangedFields, ",") != "display_name,email" {
		t.Fatalf("profile facts=%#v", facts)
	}
	empty := ""
	updated, _, err = manager.UpdateProfile(t.Context(), updated, account.ProfileInput{DisplayName: &empty})
	if err != nil || updated.DisplayName != "" {
		t.Fatalf("clear display name user=%#v err=%v", updated, err)
	}

	issued, err := manager.IssueUserToken(t.Context(), updated, "automation")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Value, "lou_") ||
		strings.Contains(strings.Join(issued.Facts.ChangedFields, ","), issued.Value) {
		t.Fatalf("issued token or facts invalid: %#v", issued)
	}
	listed, err := manager.ListUserTokens(t.Context(), updated.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != issued.Token.ID {
		t.Fatalf("listed tokens=%#v err=%v", listed, err)
	}
	authenticated, err := manager.AuthenticateUserToken(t.Context(), issued.Value)
	if err != nil || authenticated.ID != updated.ID {
		t.Fatalf("authenticated user=%#v err=%v", authenticated, err)
	}
	otherUser, err := manager.CurrentUser(t.Context(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RevokeUserToken(
		t.Context(),
		otherUser,
		issued.Token.ID,
	); account.Code(
		err,
	) != account.CodeNotFound {
		t.Fatalf("cross-user revoke err=%v", err)
	}
	if _, err := manager.RevokeUserToken(t.Context(), updated, issued.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AuthenticateUserToken(
		t.Context(),
		issued.Value,
	); account.Code(
		err,
	) != account.CodeInvalidCredentials {
		t.Fatalf("revoked token authenticated: %v", err)
	}
}

func noDelay() time.Duration { return 0 }

func TestSessionRevokeFailureFactsReportFailure(t *testing.T) {
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stored, err := store.CreateUser(t.Context(), "revoke-owner", "password", "member")
	if err != nil {
		t.Fatal(err)
	}
	manager := account.New(
		store,
		revokeSessionFailure{Sessions: account.NewMetadataSessions(store)},
		account.Options{LoginFailureDelay: noDelay},
	)
	user, err := manager.CurrentUser(t.Context(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := manager.RevokeSession(t.Context(), user, "opaque-reference")
	if account.Code(err) != account.CodeUnavailable {
		t.Fatalf("revoke err=%v", err)
	}
	if facts.Success || facts.FailureReason != "service_unavailable" {
		t.Fatalf("revoke facts=%#v", facts)
	}
}

type stubAuthMetadata struct {
	account.Metadata
	err   error
	calls int
}

func (s *stubAuthMetadata) AuthenticateCredentials(context.Context, string, string) (meta.UserInfo, error) {
	s.calls++
	return meta.UserInfo{}, s.err
}

func TestLoginRejectsOversizedInputsBeforeStoreAndTruncatesAuditName(t *testing.T) {
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stub := &stubAuthMetadata{Metadata: store, err: errors.New("should not be called")}
	manager := account.New(stub, account.NewMetadataSessions(store), account.Options{
		LoginFailureDelay: noDelay,
	})

	longUsername := strings.Repeat("a", account.MaxUsernameLength+64)
	_, err = manager.Login(t.Context(), account.LoginInput{
		Username: longUsername, Password: "password-123", SourceIP: "198.51.100.1",
	})
	if account.Code(err) != account.CodeInvalidRequest {
		t.Fatalf("long username err=%v, want CodeInvalidRequest", err)
	}
	if stub.calls != 0 {
		t.Fatalf("AuthenticateCredentials called %d times for oversized username", stub.calls)
	}
	facts := account.Facts(err)
	if len(facts.ActorUsername) > account.MaxUsernameLength || len(facts.TargetName) > account.MaxUsernameLength {
		t.Fatalf("audit facts did not bound username length: %d/%d", len(facts.ActorUsername), len(facts.TargetName))
	}

	longPassword := strings.Repeat("p", account.MaxPasswordLength+32)
	_, err = manager.Login(t.Context(), account.LoginInput{
		Username: "normal-user", Password: longPassword, SourceIP: "198.51.100.2",
	})
	if account.Code(err) != account.CodeInvalidRequest {
		t.Fatalf("long password err=%v, want CodeInvalidRequest", err)
	}
	if stub.calls != 0 {
		t.Fatalf("AuthenticateCredentials called %d times for oversized password", stub.calls)
	}
}

func TestLoginStorageFailureAndArgonBusyDoNotLockOutAccount(t *testing.T) {
	store, err := meta.OpenStore(t.Context(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for name, injected := range map[string]error{
		"db_error":   errors.New("postgres connection refused"),
		"argon_busy": meta.ErrPasswordVerificationBusy,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubAuthMetadata{Metadata: store, err: injected}
			manager := account.New(stub, account.NewMetadataSessions(store), account.Options{
				LoginFailureDelay: noDelay,
			})
			for i := range 6 {
				_, loginErr := manager.Login(t.Context(), account.LoginInput{
					Username: "target-user",
					Password: "any-password",
					SourceIP: "203.0.113." + string(rune('1'+i)),
				})
				if account.Code(loginErr) != account.CodeUnavailable {
					t.Fatalf("attempt %d: code=%v err=%v, want CodeUnavailable", i+1, account.Code(loginErr), loginErr)
				}
				if errors.Is(injected, meta.ErrPasswordVerificationBusy) && account.RetryAfter(loginErr) <= 0 {
					t.Fatalf("attempt %d: expected RetryAfter for busy argon2", i+1)
				}
			}
		})
	}
}
