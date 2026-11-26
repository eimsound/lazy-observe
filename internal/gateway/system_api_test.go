package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

func TestSystemInfoRequiresTokenAndReturnsOnlyScaleIdentity(t *testing.T) {
	tokens := &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}
	app := newTestGateway(Config{Tokens: tokens, InstanceID: "loi_test-instance"}).App()

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/system/info", nil))
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d err=%v", response.StatusCode, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	req.Header.Set("Authorization", "Bearer "+"lou_valid")
	response, err = app.Test(req)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status=%d err=%v", response.StatusCode, err)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || payload["instance_id"] != "loi_test-instance" || payload["gomaxprocs"] == nil {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestSystemInfoGeneratesDistinctProcessIDs(t *testing.T) {
	first := newTestGateway(Config{}).instanceID
	second := newTestGateway(Config{}).instanceID
	if first == "" || second == "" || first == second {
		t.Fatalf("generated instance ids are not distinct: %q %q", first, second)
	}
}

func TestWebSelfMonitoringConfigEndpoint(t *testing.T) {
	t.Run("requires-active-session-and-rejects-user-token-or-must-change", func(t *testing.T) {
		store := testStore(t)
		activeSession := sessionCookie(t, store, "viewer-user", "password", "viewer")
		userToken := userQueryToken(t, store, "token-user")

		mustChangeUser, err := store.CreateUser(context.Background(), "pending-admin", "password", "admin")
		if err != nil {
			t.Fatal(err)
		}
		hash, err := meta.HashPasswordContext(context.Background(), "password")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetPasswordHash(context.Background(), mustChangeUser.ID, hash, true); err != nil {
			t.Fatal(err)
		}
		mustChangeCookie := &http.Cookie{
			Name:  "lo_session",
			Value: metadataSession(t, store, mustChangeUser.ID, time.Now(), time.Now().Add(time.Hour)),
		}

		app := newMetadataTestGateway(store, Config{
			WebSelfMonitoring: WebSelfMonitoringConfig{
				Enabled:     true,
				ProjectSlug: meta.SelfMonitoringProjectSlug,
				Token:       "lo_test-browser-self-monitoring-token",
			},
		}).App()

		// Anonymous requests are rejected.
		unauthResp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/system/self-monitoring", nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = unauthResp.Body.Close()
		if unauthResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated status=%d want=401", unauthResp.StatusCode)
		}

		// User API tokens are rejected (authSessionOnly).
		tokenReq := httptest.NewRequest(http.MethodGet, "/api/system/self-monitoring", nil)
		tokenReq.Header.Set("Authorization", "Bearer "+userToken)
		tokenResp, err := app.Test(tokenReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = tokenResp.Body.Close()
		if tokenResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("user token status=%d want=401", tokenResp.StatusCode)
		}

		// Forced password-change sessions are blocked with 403 password_change_required.
		mustChangeReq := httptest.NewRequest(http.MethodGet, "/api/system/self-monitoring", nil)
		mustChangeReq.AddCookie(mustChangeCookie)
		mustChangeResp, err := app.Test(mustChangeReq)
		if err != nil {
			t.Fatal(err)
		}
		mustChangeBody, _ := io.ReadAll(mustChangeResp.Body)
		_ = mustChangeResp.Body.Close()
		if mustChangeResp.StatusCode != http.StatusForbidden ||
			!bytes.Contains(mustChangeBody, []byte("password_change_required")) {
			t.Fatalf("must_change_password status=%d body=%s", mustChangeResp.StatusCode, mustChangeBody)
		}

		// Active normal session succeeds.
		okReq := httptest.NewRequest(http.MethodGet, "/api/system/self-monitoring", nil)
		okReq.AddCookie(activeSession)
		okResp, err := app.Test(okReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = okResp.Body.Close()
		if okResp.StatusCode != http.StatusOK {
			t.Fatalf("active session status=%d want=200", okResp.StatusCode)
		}
	})

	t.Run("disabled-and-incomplete-configs-fail-closed", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cfg  WebSelfMonitoringConfig
		}{
			{
				name: "disabled",
				cfg:  WebSelfMonitoringConfig{},
			},
			{
				name: "missing-token",
				cfg: WebSelfMonitoringConfig{
					Enabled:     true,
					ProjectSlug: meta.SelfMonitoringProjectSlug,
					Token:       "",
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store := testStore(t)
				cookie := sessionCookie(t, store, "viewer-user", "password", "viewer")
				app := newMetadataTestGateway(store, Config{WebSelfMonitoring: tc.cfg}).App()

				req := httptest.NewRequest(http.MethodGet, "/api/system/self-monitoring", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d want=200", resp.StatusCode)
				}
				if got := resp.Header.Get("Cache-Control"); got != "no-store" {
					t.Fatalf("Cache-Control=%q want=no-store", got)
				}
				var payload map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if enabled, ok := payload["enabled"].(bool); !ok || enabled || len(payload) != 1 {
					t.Fatalf("disabled/incomplete payload=%#v", payload)
				}
			})
		}
	})

	t.Run("enabled-returns-session-browser-config-with-replay-flag", func(t *testing.T) {
		for _, replayEnabled := range []bool{false, true} {
			store := testStore(t)
			cookie := sessionCookie(t, store, "viewer-user", "password", "viewer")
			app := newMetadataTestGateway(store, Config{
				WebSelfMonitoring: WebSelfMonitoringConfig{
					Enabled:       true,
					ReplayEnabled: replayEnabled,
					ProjectSlug:   meta.SelfMonitoringProjectSlug,
					Token:         "lo_test-browser-self-monitoring-token",
					Release:       "v0.9.0-test",
				},
			}).App()

			req := httptest.NewRequest(http.MethodGet, "/api/system/self-monitoring", nil)
			req.AddCookie(cookie)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				t.Fatalf("status=%d want=200", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				_ = resp.Body.Close()
				t.Fatalf("Cache-Control=%q want=no-store", got)
			}
			var payload struct {
				Enabled       bool   `json:"enabled"`
				ProjectSlug   string `json:"project_slug"`
				Token         string `json:"token"`
				ServiceName   string `json:"service_name"`
				Release       string `json:"release"`
				ReplayEnabled bool   `json:"replay_enabled"`
			}
			err = json.NewDecoder(resp.Body).Decode(&payload)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !payload.Enabled ||
				payload.ProjectSlug != meta.SelfMonitoringProjectSlug ||
				payload.Token != "lo_test-browser-self-monitoring-token" ||
				payload.ServiceName != "lazy-observe-web" ||
				payload.Release != "v0.9.0-test" ||
				payload.ReplayEnabled != replayEnabled {
				t.Fatalf("replayEnabled=%t payload=%+v", replayEnabled, payload)
			}
		}
	})
}

func TestWebSelfMonitoringInstrumentationExemptionIsScoped(t *testing.T) {
	store := testStore(t)
	selfProject, backendToken, err := store.BootstrapSelfMonitoring(
		t.Context(),
		"lo_test-backend-self-monitoring-derived-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	const webToken = "lo_test-browser-self-monitoring-derived-token"
	if _, err := store.BootstrapSelfMonitoringWebToken(t.Context(), selfProject.ID, webToken); err != nil {
		t.Fatal(err)
	}
	otherProject, err := store.CreateProject(t.Context(), "Other", "other")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := store.CreateProjectAPIToken(t.Context(), otherProject.ID)
	if err != nil {
		t.Fatal(err)
	}

	var observed []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	gw := newMetadataTestGateway(store, Config{
		Store:               store,
		Tokens:              store,
		Metrics:             upstream.URL,
		Logs:                upstream.URL,
		Traces:              upstream.URL,
		SelfMonitoringToken: backendToken,
		WebSelfMonitoring: WebSelfMonitoringConfig{
			Enabled:     true,
			ProjectSlug: selfProject.Slug,
			Token:       webToken,
		},
		Instrumentation: func(c fiber.Ctx) error {
			observed = append(observed, c.Method()+" "+c.Path())
			return c.Next()
		},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	// 1. Browser self-monitoring ingest routes with webToken skip Fiber self-instrumentation.
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/events/errors", `{}`},
		{"/api/events/vitals", `{}`},
		{"/api/events/diagnostics", `{}`},
		{"/api/replay/segments", `{}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body))
		req.Header.Set("Authorization", "Bearer "+webToken)
		req.Header.Set("Content-Type", "application/json")
		resp, reqErr := app.Test(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		_ = resp.Body.Close()
	}
	if len(observed) != 0 {
		t.Fatalf("expected browser self-monitoring ingest routes to skip instrumentation, observed=%v", observed)
	}

	// 2. Non-ingest routes using webToken are still observed and read routes stay 403 ingest_only_token.
	reqInfo := httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	reqInfo.Header.Set("Authorization", "Bearer "+webToken)
	respInfo, err := app.Test(reqInfo)
	if err != nil {
		t.Fatal(err)
	}
	rawInfo, _ := io.ReadAll(respInfo.Body)
	_ = respInfo.Body.Close()
	if respInfo.StatusCode != http.StatusForbidden || !bytes.Contains(rawInfo, []byte("ingest_only_token")) {
		t.Fatalf("GET /api/system/info with webToken: status=%d body=%s", respInfo.StatusCode, rawInfo)
	}

	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	reqHealth.Header.Set("Authorization", "Bearer "+webToken)
	respHealth, err := app.Test(reqHealth)
	if err != nil {
		t.Fatal(err)
	}
	_ = respHealth.Body.Close()

	// 3. Regular project tokens hitting /api/events/vitals are still observed.
	reqOther := httptest.NewRequest(http.MethodPost, "/api/events/vitals", bytes.NewBufferString(`{}`))
	reqOther.Header.Set("Authorization", "Bearer "+otherToken)
	reqOther.Header.Set("Content-Type", "application/json")
	respOther, err := app.Test(reqOther)
	if err != nil {
		t.Fatal(err)
	}
	_ = respOther.Body.Close()

	wantObserved := []string{"GET /api/system/info", "GET /healthz", "POST /api/events/vitals"}
	if !slices.Equal(observed, wantObserved) {
		t.Fatalf("observed=%v want=%v", observed, wantObserved)
	}
}
