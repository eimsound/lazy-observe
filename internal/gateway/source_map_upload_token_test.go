package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
)

const testSourceMap = `{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA"}`

func createUploadTokenTest(t *testing.T, store *meta.Store, projectID int) string {
	t.Helper()
	_, value, err := store.CreateSourceMapUploadToken(context.Background(), projectID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func uploadStatus(t *testing.T, app *Gateway, token, release string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("app.min.js", "app.min.js.map")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(testSourceMap))
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/releases/"+release+"/sourcemaps", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(readBody(resp))
}

func newUploadTokenGateway(t *testing.T) (*Gateway, *meta.Store) {
	t.Helper()
	store := testStore(t)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Blobs: blobs})
	t.Cleanup(func() { _ = gw.Shutdown() })
	return gw, store
}

func responseCode(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return payload.Code
}

// ADR 0018: the project API token is public, so it must not upload source maps; a
// user API token is not an upload credential either.
func TestSourceMapUploadRejectsProjectAndUserTokens(t *testing.T) {
	gw, store := newUploadTokenGateway(t)
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Public", "public")
	projectToken, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	userToken := userQueryToken(t, store, "uploader")
	for name, token := range map[string]string{"project": projectToken, "user": userToken} {
		status, body := uploadStatus(t, gw, token, "r1")
		if status != http.StatusForbidden || responseCode(t, body) != "source_map_upload_token_required" {
			t.Fatalf("%s token upload status=%d body=%s", name, status, body)
		}
	}
	if status, _ := uploadStatus(t, gw, "", "r1"); status != http.StatusUnauthorized {
		t.Fatalf("anonymous upload status=%d want 401", status)
	}
	if status, _ := uploadStatus(t, gw, "los_"+strings.Repeat("0", 48), "r1"); status != http.StatusUnauthorized {
		t.Fatalf("unknown los_ upload status=%d want 401", status)
	}
	artifacts, err := store.SourceMapArtifacts(ctx, project.ID, "r1")
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("rejected uploads stored artifacts=%v err=%v", artifacts, err)
	}
}

func TestSourceMapUploadTokenUploadsToItsProject(t *testing.T) {
	gw, store := newUploadTokenGateway(t)
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Site", "site")
	token := createUploadTokenTest(t, store, project.ID)
	if !strings.HasPrefix(token, "los_") {
		t.Fatalf("token %q lacks los_ prefix", token)
	}
	if status, body := uploadStatus(t, gw, token, "r1"); status != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	artifacts, err := store.SourceMapArtifacts(ctx, project.ID, "r1")
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts=%v err=%v", artifacts, err)
	}
}

func TestSourceMapUploadTokenCannotCrossProjects(t *testing.T) {
	gw, store := newUploadTokenGateway(t)
	ctx := context.Background()
	a, _ := store.CreateProject(ctx, "A", "a")
	b, _ := store.CreateProject(ctx, "B", "b")
	tokenA := createUploadTokenTest(t, store, a.ID)
	if status, body := uploadStatus(t, gw, tokenA, "r1"); status != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	if got, err := store.SourceMapArtifacts(ctx, b.ID, "r1"); err != nil || len(got) != 0 {
		t.Fatalf("project A token wrote to project B: %v err=%v", got, err)
	}
	if got, err := store.SourceMapArtifacts(ctx, a.ID, "r1"); err != nil || len(got) != 1 {
		t.Fatalf("project A artifacts=%v err=%v", got, err)
	}
	// Revoking project B's token id through project A's path must not touch it.
	infoB, _, err := store.CreateSourceMapUploadToken(ctx, b.ID, "b-ci")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeSourceMapUploadTokenByID(ctx, a.ID, infoB.ID); !meta.IsNotFound(err) {
		t.Fatalf("cross-project revoke err=%v want not found", err)
	}
}

func TestSourceMapUploadTokenRevocationTakesEffect(t *testing.T) {
	gw, store := newUploadTokenGateway(t)
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Site", "site")
	info, token, err := store.CreateSourceMapUploadToken(ctx, project.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := uploadStatus(t, gw, token, "r1"); status != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	if err := store.RevokeSourceMapUploadTokenByID(ctx, project.ID, info.ID); err != nil {
		t.Fatal(err)
	}
	// The authenticator cache is keyed by the store revision, which revocation bumps.
	if status, _ := uploadStatus(t, gw, token, "r2"); status != http.StatusUnauthorized {
		t.Fatalf("revoked token upload status=%d want 401", status)
	}
}

func TestSourceMapUploadTokenUpdatesLastUsedAt(t *testing.T) {
	gw, store := newUploadTokenGateway(t)
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Site", "site")
	_, token, err := store.CreateSourceMapUploadToken(ctx, project.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.ListSourceMapUploadTokens(ctx, project.ID)
	if len(before) != 1 || before[0].LastUsedAt != nil {
		t.Fatalf("fresh token = %+v", before)
	}
	start := time.Now().Add(-time.Second)
	if status, body := uploadStatus(t, gw, token, "r1"); status != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	after, _ := store.ListSourceMapUploadTokens(ctx, project.ID)
	if len(after) != 1 || after[0].LastUsedAt == nil || after[0].LastUsedAt.Before(start) {
		t.Fatalf("last_used_at not updated: %+v", after)
	}
}

// An upload token can only upload: ingest, query and management routes refuse it
// with 403 upload_only_token.
func TestSourceMapUploadTokenIsRefusedEverywhereElse(t *testing.T) {
	gw, store := newUploadTokenGateway(t)
	project, _ := store.CreateProject(context.Background(), "Site", "site")
	token := createUploadTokenTest(t, store, project.ID)
	for _, test := range []struct{ method, target string }{
		// Ingest.
		{http.MethodPost, "/v1/metrics"},
		{http.MethodPost, "/v1/logs"},
		{http.MethodPost, "/v1/traces"},
		{http.MethodPost, "/api/v1/write"},
		{http.MethodPost, "/api/events/errors"},
		{http.MethodPost, "/api/events/vitals"},
		{http.MethodPost, "/api/events/diagnostics"},
		{http.MethodPost, "/api/replay/segments"},
		{http.MethodPost, "/api/profiles"},
		// Query.
		{http.MethodGet, "/api/v1/issues"},
		{http.MethodGet, "/api/releases"},
		{http.MethodGet, "/api/profiles/services"},
		{http.MethodGet, "/api/system/info"},
		{http.MethodPost, "/mcp"},
		// Session API: reads, project management and token management.
		{http.MethodGet, "/api/projects"},
		{http.MethodGet, "/api/auth/me"},
		{http.MethodPost, "/api/projects/1/tokens"},
		{http.MethodPost, "/api/projects/1/source-map-upload-tokens"},
		{http.MethodGet, "/api/projects/1/source-map-upload-tokens"},
		{http.MethodDelete, "/api/releases/r1/sourcemaps"},
		{http.MethodGet, "/api/audit"},
	} {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, bytes.NewReader(nil))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := gw.App().Test(req)
			if err != nil {
				t.Fatal(err)
			}
			body := string(readBody(resp))
			if resp.StatusCode != http.StatusForbidden || responseCode(t, body) != "upload_only_token" {
				t.Fatalf("status=%d body=%s want 403 upload_only_token", resp.StatusCode, body)
			}
		})
	}
}

// A project API token can no longer be exchanged for an upload credential through
// the ingest authenticator, and an upload token is not found in the ingest table.
func TestUploadTokenAndProjectTokenAreDistinctCredentials(t *testing.T) {
	_, store := newUploadTokenGateway(t)
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Site", "site")
	projectToken, _ := store.CreateProjectAPIToken(ctx, project.ID)
	uploadToken := createUploadTokenTest(t, store, project.ID)
	if _, err := store.AuthenticateProjectAPIToken(ctx, uploadToken); err == nil {
		t.Fatal("upload token authenticated as a project API token")
	}
	if _, err := store.AuthenticateSourceMapUploadToken(ctx, projectToken); err == nil {
		t.Fatal("project API token authenticated as an upload token")
	}
}

func TestSourceMapUploadTokenManagementAPIAndAudit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin := sessionCookie(t, store, "sm-admin", "password", "admin")
	member := sessionCookie(t, store, "sm-member", "password", "member")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Blobs: blobs, Management: management.New(store),
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	project, _ := store.CreateProject(ctx, "Site", "site")
	other, _ := store.CreateProject(ctx, "Other", "other")
	base := "/api/projects/" + strconv.Itoa(project.ID) + "/source-map-upload-tokens"

	do := func(method, path, body string, cookie *http.Cookie) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.AddCookie(cookie)
		resp, err := gw.App().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, readBody(resp)
	}

	if status, _ := do(http.MethodPost, base, `{"name":"ci"}`, member); status != http.StatusForbidden {
		t.Fatalf("member create status=%d want 403", status)
	}
	if status, _ := do(http.MethodGet, base, "", member); status != http.StatusForbidden {
		t.Fatalf("member list status=%d want 403", status)
	}
	if status, _ := do(http.MethodPost, base, `{}`, admin); status != http.StatusBadRequest {
		t.Fatalf("nameless create status=%d want 400", status)
	}
	status, body := do(http.MethodPost, base, `{"name":"github-actions"}`, admin)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", status, body)
	}
	var issued struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
		Value  string `json:"value"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Value, "los_") || !strings.HasPrefix(issued.Value, issued.Prefix) ||
		issued.Name != "github-actions" {
		t.Fatalf("issued token = %+v", issued)
	}
	// The value is shown once: listings carry only the prefix.
	status, body = do(http.MethodGet, base, "", admin)
	if status != http.StatusOK || strings.Contains(string(body), issued.Value) ||
		!strings.Contains(string(body), issued.Prefix) {
		t.Fatalf("list status=%d body=%s", status, body)
	}
	if code, _ := uploadStatus(t, gw, issued.Value, "r1"); code != http.StatusCreated {
		t.Fatalf("issued token cannot upload: %d", code)
	}

	// A token id from another project is not revocable through this project's path.
	otherInfo, otherValue, err := store.CreateSourceMapUploadToken(ctx, other.ID, "other-ci")
	if err != nil {
		t.Fatal(err)
	}
	crossPath := base + "/" + strconv.Itoa(otherInfo.ID)
	if status, _ := do(http.MethodDelete, crossPath, "", admin); status != http.StatusNotFound {
		t.Fatalf("cross-project revoke status=%d want 404", status)
	}
	if _, err := store.AuthenticateSourceMapUploadToken(ctx, otherValue); err != nil {
		t.Fatalf("cross-project revoke disabled the other project's token: %v", err)
	}

	if status, _ := do(http.MethodDelete, base+"/"+issued.ID, "", member); status != http.StatusForbidden {
		t.Fatalf("member revoke status=%d want 403", status)
	}
	if status, _ := do(http.MethodDelete, base+"/"+issued.ID, "", admin); status != http.StatusNoContent {
		t.Fatalf("revoke status=%d want 204", status)
	}
	if code, _ := uploadStatus(t, gw, issued.Value, "r2"); code != http.StatusUnauthorized {
		t.Fatalf("revoked token upload status=%d want 401", code)
	}

	records, err := store.ListAudit(ctx, meta.AuditListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var created, revoked bool
	for _, record := range records {
		if record.TargetType != "source_map_upload_token" {
			continue
		}
		switch {
		case record.Action == "source_map_upload_token.create" && record.Success:
			created = record.TargetName == "github-actions" && record.ProjectSlug == "site" && record.TargetID > 0
		case record.Action == "source_map_upload_token.revoke" && record.Success:
			revoked = record.ProjectSlug == "site" && strconv.Itoa(record.TargetID) == issued.ID
		}
	}
	if !created || !revoked {
		t.Fatalf("audit created=%t revoked=%t records=%+v", created, revoked, records)
	}
	persisted, _ := json.Marshal(records)
	if strings.Contains(string(persisted), issued.Value) {
		t.Fatal("audit log leaked the upload token value")
	}
}
