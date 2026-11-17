package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
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
