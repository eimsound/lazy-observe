package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
	pprofprofile "github.com/google/pprof/profile"
)

func TestProfileIngestRejectsInvalidPprof(t *testing.T) {
	app, token, _, _ := profileTestApp(t)
	var invalid bytes.Buffer
	zw := gzip.NewWriter(&invalid)
	_, _ = zw.Write([]byte("not a pprof"))
	_ = zw.Close()
	resp := uploadProfileFixture(t, app, token, "svc", "cpu", invalid.Bytes(), http.StatusUnprocessableEntity)
	resp.Body.Close()
}

func TestFlamegraphKnownFunctionsMergeAndTruncation(t *testing.T) {
	p1 := syntheticProfile(7, "main.work", "main.main")
	p2 := syntheticProfile(5, "main.work", "main.main")
	merged, err := pprofprofile.Merge([]*pprofprofile.Profile{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	tree := buildFlamegraph(merged, meta.ProfileTypeCPU, 64, 10_000)
	if tree.Value != 12 || findFlameValue(tree, "main.work") != 12 || findFlameValue(tree, "main.main") != 12 {
		t.Fatalf("merged tree values: %#v", tree)
	}

	depthCut := buildFlamegraph(p1, meta.ProfileTypeCPU, 1, 10_000)
	if len(depthCut.Children) != 1 || !depthCut.Children[0].Truncated {
		t.Fatalf("depth truncation missing: %#v", depthCut)
	}
	nodeCut := buildFlamegraph(p1, meta.ProfileTypeCPU, 64, 1)
	if !nodeCut.Truncated || len(nodeCut.Children) != 0 || nodeCut.Value != 7 {
		t.Fatalf("node truncation missing: %#v", nodeCut)
	}
}

func TestProfileAPIResponseContract(t *testing.T) {
	app, token, _, store := profileTestApp(t)
	reader := userQueryToken(t, store, "profile-reader")
	data := writeProfile(t, syntheticProfile(11, "main.busyLoop", "main.main"))

	created := uploadProfileFixture(t, app, token, "example", "cpu", data, http.StatusCreated)
	createdBody := readBody(created)
	createdRecord := decodeProfileRecordContract(t, createdBody)
	if createdRecord.ID <= 0 || createdRecord.Service != "example" || createdRecord.Type != "cpu" {
		t.Fatalf("created profile identity=%+v", createdRecord)
	}
	if createdRecord.Start != "2026-07-17T12:00:00Z" || createdRecord.End != "2026-07-17T12:00:10Z" {
		t.Fatalf("created profile window=%s..%s", createdRecord.Start, createdRecord.End)
	}
	if createdRecord.Size != int64(len(data)) || createdRecord.TraceID != "abc" {
		t.Fatalf("created profile size/trace=%d/%q", createdRecord.Size, createdRecord.TraceID)
	}

	servicesResp := profileGET(t, app, reader, "default", "/api/profiles/services")
	servicesBody := readBody(servicesResp)
	var servicesEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(servicesBody, &servicesEnvelope); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, servicesEnvelope, "services")
	var services []string
	if err := json.Unmarshal(servicesEnvelope["services"], &services); err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0] != "example" {
		t.Fatalf("services=%v", services)
	}

	profilesResp := profileGET(t, app, reader, "default", "/api/profiles?service=example&type=cpu")
	profilesBody := readBody(profilesResp)
	var profilesEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(profilesBody, &profilesEnvelope); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, profilesEnvelope, "profiles")
	var records []json.RawMessage
	if err := json.Unmarshal(profilesEnvelope["profiles"], &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("profiles=%s", profilesEnvelope["profiles"])
	}
	listedRecord := decodeProfileRecordContract(t, records[0])
	if listedRecord != createdRecord {
		t.Fatalf("listed profile=%+v, created=%+v", listedRecord, createdRecord)
	}
}

type profileRecordContract struct {
	ID      int
	Service string
	Type    string
	Start   string
	End     string
	Size    int64
	TraceID string
}

func decodeProfileRecordContract(t *testing.T, body []byte) profileRecordContract {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, object, "id", "service", "type", "start", "end", "size", "trace_id")
	var record profileRecordContract
	for key, target := range map[string]any{
		"id": &record.ID, "service": &record.Service, "type": &record.Type,
		"start": &record.Start, "end": &record.End, "size": &record.Size,
		"trace_id": &record.TraceID,
	} {
		if err := json.Unmarshal(object[key], target); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record.Start); err != nil {
		t.Fatalf("start is not RFC3339: %q: %v", record.Start, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.End); err != nil {
		t.Fatalf("end is not RFC3339: %q: %v", record.End, err)
	}
	return record
}

func assertJSONKeys(t *testing.T, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(object) != len(want) {
		t.Fatalf("JSON keys=%v want=%v", object, want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Fatalf("JSON response missing %q: %s", key, mustJSON(object))
		}
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestProfileAPIEndToEndAndProjectIsolation(t *testing.T) {
	app, defaultToken, p2Token, store := profileTestApp(t)
	reader := userQueryToken(t, store, "profile-reader")
	data := writeProfile(t, syntheticProfile(11, "main.busyLoop", "main.main"))
	for _, value := range []int64{11, 13} {
		data = writeProfile(t, syntheticProfile(value, "main.busyLoop", "main.main"))
		resp := uploadProfileFixture(t, app, defaultToken, "example", "cpu", data, http.StatusCreated)
		resp.Body.Close()
	}
	p2Data := writeProfile(t, syntheticProfile(99, "main.private", "main.main"))
	resp := uploadProfileFixture(t, app, p2Token, "private", "cpu", p2Data, http.StatusCreated)
	resp.Body.Close()

	list := profileGET(t, app, reader, "default", "/api/profiles?service=example&type=cpu")
	var listing struct {
		Profiles []struct {
			ID int `json:"id"`
		} `json:"profiles"`
	}
	decodeResponse(t, list, &listing)
	if len(listing.Profiles) != 2 {
		t.Fatalf("default listing count=%d", len(listing.Profiles))
	}
	viewer, err := store.CreateUser(context.Background(), "profile-viewer", "password", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	session := metadataSession(t, store, viewer.ID, time.Now(), time.Now().Add(time.Hour))
	sessionReq := httptest.NewRequest(http.MethodGet, "/api/profiles?service=example", nil)
	sessionReq.AddCookie(&http.Cookie{Name: "lo_session", Value: session})
	sessionResp, err := app.Test(sessionReq)
	if err != nil || sessionResp.StatusCode != http.StatusOK {
		t.Fatalf("session query status=%d err=%v", sessionResp.StatusCode, err)
	}
	sessionResp.Body.Close()
	p2List := profileGET(t, app, reader, "p2", "/api/profiles")
	p2Body := readBody(p2List)
	if bytes.Contains(p2Body, []byte("example")) || !bytes.Contains(p2Body, []byte("private")) {
		t.Fatalf("project isolation failed: %s", p2Body)
	}

	flame := profileGET(t, app, reader, "default", "/api/profiles/flamegraph?service=example&type=cpu")
	if flame.StatusCode != http.StatusOK {
		t.Fatalf("flame status=%d body=%s", flame.StatusCode, readBody(flame))
	}
	var tree flameNode
	decodeResponse(t, flame, &tree)
	if tree.Value != 24 || findFlameValue(&tree, "main.busyLoop") != 24 {
		t.Fatalf("flame tree=%+v", tree)
	}

	raw := profileGET(t, app, reader, "default", "/api/profiles/"+strconvI(listing.Profiles[0].ID)+"/raw")
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("raw status=%d body=%s", raw.StatusCode, readBody(raw))
	}
	parsed, err := pprofprofile.Parse(raw.Body)
	raw.Body.Close()
	if err != nil || len(parsed.Sample) != 1 {
		t.Fatalf("raw parse samples=%d err=%v", len(parsed.Sample), err)
	}

	empty := profileGET(t, app, reader, "default", "/api/profiles/flamegraph?service=missing&type=cpu")
	if empty.StatusCode != http.StatusNotFound {
		t.Fatalf("empty window status=%d", empty.StatusCode)
	}
}

func TestProfileUploadIDIsIdempotentAndProjectScoped(t *testing.T) {
	app, defaultToken, p2Token, store := profileTestApp(t)
	reader := userQueryToken(t, store, "profile-reader")
	data := writeProfile(t, syntheticProfile(11, "main.busyLoop", "main.main"))
	uploadID := strings.Repeat("a", 32)

	created := uploadProfileFixtureWithID(
		t,
		app,
		defaultToken,
		"example",
		"cpu",
		data,
		uploadID,
		http.StatusCreated,
	)
	first := decodeProfileRecordContract(t, readBody(created))
	retried := uploadProfileFixtureWithID(
		t,
		app,
		defaultToken,
		"example",
		"cpu",
		data,
		uploadID,
		http.StatusOK,
	)
	second := decodeProfileRecordContract(t, readBody(retried))
	if second.ID != first.ID {
		t.Fatalf("retry created profile %d instead of returning %d", second.ID, first.ID)
	}

	conflicting := writeProfile(t, syntheticProfile(99, "main.other", "main.main"))
	uploadProfileFixtureWithID(
		t,
		app,
		defaultToken,
		"example",
		"cpu",
		conflicting,
		uploadID,
		http.StatusConflict,
	)
	uploadProfileFixtureWithID(
		t,
		app,
		p2Token,
		"private",
		"cpu",
		data,
		uploadID,
		http.StatusCreated,
	)

	var listing struct {
		Profiles []profileRecordContract `json:"profiles"`
	}
	if err := json.Unmarshal(readBody(profileGET(t, app, reader, "default", "/api/profiles")), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Profiles) != 1 {
		t.Fatalf("default project profiles=%d want=1", len(listing.Profiles))
	}
}

func profileTestApp(t *testing.T) (interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, string, string, *meta.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	defaultToken, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := store.CreateProject(ctx, "Project 2", "p2")
	if err != nil {
		t.Fatal(err)
	}
	p2Token, err := store.CreateProjectAPIToken(ctx, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newMetadataTestGateway(store,
			Config{Tokens: store, Store: store, Profiles: store, Blobs: blobs},
		).App(),
		defaultToken, p2Token, store
}

func uploadProfileFixture(t *testing.T, app interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, token, service, profileType string, data []byte, want int) *http.Response {
	t.Helper()
	uploadID, err := randomProfileID()
	if err != nil {
		t.Fatal(err)
	}
	uploadID = strings.ReplaceAll(uploadID, "-", "")
	return uploadProfileFixtureWithID(t, app, token, service, profileType, data, uploadID, want)
}

func uploadProfileFixtureWithID(t *testing.T, app interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, token, service, profileType string, data []byte, uploadID string, want int) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("profile", "profile.pb.gz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = w.WriteField("type", profileType)
	_ = w.WriteField("service", service)
	_ = w.WriteField("start", "2026-07-17T12:00:00Z")
	_ = w.WriteField("end", "2026-07-17T12:00:10Z")
	_ = w.WriteField("labels", `{"trace_id":"abc"}`)
	_ = w.WriteField("upload_id", uploadID)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/profiles", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("upload status=%d want=%d body=%s", resp.StatusCode, want, readBody(resp))
	}
	return resp
}

// profileGET reads as a user API token holder; project tokens are ingest-only.
func profileGET(t *testing.T, app interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, reader, project, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+reader)
	req.Header.Set("X-LO-Project", project)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func syntheticProfile(value int64, leaf, root string) *pprofprofile.Profile {
	rootFn := &pprofprofile.Function{ID: 1, Name: root, SystemName: root, Filename: "main.go"}
	leafFn := &pprofprofile.Function{ID: 2, Name: leaf, SystemName: leaf, Filename: "main.go"}
	rootLoc := &pprofprofile.Location{ID: 1, Address: 0x1000, Line: []pprofprofile.Line{{Function: rootFn, Line: 10}}}
	leafLoc := &pprofprofile.Location{ID: 2, Address: 0x2000, Line: []pprofprofile.Line{{Function: leafFn, Line: 20}}}
	return &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*pprofprofile.Sample{
			{Location: []*pprofprofile.Location{leafLoc, rootLoc}, Value: []int64{1, value}},
		},
		Location:   []*pprofprofile.Location{rootLoc, leafLoc},
		Function:   []*pprofprofile.Function{rootFn, leafFn},
		PeriodType: &pprofprofile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     10_000_000,
		TimeNanos:  time.Now().UnixNano(),
	}
}

func writeProfile(t *testing.T, p *pprofprofile.Profile) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func findFlameValue(node *flameNode, name string) int64 {
	if node.Name == name {
		return node.Value
	}
	for _, child := range node.Children {
		if value := findFlameValue(child, name); value != 0 {
			return value
		}
	}
	return 0
}
