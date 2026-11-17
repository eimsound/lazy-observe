package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eimsound/lazy-observe/internal/management"
)

func TestProjectAndAPITokenAPIContract(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "project-admin", "password", "admin")
	viewer := sessionCookie(t, store, "project-viewer", "password", "viewer")
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()

	request := func(method, path, body string, cookie *http.Cookie) *http.Response {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	status := func(resp *http.Response, want int) []byte {
		t.Helper()
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, want, body)
		}
		return body
	}

	status(request(http.MethodPost, "/api/projects", `{"name":"Denied","slug":"denied"}`, viewer), http.StatusForbidden)
	createdBody := status(
		request(http.MethodPost, "/api/projects", `{"name":"Contract","slug":"contract"}`, admin),
		http.StatusOK,
	)
	var created map[string]any
	if err := json.Unmarshal(createdBody, &created); err != nil {
		t.Fatal(err)
	}
	projectID, ok := created["id"].(string)
	if !ok || projectID == "" || created["name"] != "Contract" || created["slug"] != "contract" ||
		created["created_at"] == nil {
		t.Fatalf("unexpected project DTO: %#v", created)
	}
	if _, leaked := created["edges"]; leaked {
		t.Fatalf("project DTO leaked ORM edges: %#v", created)
	}

	status(
		request(http.MethodPatch, "/api/projects/"+projectID, `{"name":"Viewer edit"}`, viewer),
		http.StatusForbidden,
	)
	updatedBody := status(
		request(http.MethodPatch, "/api/projects/"+projectID, `{"name":"Contract updated"}`, admin),
		http.StatusOK,
	)
	var updated projectResponse
	if err := json.Unmarshal(updatedBody, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ID != projectID || updated.Name != "Contract updated" || updated.Slug != "contract" {
		t.Fatalf("unexpected updated project: %#v", updated)
	}
	status(request(http.MethodPatch, "/api/projects/"+projectID, `{"slug":"renamed"}`, admin), http.StatusConflict)
	unchangedBody := status(request(http.MethodGet, "/api/projects/"+projectID, "", admin), http.StatusOK)
	var unchanged projectResponse
	if err := json.Unmarshal(unchangedBody, &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Slug != "contract" {
		t.Fatalf("immutable slug changed to %q", unchanged.Slug)
	}

	status(
		request(http.MethodPost, "/api/projects/"+projectID+"/tokens", `{"name":"viewer-token"}`, viewer),
		http.StatusForbidden,
	)
	status(request(http.MethodPost, "/api/projects/"+projectID+"/tokens", `{}`, admin), http.StatusBadRequest)
	issuedBody := status(
		request(http.MethodPost, "/api/projects/"+projectID+"/tokens", `{"name":"ci-agent"}`, admin),
		http.StatusCreated,
	)
	var issued struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		Prefix     string  `json:"prefix"`
		CreatedAt  string  `json:"created_at"`
		LastUsedAt *string `json:"last_used_at"`
		Revoked    bool    `json:"revoked"`
		Value      string  `json:"value"`
	}
	if err := json.Unmarshal(issuedBody, &issued); err != nil {
		t.Fatal(err)
	}
	if issued.ID == "" || issued.Name != "ci-agent" || issued.Prefix == "" || issued.CreatedAt == "" ||
		issued.LastUsedAt != nil ||
		issued.Revoked ||
		!strings.HasPrefix(issued.Value, issued.Prefix) {
		t.Fatalf("unexpected issued token DTO: %#v", issued)
	}

	assertTokenList := func(wantRevoked, wantUsed bool) {
		t.Helper()
		body := status(request(http.MethodGet, "/api/projects/"+projectID+"/tokens", "", viewer), http.StatusOK)
		var tokens []map[string]any
		if err := json.Unmarshal(body, &tokens); err != nil {
			t.Fatal(err)
		}
		if len(tokens) != 1 {
			t.Fatalf("tokens=%#v", tokens)
		}
		token := tokens[0]
		for _, field := range []string{"id", "name", "prefix", "created_at", "last_used_at", "revoked"} {
			if _, exists := token[field]; !exists {
				t.Fatalf("token field %q missing from %#v", field, token)
			}
		}
		if token["id"] != issued.ID || token["name"] != "ci-agent" || token["prefix"] != issued.Prefix ||
			token["revoked"] != wantRevoked {
			t.Fatalf("unexpected token list DTO: %#v", token)
		}
		if (token["last_used_at"] != nil) != wantUsed {
			t.Fatalf("last_used_at=%#v wantUsed=%t", token["last_used_at"], wantUsed)
		}
		if _, leaked := token["value"]; leaked {
			t.Fatalf("list leaked one-time secret: %#v", token)
		}
		if _, leaked := token["token"]; leaked {
			t.Fatalf("list leaked stored secret: %#v", token)
		}
	}

	assertTokenList(false, false)
	if _, err := store.AuthenticateProjectAPIToken(context.Background(), issued.Value); err != nil {
		t.Fatal(err)
	}
	assertTokenList(false, true)

	status(
		request(http.MethodDelete, "/api/projects/"+projectID+"/tokens/"+issued.ID, "", viewer),
		http.StatusForbidden,
	)
	status(request(http.MethodDelete, "/api/projects/"+projectID+"/tokens/"+issued.ID, "", admin), http.StatusNoContent)
	assertTokenList(true, true)
	if _, err := store.AuthenticateProjectAPIToken(context.Background(), issued.Value); err == nil {
		t.Fatal("revoked token still authenticates")
	}

	status(request(http.MethodDelete, "/api/projects/"+projectID, "", nil), http.StatusUnauthorized)
	status(request(http.MethodDelete, "/api/projects/"+projectID, "", viewer), http.StatusForbidden)
	status(request(http.MethodDelete, "/api/projects/"+projectID, "", admin), http.StatusNoContent)
	status(request(http.MethodGet, "/api/projects/"+projectID, "", admin), http.StatusNotFound)

	self, _, err := store.BootstrapSelfMonitoring(context.Background(), "lo_test-self-monitoring-derived-token-value")
	if err != nil {
		t.Fatal(err)
	}
	status(request(http.MethodDelete, "/api/projects/"+strconvI(self.ID), "", admin), http.StatusForbidden)
}

func TestTokenCreateRejectsMalformedJSON(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "malformed-admin", "password", "admin")
	project, err := store.CreateProject(context.Background(), "Malformed", "malformed")
	if err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/projects/"+strconvI(project.ID)+"/tokens",
		bytes.NewBufferString("{"),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestProjectMutationErrorPrecedence(t *testing.T) {
	store := testStore(t)
	admin := sessionCookie(t, store, "precedence-admin", "password", "admin")
	self, _, err := store.BootstrapSelfMonitoring(context.Background(), "lo_test-self-monitoring-derived-token-value")
	if err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Management: management.New(store)}).App()

	request := func(method, target, body string) int {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.AddCookie(admin)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	missing := "999999"
	selfID := strconvI(self.ID)
	for _, test := range []struct {
		name, method, target, body string
		want                       int
	}{
		{"missing project malformed update", http.MethodPatch, "/api/projects/" + missing, "{", http.StatusNotFound},
		{"protected project malformed update", http.MethodPatch, "/api/projects/" + selfID, "{", http.StatusForbidden},
		{"missing project malformed token", http.MethodPost, "/api/projects/" + missing + "/tokens", "{", http.StatusNotFound},
		{"protected project malformed token", http.MethodPost, "/api/projects/" + selfID + "/tokens", "{", http.StatusForbidden},
		{"missing project invalid token id", http.MethodDelete, "/api/projects/" + missing + "/tokens/nope", "", http.StatusNotFound},
		{"protected project invalid token id", http.MethodDelete, "/api/projects/" + selfID + "/tokens/nope", "", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := request(test.method, test.target, test.body); got != test.want {
				t.Fatalf("status=%d want=%d", got, test.want)
			}
		})
	}
}
