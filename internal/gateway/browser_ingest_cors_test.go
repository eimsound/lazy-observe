package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserIngestCORSIsLimitedToBrowserWriteEndpoints(t *testing.T) {
	t.Parallel()

	app := newTestGateway(Config{}).App()
	for _, path := range []string{
		"/v1/traces",
		"/api/events/errors",
		"/api/events/vitals",
		"/api/events/diagnostics",
		"/api/replay/segments",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(http.MethodOptions, path, nil)
			request.Header.Set("Origin", "https://example.test")
			request.Header.Set("Access-Control-Request-Method", http.MethodPost)
			request.Header.Set(
				"Access-Control-Request-Headers",
				"authorization,content-type,x-lo-replay-session,x-lo-replay-seq,x-lo-replay-error",
			)
			response, err := app.Test(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status=%d want=%d", response.StatusCode, http.StatusNoContent)
			}
			if got := response.Header.Get("Access-Control-Allow-Origin"); got != "*" {
				t.Fatalf("allow origin=%q want=%q", got, "*")
			}
			if got := response.Header.Get("Access-Control-Allow-Methods"); got != http.MethodPost {
				t.Fatalf("allow methods=%q want=%q", got, http.MethodPost)
			}
			if got := response.Header.Get("Access-Control-Expose-Headers"); got != "Retry-After" {
				t.Fatalf("expose headers=%q want Retry-After", got)
			}
			if got := response.Header.Get("Access-Control-Max-Age"); got != "600" {
				t.Fatalf("max age=%q want=%q", got, "600")
			}
			allowed := strings.ToLower(response.Header.Get("Access-Control-Allow-Headers"))
			for _, header := range []string{
				"authorization",
				"content-type",
				"content-encoding",
				"x-lo-replay-session",
				"x-lo-replay-seq",
				"x-lo-replay-compression",
				"x-lo-replay-error",
			} {
				if !strings.Contains(allowed, header) {
					t.Fatalf("allow headers=%q missing %q", allowed, header)
				}
			}
		})
	}

	request := httptest.NewRequest(http.MethodOptions, "/api/auth/me", nil)
	request.Header.Set("Origin", "https://example.test")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("management route unexpectedly allows cross-origin access: %q", got)
	}
}

func TestBrowserIngestPOSTIncludesCORSHeadersOnAuthFailure(t *testing.T) {
	t.Parallel()

	app := newTestGateway(Config{}).App()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/events/errors",
		strings.NewReader(`{"events":[]}`),
	)
	request.Header.Set("Origin", "https://example.test")
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", response.StatusCode, http.StatusUnauthorized)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow origin=%q want=%q", got, "*")
	}
}
