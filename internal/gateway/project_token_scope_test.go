package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Project API tokens are ingest credentials (requirements §2.7): browser SDKs
// embed them in public pages, so they must never authorize data reads.
func TestProjectTokenCannotQuery(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(t.Context(), "Public", "public")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("project token query reached storage: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Metrics: upstream.URL, Logs: upstream.URL, Traces: upstream.URL,
	}).App()
	for _, test := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/logs/query?query=*"},
		{http.MethodGet, "/api/v1/query?query=up"},
		{http.MethodGet, "/api/v1/performance/vitals"},
		{http.MethodGet, "/api/traces/services"},
		{http.MethodGet, "/api/v1/issues"},
		{http.MethodGet, "/api/replay/sessions"},
		{http.MethodGet, "/api/profiles"},
		{http.MethodGet, "/api/system/info"},
		{http.MethodPost, "/mcp"},
		// Session API group (userRequired): reads must refuse project tokens
		// with the same 403 ingest_only_token as the telemetry query routes.
		{http.MethodGet, "/api/issues"},
		{http.MethodGet, "/api/issues/1"},
		{http.MethodGet, "/api/issues/1/events/1"},
		{http.MethodGet, "/api/alerts"},
		{http.MethodGet, "/api/hosts"},
		{http.MethodGet, "/api/scrape-targets"},
		{http.MethodGet, "/api/projects"},
		{http.MethodGet, "/api/auth/me"},
		{http.MethodGet, "/api/audit"},
		{http.MethodGet, "/api/system/error-inbox"},
	} {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, bytes.NewReader(nil))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			body := readBody(resp)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status=%d want=403 body=%s", resp.StatusCode, body)
			}
			var payload struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode body %q: %v", body, err)
			}
			if payload.Code != "ingest_only_token" {
				t.Fatalf("code=%q want=ingest_only_token body=%s", payload.Code, body)
			}
		})
	}
}
