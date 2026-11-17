package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every cookie-authenticated mutation under /api/auth must pass the
// double-submit CSRF check; login itself cannot (there is no session yet).
func TestAuthMutationsRequireCSRF(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "csrf-user", "password", "member")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, RequireCSRF: true}).App()
	for _, test := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/auth/tokens", `{"name":"cli"}`},
		{http.MethodDelete, "/api/auth/tokens/1", ""},
		{http.MethodPost, "/api/auth/password", `{"current_password":"password","new_password":"new-password-123"}`},
		{http.MethodPut, "/api/auth/me", `{"display_name":"x"}`},
		{http.MethodDelete, "/api/auth/sessions/1", ""},
		{http.MethodPost, "/api/auth/logout", ""},
	} {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, bytes.NewBufferString(test.body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(cookie)
			req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "expected"})
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status=%d want=403 body=%s", resp.StatusCode, readBody(resp))
			}
		})
	}

	t.Run("matching token passes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/tokens", bytes.NewBufferString(`{"name":"cli"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrfHeaderName, "expected")
		req.AddCookie(cookie)
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "expected"})
		resp, err := app.Test(req)
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("status=%d err=%v", resp.StatusCode, err)
		}
	})
	t.Run("login is exempt", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/auth/login",
			bytes.NewBufferString(`{"username":"csrf-user","password":"password"}`),
		)
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("login status=%d err=%v body=%s", resp.StatusCode, err, readBody(resp))
		}
	})
}
