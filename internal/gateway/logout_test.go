package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eimsound/lazy-observe/internal/account"
)

type logoutFailsAccount struct{ unavailableTestAccount }

func (logoutFailsAccount) AuthenticateSession(context.Context, string) (account.User, error) {
	return account.User{ID: 1, Username: "u", Role: "member"}, nil
}

func (logoutFailsAccount) Logout(context.Context, string, account.User) (account.AuditFacts, error) {
	return account.AuditFacts{Action: "auth.logout", FailureReason: "session_delete_failed"},
		&account.Error{Code: account.CodeUnavailable, Stage: "session_delete"}
}

// ADR 0006: a partially completed account operation is never reported as
// success. If the server-side session survives, logout must say so and keep
// the cookie so the client can retry, instead of answering 204.
func TestLogoutReportsSessionDeleteFailure(t *testing.T) {
	app := newTestGateway(Config{Account: logoutFailsAccount{}, Tokens: &fakeTokens{}}).App()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "lo_session", Value: "still-valid"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=503", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "lo_session" {
			t.Fatalf("logout cleared the cookie of a session that still exists: %+v", cookie)
		}
	}
}
