package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eimsound/lazy-observe/internal/management"
)

func TestUserAPITokenAPIAndBearerAuthentication(t *testing.T) {
	store := testStore(t)
	cookie := sessionCookie(t, store, "api-user", "password", "member")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()

	request := func(method, target, body string, session *http.Cookie, bearer string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if session != nil {
			req.AddCookie(session)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := request(http.MethodPost, "/api/auth/tokens", `{"name":"local-cli"}`, cookie, "")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, body)
	}
	var issued issuedTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
		t.Fatal(err)
	}
	if issued.ID == "" || issued.Name != "local-cli" || issued.Prefix == "" || issued.Value == "" {
		t.Fatalf("issued=%#v", issued)
	}

	resp = request(http.MethodGet, "/api/auth/tokens", "", cookie, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", resp.StatusCode)
	}
	var listed []tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil || len(listed) != 1 || listed[0].ID != issued.ID {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}

	resp = request(http.MethodGet, "/api/auth/me", "", nil, issued.Value)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer me status=%d", resp.StatusCode)
	}
	resp = request(http.MethodGet, "/api/projects", "", nil, issued.Value)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer management status=%d", resp.StatusCode)
	}

	resp = request(http.MethodDelete, "/api/auth/tokens/"+issued.ID, "", cookie, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status=%d", resp.StatusCode)
	}
	resp = request(http.MethodGet, "/api/auth/me", "", nil, issued.Value)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked bearer status=%d", resp.StatusCode)
	}
}

func TestProjectAPITokenCannotAuthenticateUserAPI(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(t.Context(), "Project", "project")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store}).App()
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project bearer status=%d err=%v", resp.StatusCode, err)
	}
}

func TestUserAPITokenQueriesSelectedProject(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(t.Context(), "Selected", "selected")
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.CreateUser(t.Context(), "query-user", "password", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateUserAPIToken(t.Context(), u.ID, "query-cli")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("ProjectID"); got != strconvI(project.ID) {
			t.Errorf("ProjectID=%q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL}).App()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("query status=%d err=%v", resp.StatusCode, err)
	}
}
