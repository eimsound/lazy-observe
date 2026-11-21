package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

func TestAllRegisteredRoutesHaveExplicitDeclarativePolicy(t *testing.T) {
	store := testStore(t)
	gw := newMetadataTestGateway(store, Config{
		Store:      store,
		Tokens:     store,
		Management: management.New(store),
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	fiberRoutes := gw.App().GetRoutes(true)
	if len(fiberRoutes) == 0 {
		t.Fatal("expected registered Fiber routes")
	}

	seen := make(map[string]bool)
	for _, r := range fiberRoutes {
		if r.Method == "USE" {
			continue
		}
		key := r.Method + " " + r.Path
		allKey := routeMethodAll + " " + r.Path
		_, exactOK := gw.registeredIndex[key]
		_, allOK := gw.registeredIndex[allKey]
		if !exactOK && !allOK {
			t.Errorf("Fiber route %s %s is missing from declarative registeredIndex", r.Method, r.Path)
		}
		seen[key] = true
		seen[allKey] = true
	}

	for _, reg := range gw.registeredRoutes {
		key := reg.method + " " + reg.path
		if !seen[key] {
			t.Errorf("declarative route %s was not registered on Fiber app", key)
		}
	}
}

func TestRoutePolicyValidationPanicsOnInvalidCombinations(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		policy routePolicy
	}{
		{
			name:   "role on public route",
			method: fiber.MethodGet,
			path:   "/bad",
			policy: routePolicy{auth: authPublic, role: roleAdmin},
		},
		{
			name:   "csrf on project token route",
			method: fiber.MethodPost,
			path:   "/bad",
			policy: routePolicy{auth: authProjectToken, csrf: true},
		},
		{
			name:   "mutating session-only route without csrf",
			method: fiber.MethodPost,
			path:   "/bad",
			policy: routePolicy{auth: authSessionOnly, csrf: false},
		},
		{
			name:   "allowWhenMustChange on public route",
			method: fiber.MethodGet,
			path:   "/bad",
			policy: routePolicy{auth: authPublic, allowWhenMustChange: true},
		},
		{
			name:   "projectScope on project token route",
			method: fiber.MethodPost,
			path:   "/bad",
			policy: routePolicy{auth: authProjectToken, projectScope: true},
		},
		{
			name:   "browserIngestCORS on user route",
			method: fiber.MethodPost,
			path:   "/bad",
			policy: routePolicy{auth: authUser, browserIngestCORS: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for invalid policy %+v", tc.policy)
				}
			}()
			validateRoutePolicy(tc.method, tc.path, tc.policy)
		})
	}
}

func samplePathForRoute(pattern string) string {
	replacer := strings.NewReplacer(
		":id", "999999",
		":tokenID", "999999",
		":release", "v1.0.0",
		":session_id", "session-1",
		":seq", "0",
		":name", "service",
		":svc", "api",
		":traceID", "0123456789abcdef",
		":event_id", "evt-1",
	)
	return replacer.Replace(pattern)
}

func TestFullRoutePermissionMatrix(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()

	// Bootstrap must run on an empty store so it creates the "default" project
	// and the generated bootstrap admin flagged with must_change_password=true.
	bootstrapPassword := bootstrapGeneratedAdmin(t, store)
	flaggedID := mustUserID(t, store, meta.BootstrapAdminUsername)
	_, flaggedToken, err := store.CreateUserAPIToken(ctx, flaggedID, "matrix-flagged-tok")
	if err != nil {
		t.Fatal(err)
	}

	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	projectToken, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, uploadToken, err := store.CreateSourceMapUploadToken(ctx, project.ID, "matrix-ci")
	if err != nil {
		t.Fatal(err)
	}

	viewerCookie := sessionCookie(t, store, "matrix-viewer", "password-123", "viewer")
	memberCookie := sessionCookie(t, store, "matrix-member", "password-123", "member")
	adminCookie := sessionCookie(t, store, "matrix-admin", "password-123", "admin")

	viewerID := mustUserID(t, store, "matrix-viewer")
	memberID := mustUserID(t, store, "matrix-member")
	adminID := mustUserID(t, store, "matrix-admin")

	_, viewerToken, err := store.CreateUserAPIToken(ctx, viewerID, "matrix-viewer-tok")
	if err != nil {
		t.Fatal(err)
	}
	_, memberToken, err := store.CreateUserAPIToken(ctx, memberID, "matrix-member-tok")
	if err != nil {
		t.Fatal(err)
	}
	_, adminToken, err := store.CreateUserAPIToken(ctx, adminID, "matrix-admin-tok")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{
		Store:       store,
		Tokens:      store,
		Management:  management.New(store),
		Blobs:       blobs,
		Metrics:     upstream.URL,
		Logs:        upstream.URL,
		Traces:      upstream.URL,
		RequireCSRF: true,
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	// Log in as flagged bootstrap admin to get a real session + CSRF cookie pair.
	loginReq := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		strings.NewReader(`{"username":"admin","password":"`+bootstrapPassword+`"}`),
	)
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := app.Test(loginReq)
	if err != nil || loginResp.StatusCode != http.StatusOK {
		t.Fatalf("flagged admin login status=%d err=%v", loginResp.StatusCode, err)
	}
	var flaggedCookie, flaggedCSRFCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		switch c.Name {
		case "lo_session":
			flaggedCookie = c
		case csrfCookieName:
			flaggedCSRFCookie = c
		}
	}
	_ = loginResp.Body.Close()
	if flaggedCookie == nil || flaggedCSRFCookie == nil {
		t.Fatal("flagged admin login did not set session and csrf cookies")
	}

	doReq := func(method, target string, customize func(*http.Request)) (int, string, string) {
		t.Helper()
		var body io.Reader
		if isMutatingMethod(method) {
			body = bytes.NewReader([]byte(`{}`))
		}
		req := httptest.NewRequest(method, target, body)
		if isMutatingMethod(method) {
			req.Header.Set("Content-Type", "application/json")
		}
		if customize != nil {
			customize(req)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s %s failed: %v", method, target, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var parsed struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &parsed)
		return resp.StatusCode, parsed.Code, string(raw)
	}

	withSession := func(cookie *http.Cookie, withCSRF bool, projectSlug string) func(*http.Request) {
		return func(req *http.Request) {
			req.AddCookie(cookie)
			if withCSRF {
				req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "matrix-csrf"})
				req.Header.Set(csrfHeaderName, "matrix-csrf")
			} else {
				req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "matrix-csrf"})
			}
			if projectSlug != "" {
				req.Header.Set("X-LO-Project", projectSlug)
			}
		}
	}

	withBearer := func(token, projectSlug string) func(*http.Request) {
		return func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
			if projectSlug != "" {
				req.Header.Set("X-LO-Project", projectSlug)
			}
		}
	}

	assertDenied := func(t *testing.T, label string, status int, code, body string, wantStatus int, wantCode string) {
		t.Helper()
		if status != wantStatus {
			t.Errorf("%s: status=%d want=%d body=%s", label, status, wantStatus, body)
			return
		}
		if wantCode != "" && code != wantCode {
			t.Errorf("%s: code=%q want=%q body=%s", label, code, wantCode, body)
		}
	}

	assertPassedPolicy := func(t *testing.T, label string, status int, code, body string) {
		t.Helper()
		if status == http.StatusUnauthorized {
			t.Errorf("%s: unexpectedly rejected with 401 body=%s", label, body)
		}
		if status == http.StatusForbidden &&
			(code == "csrf_failed" ||
				code == "password_change_required" ||
				code == "ingest_only_token" ||
				code == "upload_only_token" ||
				code == "source_map_upload_token_required" ||
				strings.Contains(body, `"forbidden"`)) {
			t.Errorf("%s: unexpectedly rejected by route policy with 403 code=%q body=%s", label, code, body)
		}
	}

	for _, reg := range gw.registeredRoutes {
		method := reg.method
		if method == routeMethodAll {
			method = fiber.MethodGet
		}
		target := samplePathForRoute(reg.path)
		policy := reg.policy
		// Skip logout for the shared flagged/admin session mutation or use a dedicated disposable session.
		t.Run(reg.method+" "+reg.path, func(t *testing.T) {
			// 1. Anonymous request
			status, code, body := doReq(method, target, nil)
			if policy.auth == authPublic {
				if reg.path != "/api/auth/login" {
					assertPassedPolicy(t, "anonymous", status, code, body)
				}
				return
			}
			assertDenied(t, "anonymous", status, code, body, http.StatusUnauthorized, "")

			// 2. Project API token (lo_...)
			status, code, body = doReq(method, target, withBearer(projectToken, "default"))
			switch policy.auth {
			case authProjectToken:
				assertPassedPolicy(t, "project token", status, code, body)
			case authUploadToken:
				assertDenied(
					t,
					"project token",
					status,
					code,
					body,
					http.StatusForbidden,
					"source_map_upload_token_required",
				)
			case authSessionOnly:
				assertDenied(t, "project token", status, code, body, http.StatusUnauthorized, "")
			case authUser, authUserTokenOnly:
				assertDenied(t, "project token", status, code, body, http.StatusForbidden, "ingest_only_token")
			}

			// 3. Source Map upload token (los_...)
			status, code, body = doReq(method, target, withBearer(uploadToken, "default"))
			switch policy.auth {
			case authUploadToken:
				assertPassedPolicy(t, "upload token", status, code, body)
			case authSessionOnly:
				assertDenied(t, "upload token", status, code, body, http.StatusUnauthorized, "")
			case authProjectToken, authUser, authUserTokenOnly:
				assertDenied(t, "upload token", status, code, body, http.StatusForbidden, "upload_only_token")
			}

			// 4. User API token (lou_...) across viewer / member / admin
			userTokens := []struct {
				role  string
				token string
			}{
				{"viewer", viewerToken},
				{"member", memberToken},
				{"admin", adminToken},
			}
			for _, ut := range userTokens {
				status, code, body = doReq(method, target, withBearer(ut.token, "default"))
				switch policy.auth {
				case authProjectToken:
					assertDenied(t, "user token "+ut.role, status, code, body, http.StatusUnauthorized, "")
				case authUploadToken:
					assertDenied(
						t,
						"user token "+ut.role,
						status,
						code,
						body,
						http.StatusForbidden,
						"source_map_upload_token_required",
					)
				case authSessionOnly:
					assertDenied(t, "user token "+ut.role, status, code, body, http.StatusUnauthorized, "")
				case authUser, authUserTokenOnly:
					if !roleSatisfies(ut.role, policy.role) {
						assertDenied(t, "user token "+ut.role, status, code, body, http.StatusForbidden, "")
					} else {
						assertPassedPolicy(t, "user token "+ut.role, status, code, body)
					}
				}
			}

			// 5. Session cookie across viewer / member / admin (with & without CSRF)
			// For /api/auth/logout, mint a fresh disposable session so shared cookies stay valid.
			freshSession := func(role string) *http.Cookie {
				if reg.path == "/api/auth/logout" {
					uID := map[string]int{"viewer": viewerID, "member": memberID, "admin": adminID}[role]
					tok := metadataSession(t, store, uID, gw.now(), gw.now().Add(sessionTTL))
					return &http.Cookie{Name: "lo_session", Value: tok}
				}
				return map[string]*http.Cookie{
					"viewer": viewerCookie,
					"member": memberCookie,
					"admin":  adminCookie,
				}[role]
			}

			if policy.auth == authUserTokenOnly {
				status, code, body = doReq(method, target, withSession(adminCookie, true, "default"))
				assertDenied(t, "session on user-token-only", status, code, body, http.StatusUnauthorized, "")
			}

			if policy.auth.supportsSessionCookie() {
				if policy.csrf && isMutatingMethod(method) {
					status, code, body = doReq(method, target, withSession(freshSession("admin"), false, "default"))
					assertDenied(t, "missing CSRF", status, code, body, http.StatusForbidden, "csrf_failed")
				}
				for _, role := range []string{"viewer", "member", "admin"} {
					if reg.path == "/api/auth/password" && role == "admin" {
						// Do not mutate shared admin password; use empty body which fails validation after auth/CSRF.
						status, code, body = doReq(method, target, withSession(freshSession(role), true, "default"))
					} else {
						status, code, body = doReq(method, target, withSession(freshSession(role), true, "default"))
					}
					if !roleSatisfies(role, policy.role) {
						assertDenied(t, "session "+role, status, code, body, http.StatusForbidden, "")
					} else {
						assertPassedPolicy(t, "session "+role, status, code, body)
					}
				}
			}

			// 6. must_change_password gate
			if policy.auth.isUserBearing() {
				if policy.auth.supportsSessionCookie() {
					cookie := flaggedCookie
					if reg.path == "/api/auth/logout" {
						tok := metadataSession(t, store, flaggedID, gw.now(), gw.now().Add(sessionTTL))
						cookie = &http.Cookie{Name: "lo_session", Value: tok}
					}
					status, code, body = doReq(method, target, func(req *http.Request) {
						req.AddCookie(cookie)
						req.AddCookie(flaggedCSRFCookie)
						req.Header.Set(csrfHeaderName, flaggedCSRFCookie.Value)
						req.Header.Set("X-LO-Project", "default")
					})
					if policy.allowWhenMustChange {
						assertPassedPolicy(t, "must_change session allowed", status, code, body)
					} else {
						assertDenied(
							t,
							"must_change session blocked",
							status,
							code,
							body,
							http.StatusForbidden,
							"password_change_required",
						)
					}
				}
				if policy.auth == authUser || policy.auth == authUserTokenOnly {
					status, code, body = doReq(method, target, withBearer(flaggedToken, "default"))
					if policy.allowWhenMustChange {
						assertPassedPolicy(t, "must_change token allowed", status, code, body)
					} else {
						assertDenied(
							t,
							"must_change token blocked",
							status,
							code,
							body,
							http.StatusForbidden,
							"password_change_required",
						)
					}
				}
			}

			// 7. Project context fail-closed boundary (unknown project -> 404 project_not_found)
			if policy.projectScope {
				status, code, body = doReq(
					method,
					target,
					withBearer(adminToken, "does-not-exist-"+strconv.Itoa(project.ID)),
				)
				assertDenied(t, "unknown project", status, code, body, http.StatusNotFound, "project_not_found")
			}
		})
	}
}
