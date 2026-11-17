package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

// bootstrapGeneratedAdmin runs the real bootstrap with a generated password and
// returns that password, read from the 0600 file like an operator would.
func bootstrapGeneratedAdmin(t *testing.T, store *meta.Store) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := store.Bootstrap(
		context.Background(),
		logger,
		meta.BootstrapAdmin{GeneratedPasswordFile: path},
	); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

type passwordChangeClient struct {
	t   *testing.T
	app *fiber.App
}

func (c passwordChangeClient) do(method, path, body string, credential func(*http.Request)) *http.Response {
	c.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != nil {
		credential(req)
	}
	resp, err := c.app.Test(req, -1)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c passwordChangeClient) login(password string) *http.Cookie {
	c.t.Helper()
	payload, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
	resp := c.do(http.MethodPost, "/api/auth/login", string(payload), nil)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("login status=%d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "lo_session" {
			return cookie
		}
	}
	c.t.Fatal("login did not set lo_session")
	return nil
}

func withCookie(cookie *http.Cookie) func(*http.Request) {
	return func(req *http.Request) { req.AddCookie(cookie) }
}

func withBearer(token string) func(*http.Request) {
	return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+token) }
}

func decodeJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func assertPasswordChangeRequired(t *testing.T, resp *http.Response, route string) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s status=%d, want 403 password_change_required", route, resp.StatusCode)
	}
	if body := decodeJSONBody(t, resp); body["code"] != "password_change_required" {
		t.Fatalf("%s body=%v, want code password_change_required", route, body)
	}
}

func TestGeneratedBootstrapAdminMustChangePasswordBeforeUsingTheAPI(t *testing.T) {
	adapters := map[string]func(*testing.T, *meta.Store) account.Behavior{
		"metadata sessions": func(_ *testing.T, store *meta.Store) account.Behavior {
			return testAccount(store, account.Options{LoginFailureDelay: noLoginFailureDelay})
		},
		"redis sessions": func(t *testing.T, store *meta.Store) account.Behavior {
			client := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			t.Cleanup(func() { _ = client.Close() })
			shared := state.NewRedis(client, "password-change-test")
			return account.New(store, account.NewStateSessions(shared), account.Options{
				Limits: shared, LoginGate: shared, LoginFailureDelay: noLoginFailureDelay,
			})
		},
	}
	for name, newAccount := range adapters {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			password := bootstrapGeneratedAdmin(t, store)
			adminID := mustUserID(t, store, "admin")
			_, userToken, err := store.CreateUserAPIToken(context.Background(), adminID, "pre-existing")
			if err != nil {
				t.Fatal(err)
			}
			client := passwordChangeClient{t: t, app: newMetadataTestGateway(store, Config{
				Store: store, Tokens: store, Account: newAccount(t, store), Management: management.New(store),
			}).App()}
			session := client.login(password)

			me := client.do(http.MethodGet, "/api/auth/me", "", withCookie(session))
			if me.StatusCode != http.StatusOK {
				t.Fatalf("me status=%d", me.StatusCode)
			}
			if body := decodeJSONBody(t, me); body["must_change_password"] != true {
				t.Fatalf("me body=%v, want must_change_password=true", body)
			}
			for _, route := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/projects", ""},
				{http.MethodPost, "/api/projects", `{"name":"Blocked","slug":"blocked"}`},
				{http.MethodGet, "/api/users", ""},
				{http.MethodGet, "/api/v1/issues", ""},
				{http.MethodGet, "/api/auth/tokens", ""},
				{http.MethodPost, "/api/auth/tokens", `{"name":"blocked"}`},
				{http.MethodPut, "/api/auth/me", `{"display_name":"blocked"}`},
				{http.MethodGet, "/api/auth/sessions", ""},
			} {
				resp := client.do(route.method, route.path, route.body, withCookie(session))
				assertPasswordChangeRequired(t, resp, route.method+" "+route.path+" (session)")
			}
			// A user API token belonging to the flagged user follows the same
			// rule: it may read the current user and nothing else.
			assertPasswordChangeRequired(
				t,
				client.do(http.MethodGet, "/api/v1/issues", "", withBearer(userToken)),
				"GET /api/v1/issues (user token)",
			)
			assertPasswordChangeRequired(
				t,
				client.do(http.MethodGet, "/api/projects", "", withBearer(userToken)),
				"GET /api/projects (user token)",
			)
			if resp := client.do(
				http.MethodGet,
				"/api/auth/me",
				"",
				withBearer(userToken),
			); resp.StatusCode != http.StatusOK {
				t.Fatalf("me with user token status=%d", resp.StatusCode)
			}

			// Logout stays reachable for a flagged session.
			spare := client.login(password)
			if resp := client.do(
				http.MethodPost,
				"/api/auth/logout",
				"",
				withCookie(spare),
			); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("logout status=%d", resp.StatusCode)
			}

			changed := client.do(
				http.MethodPost,
				"/api/auth/password",
				`{"current_password":"`+password+`","new_password":"operator-chosen-password"}`,
				withCookie(session),
			)
			if changed.StatusCode != http.StatusNoContent {
				t.Fatalf("change password status=%d", changed.StatusCode)
			}
			me = client.do(http.MethodGet, "/api/auth/me", "", withCookie(session))
			if body := decodeJSONBody(t, me); body["must_change_password"] != false {
				t.Fatalf("me after change body=%v, want must_change_password=false", body)
			}
			if resp := client.do(
				http.MethodGet,
				"/api/projects",
				"",
				withCookie(session),
			); resp.StatusCode != http.StatusOK {
				t.Fatalf("projects after change status=%d", resp.StatusCode)
			}
			if resp := client.do(
				http.MethodGet,
				"/api/projects",
				"",
				withBearer(userToken),
			); resp.StatusCode != http.StatusOK {
				t.Fatalf("user token after change status=%d", resp.StatusCode)
			}
		})
	}
}

func TestProvidedBootstrapAdminPasswordIsNotForcedToChange(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "admin-password")
	const provided = "deployer-provided-password"
	if err := os.WriteFile(passwordFile, []byte(provided+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := store.Bootstrap(
		context.Background(),
		logger,
		meta.BootstrapAdmin{PasswordFile: passwordFile},
	); err != nil {
		t.Fatal(err)
	}
	client := passwordChangeClient{t: t, app: newMetadataTestGateway(
		store,
		Config{Store: store, Tokens: store, Management: management.New(store)},
	).App()}
	session := client.login(provided)
	me := client.do(http.MethodGet, "/api/auth/me", "", withCookie(session))
	if body := decodeJSONBody(t, me); body["must_change_password"] != false {
		t.Fatalf("me body=%v, want must_change_password=false", body)
	}
	if resp := client.do(http.MethodGet, "/api/projects", "", withCookie(session)); resp.StatusCode != http.StatusOK {
		t.Fatalf("projects status=%d", resp.StatusCode)
	}
}
