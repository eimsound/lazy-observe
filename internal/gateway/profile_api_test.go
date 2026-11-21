package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/janitor"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/profiling"
	"github.com/gofiber/fiber/v3"
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
	tree := profiling.BuildFlamegraph(merged, meta.ProfileTypeCPU, 64, 10_000)
	if tree.Value != 12 || findFlameValue(tree, "main.work") != 12 || findFlameValue(tree, "main.main") != 12 {
		t.Fatalf("merged tree values: %#v", tree)
	}

	depthCut := profiling.BuildFlamegraph(p1, meta.ProfileTypeCPU, 1, 10_000)
	if len(depthCut.Children) != 1 || !depthCut.Children[0].Truncated {
		t.Fatalf("depth truncation missing: %#v", depthCut)
	}
	nodeCut := profiling.BuildFlamegraph(p1, meta.ProfileTypeCPU, 64, 1)
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
	for _, value := range []int64{11, 13} {
		data := writeProfile(t, syntheticProfile(value, "main.busyLoop", "main.main"))
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
	var tree profiling.FlameNode
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

func profileTestApp(t *testing.T) (httpTestApp, string, string, *meta.Store) {
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

func uploadProfileFixture(
	t *testing.T,
	app httpTestApp,
	token, service, profileType string,
	data []byte,
	want int,
) *http.Response {
	t.Helper()
	uploadID, err := randomProfileID()
	if err != nil {
		t.Fatal(err)
	}
	uploadID = strings.ReplaceAll(uploadID, "-", "")
	return uploadProfileFixtureWithID(t, app, token, service, profileType, data, uploadID, want)
}

func uploadProfileFixtureWithID(
	t *testing.T,
	app httpTestApp,
	token, service, profileType string,
	data []byte,
	uploadID string,
	want int,
) *http.Response {
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
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("upload status=%d want=%d body=%s", resp.StatusCode, want, readBody(resp))
	}
	return resp
}

// profileGET reads as a user API token holder; project tokens are ingest-only.
func profileGET(t *testing.T, app httpTestApp, reader, project, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+reader)
	req.Header.Set("X-LO-Project", project)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
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

func findFlameValue(node *profiling.FlameNode, name string) int64 {
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

func TestProfileIngestRejectsDoubleGzipAndSkipsCorruptBlobs(t *testing.T) {
	app, token, _, _ := profileTestApp(t)

	inner := writeProfile(t, syntheticProfile(11, "main.busyLoop", "main.main"))
	var doubleGzip bytes.Buffer
	zw := gzip.NewWriter(&doubleGzip)
	_, _ = zw.Write(inner)
	_ = zw.Close()

	resp := uploadProfileFixture(t, app, token, "svc", "cpu", doubleGzip.Bytes(), http.StatusUnprocessableEntity)
	resp.Body.Close()
}

func TestProfileRawNonDefaultProjectAndContentType(t *testing.T) {
	app, _, p2Token, store := profileTestApp(t)
	reader := userQueryToken(t, store, "profile-reader")
	data := writeProfile(t, syntheticProfile(42, "main.p2Work", "main.main"))

	created := uploadProfileFixture(t, app, p2Token, "p2-svc", "cpu", data, http.StatusCreated)
	rec := decodeProfileRecordContract(t, readBody(created))

	// Default project must not see p2's raw profile.
	crossResp := profileGET(t, app, reader, "default", "/api/profiles/"+strconvI(rec.ID)+"/raw")
	if crossResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-project raw status=%d want=404", crossResp.StatusCode)
	}
	crossResp.Body.Close()

	// Non-default project download must succeed with application/gzip and no Content-Encoding: gzip.
	rawResp := profileGET(t, app, reader, "p2", "/api/profiles/"+strconvI(rec.ID)+"/raw")
	if rawResp.StatusCode != http.StatusOK {
		t.Fatalf("p2 raw status=%d body=%s", rawResp.StatusCode, readBody(rawResp))
	}
	if ct := rawResp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("Content-Type=%q want=application/gzip", ct)
	}
	if ce := rawResp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding=%q want empty", ce)
	}
	body := readBody(rawResp)
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("raw response is not gzip bytes")
	}
}

func TestProfileQueryNonUTCRFC3339(t *testing.T) {
	app, token, _, store := profileTestApp(t)
	reader := userQueryToken(t, store, "profile-reader")
	data := writeProfile(t, syntheticProfile(15, "main.tzWork", "main.main"))
	resp := uploadProfileFixture(t, app, token, "tz-svc", "cpu", data, http.StatusCreated)
	resp.Body.Close()

	// 2026-07-17T12:00:00Z == 2026-07-17T20:00:00+08:00
	listResp := profileGET(
		t,
		app,
		reader,
		"default",
		"/api/profiles?service=tz-svc&type=cpu&start=2026-07-17T19:59:55%2B08:00&end=2026-07-17T20:00:15%2B08:00",
	)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResp.StatusCode, readBody(listResp))
	}
	var listing profileListResponse
	decodeResponse(t, listResp, &listing)
	if len(listing.Profiles) != 1 {
		t.Fatalf("non-UTC profile list count=%d want=1", len(listing.Profiles))
	}
}

func TestProfileCorruptAndMissingBlobsSkippedInFlamegraph(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	)
	if err != nil {
		t.Fatal(err)
	}
	reader := userQueryToken(t, store, "flame-skip-reader")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Profiles: store, Blobs: blobs,
	}).App()

	good := writeProfile(t, syntheticProfile(19, "main.healthy", "main.main"))
	uploadProfileFixture(t, app, token, "mixed", "cpu", good, http.StatusCreated).Body.Close()

	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	baseStart := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	// Inject one corrupt (double-gzipped) blob and one missing blob directly.
	var doubleGz bytes.Buffer
	zw := gzip.NewWriter(&doubleGz)
	_, _ = zw.Write(good)
	_ = zw.Close()
	if err := blobs.Put(
		ctx,
		"profiles/default/mixed/cpu/corrupt.pb.gz",
		bytes.NewReader(doubleGz.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProfileMeta(
		ctx, project.ID, "mixed", meta.ProfileTypeCPU,
		baseStart, baseStart.Add(10*time.Second), int64(doubleGz.Len()),
		strings.Repeat("c", 32), strings.Repeat("c", 64),
		"profiles/default/mixed/cpu/corrupt.pb.gz", map[string]string{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProfileMeta(
		ctx, project.ID, "mixed", meta.ProfileTypeCPU,
		baseStart, baseStart.Add(10*time.Second), 10,
		strings.Repeat("d", 32), strings.Repeat("d", 64),
		"profiles/default/mixed/cpu/missing.pb.gz", map[string]string{},
	); err != nil {
		t.Fatal(err)
	}

	flame := profileGET(t, app, reader, "default", "/api/profiles/flamegraph?service=mixed&type=cpu")
	if flame.StatusCode != http.StatusOK {
		t.Fatalf("flame status=%d body=%s", flame.StatusCode, readBody(flame))
	}
	var tree profiling.FlameNode
	decodeResponse(t, flame, &tree)
	if tree.Value != 19 || findFlameValue(&tree, "main.healthy") != 19 {
		t.Fatalf("expected healthy profile value 19, got %+v", tree)
	}
}

func TestProfileIngestFutureClampMetadataBoundsAndRateLimit(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Profiles: store, Blobs: blobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	data := writeProfile(t, syntheticProfile(7, "main.work", "main.main"))
	postRaw := func(service, start, end, labels string) *http.Response {
		t.Helper()
		uploadID, _ := randomProfileID()
		uploadID = strings.ReplaceAll(uploadID, "-", "")
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, _ := w.CreateFormFile("profile", "profile.pb.gz")
		_, _ = part.Write(data)
		_ = w.WriteField("type", "cpu")
		_ = w.WriteField("service", service)
		_ = w.WriteField("start", start)
		_ = w.WriteField("end", end)
		if labels != "" {
			_ = w.WriteField("labels", labels)
		}
		_ = w.WriteField("upload_id", uploadID)
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/profiles", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Future year 9999 must be clamped to fixedNow and cleaned by retention.
	resp := postRaw("svc", "9999-01-01T00:00:00Z", "9999-01-01T00:01:00Z", `{"trace_id":"t1"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("future profile status=%d body=%s", resp.StatusCode, readBody(resp))
	}
	rec := decodeProfileRecordContract(t, readBody(resp))
	parsedEnd, _ := time.Parse(time.RFC3339Nano, rec.End)
	if parsedEnd.After(fixedNow.Add(maxIngestClockSkew)) {
		t.Fatalf("future profile end escaped clamp: %s", rec.End)
	}
	deleted, err := (&janitor.Profiles{Metadata: store, Blobs: blobs}).Clean(ctx, fixedNow.Add(8*24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("clamped profile not cleaned by retention: deleted=%d err=%v", deleted, err)
	}

	// Oversized service name & oversized labels must be rejected with 400.
	badSvc := postRaw(strings.Repeat("a", maxProfileServiceLen+1), "2026-07-17T11:59:00Z", "2026-07-17T11:59:10Z", "")
	if badSvc.StatusCode != http.StatusBadRequest {
		t.Fatalf("long service status=%d want=400", badSvc.StatusCode)
	}
	badSvc.Body.Close()

	tooManyLabels := make(map[string]string)
	for i := 0; i < maxProfileLabelsCount+1; i++ {
		tooManyLabels["k"+strconvI(i)] = "v"
	}
	labelsJSON, _ := json.Marshal(tooManyLabels)
	badLbl := postRaw("svc", "2026-07-17T11:59:00Z", "2026-07-17T11:59:10Z", string(labelsJSON))
	if badLbl.StatusCode != http.StatusBadRequest {
		t.Fatalf("too many labels status=%d want=400", badLbl.StatusCode)
	}
	badLbl.Body.Close()

	// Rate limit enforced with 429 + Retry-After.
	denyApp := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Profiles: store, Blobs: blobs,
		Limits: fixedAllowLimiter{allowed: false},
	}).App()
	var rlBody bytes.Buffer
	w := multipart.NewWriter(&rlBody)
	part, _ := w.CreateFormFile("profile", "profile.pb.gz")
	_, _ = part.Write(data)
	_ = w.Close()
	rlReq := httptest.NewRequest(http.MethodPost, "/api/profiles", &rlBody)
	rlReq.Header.Set("Content-Type", w.FormDataContentType())
	rlReq.Header.Set("Authorization", "Bearer "+token)
	rlResp, err := denyApp.Test(rlReq)
	if err != nil {
		t.Fatal(err)
	}
	if rlResp.StatusCode != http.StatusTooManyRequests || rlResp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit status=%d retry=%q", rlResp.StatusCode, rlResp.Header.Get("Retry-After"))
	}
	rlResp.Body.Close()
}

type fixedAllowLimiter struct {
	allowed bool
	err     error
}

func (l fixedAllowLimiter) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return l.allowed, l.err
}

func TestProfileFlamegraphWindowBoundsHeterogeneousAndSnapshotAverage(t *testing.T) {
	app, token, _, store := profileTestApp(t)
	reader := userQueryToken(t, store, "flame-adv-reader")

	// Window validation: single bound or span > 7 days rejected with 400.
	halfWindow := profileGET(
		t,
		app,
		reader,
		"default",
		"/api/profiles/flamegraph?service=svc&start=2026-07-17T00:00:00Z",
	)
	if halfWindow.StatusCode != http.StatusBadRequest {
		t.Fatalf("half window status=%d want=400", halfWindow.StatusCode)
	}
	halfWindow.Body.Close()

	hugeWindow := profileGET(
		t,
		app,
		reader,
		"default",
		"/api/profiles/flamegraph?service=svc&start=2026-07-01T00:00:00Z&end=2026-07-20T00:00:00Z",
	)
	if hugeWindow.StatusCode != http.StatusBadRequest {
		t.Fatalf("huge window status=%d want=400", hugeWindow.StatusCode)
	}
	hugeWindow.Body.Close()

	// Heterogeneous SampleType CPU profiles must merge without 422.
	p1 := syntheticProfile(10, "main.a", "main.main")
	p2 := syntheticProfile(20, "main.b", "main.main")
	p2.SampleType = []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}
	p2.Sample[0].Value = []int64{20}
	uploadProfileFixture(t, app, token, "hetero", "cpu", writeProfile(t, p1), http.StatusCreated).Body.Close()
	uploadProfileFixture(t, app, token, "hetero", "cpu", writeProfile(t, p2), http.StatusCreated).Body.Close()

	heteroResp := profileGET(t, app, reader, "default", "/api/profiles/flamegraph?service=hetero&type=cpu")
	if heteroResp.StatusCode != http.StatusOK {
		t.Fatalf("hetero flame status=%d body=%s", heteroResp.StatusCode, readBody(heteroResp))
	}
	var heteroTree profiling.FlameNode
	decodeResponse(t, heteroResp, &heteroTree)
	if heteroTree.Value != 30 || heteroTree.Unit != "nanoseconds" {
		t.Fatalf("hetero tree=%+v want value=30 unit=nanoseconds", heteroTree)
	}

	// Heap snapshot profiles must be averaged across snapshots instead of summed.
	h1 := syntheticProfile(100, "main.alloc", "main.main")
	h1.SampleType = []*pprofprofile.ValueType{{Type: "inuse_space", Unit: "bytes"}}
	h1.Sample[0].Value = []int64{100}
	h2 := syntheticProfile(200, "main.alloc", "main.main")
	h2.SampleType = []*pprofprofile.ValueType{{Type: "inuse_space", Unit: "bytes"}}
	h2.Sample[0].Value = []int64{200}
	uploadProfileFixture(t, app, token, "heap-svc", "heap", writeProfile(t, h1), http.StatusCreated).Body.Close()
	uploadProfileFixture(t, app, token, "heap-svc", "heap", writeProfile(t, h2), http.StatusCreated).Body.Close()

	heapResp := profileGET(t, app, reader, "default", "/api/profiles/flamegraph?service=heap-svc&type=heap")
	if heapResp.StatusCode != http.StatusOK {
		t.Fatalf("heap flame status=%d body=%s", heapResp.StatusCode, readBody(heapResp))
	}
	var heapTree profiling.FlameNode
	decodeResponse(t, heapResp, &heapTree)
	if heapTree.Value != 150 || heapTree.Unit != "bytes" {
		t.Fatalf("heap snapshot tree=%+v want averaged value=150 unit=bytes", heapTree)
	}

	// Zero/negative samples ignored, int64 overflow clamped, and depth > 64 preserved.
	deep := &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
	}
	locs := make([]*pprofprofile.Location, 0, 80)
	for i := 1; i <= 80; i++ {
		fn := &pprofprofile.Function{ID: uint64(i), Name: "fn" + strconvI(i)}
		loc := &pprofprofile.Location{ID: uint64(i), Line: []pprofprofile.Line{{Function: fn}}}
		deep.Function = append(deep.Function, fn)
		deep.Location = append(deep.Location, loc)
		locs = append(locs, loc)
	}
	deep.Sample = []*pprofprofile.Sample{
		{Location: locs, Value: []int64{-50}},
		{Location: locs, Value: []int64{0}},
		{Location: locs, Value: []int64{math.MaxInt64 - 5}},
		{Location: locs, Value: []int64{100}},
	}
	deepTree := profiling.BuildFlamegraph(deep, meta.ProfileTypeCPU, maxFlameDepth, maxFlameNodes)
	if deepTree.Value != math.MaxInt64 {
		t.Fatalf("overflow clamping failed: got %d", deepTree.Value)
	}
	if findFlameValue(deepTree, "fn1") != math.MaxInt64 {
		t.Fatalf("depth 80 leaf frame was truncated")
	}
}

func TestDeleteProjectCleansProfileAndReplayBlobs(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	); err != nil {
		t.Fatal(err)
	}
	p, err := store.CreateProject(ctx, "Disposable", "disposable")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, "profiles/disposable/svc/cpu/1.pb.gz", strings.NewReader("p")); err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, "replay/disposable/sess/0.json.gz", strings.NewReader("r")); err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(
		ctx,
		"sourcemaps/disposable/revisions/"+strings.Repeat("a", 64)+".map",
		strings.NewReader("s"),
	); err != nil {
		t.Fatal(err)
	}
	admin, err := store.CreateUser(ctx, "profile-del-admin", "password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, adminToken, err := store.CreateUserAPIToken(ctx, admin.ID, "admin-del")
	if err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Management: management.New(store), Profiles: store, Replays: store, Blobs: blobs,
	}).App()

	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+strconvI(p.ID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete project status=%d body=%s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	if _, err := blobs.Get(ctx, "profiles/disposable/svc/cpu/1.pb.gz"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("profile blob still present after project delete: err=%v", err)
	}
	if _, err := blobs.Get(ctx, "replay/disposable/sess/0.json.gz"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("replay blob still present after project delete: err=%v", err)
	}
	if _, err := blobs.Get(
		ctx,
		"sourcemaps/disposable/revisions/"+strings.Repeat("a", 64)+".map",
	); !errors.Is(
		err,
		blob.ErrNotFound,
	) {
		t.Fatalf("sourcemap blob still present after project delete: err=%v", err)
	}
	// Recreating a project with the deleted slug must be rejected with 409 Conflict.
	recreateReq := httptest.NewRequest(
		http.MethodPost,
		"/api/projects",
		strings.NewReader(`{"name":"Reused","slug":"disposable"}`),
	)
	recreateReq.Header.Set("Authorization", "Bearer "+adminToken)
	recreateReq.Header.Set("Content-Type", "application/json")
	recreateResp, err := app.Test(recreateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer recreateResp.Body.Close()
	if recreateResp.StatusCode != http.StatusConflict {
		t.Fatalf("recreate deleted slug status=%d want 409 body=%s", recreateResp.StatusCode, readBody(recreateResp))
	}
}

type keyMatchLimiter struct {
	denyPrefix string
	errPrefix  string
	seenKeys   []string
}

func (l *keyMatchLimiter) Allow(_ context.Context, key string, _ int, _ time.Duration) (bool, error) {
	l.seenKeys = append(l.seenKeys, key)
	if l.errPrefix != "" && strings.HasPrefix(key, l.errPrefix) {
		return false, errors.New("limiter backend unavailable")
	}
	if l.denyPrefix != "" && strings.HasPrefix(key, l.denyPrefix) {
		return false, nil
	}
	return true, nil
}

type failingCreateProfileStore struct {
	ProfileStore
	err error
}

func (s failingCreateProfileStore) CreateProfileMeta(
	context.Context,
	int,
	string,
	meta.ProfileType,
	time.Time,
	time.Time,
	int64,
	string,
	string,
	string,
	map[string]string,
) (*meta.ProfileMeta, error) {
	return nil, s.err
}

func TestProfileIngestSampleTypeIPLimitAndRollback(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := store.Bootstrap(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		meta.BootstrapAdmin{GeneratedPasswordFile: filepath.Join(t.TempDir(), meta.InitialAdminPasswordFileName)},
	)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// 1. Mismatched SampleType (heap payload uploaded as cpu) rejected with 422.
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Profiles: store, Blobs: blobs,
	}).App()
	heapPayload := syntheticProfile(100, "main.alloc", "main.main")
	heapPayload.SampleType = []*pprofprofile.ValueType{{Type: "inuse_space", Unit: "bytes"}}
	heapPayload.Sample[0].Value = []int64{100}
	uploadProfileFixture(
		t,
		app,
		token,
		"svc",
		"cpu",
		writeProfile(t, heapPayload),
		http.StatusUnprocessableEntity,
	).Body.Close()

	// 2. Per-IP rate limit enforced via unified publicIngestAdmission guard.
	ipLimiter := &keyMatchLimiter{denyPrefix: "profile:ingest:ip:"}
	ipApp := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Profiles: store, Blobs: blobs, Limits: ipLimiter,
	}).App()
	validData := writeProfile(t, syntheticProfile(11, "main.work", "main.main"))
	ipResp := uploadProfileFixture(t, ipApp, token, "svc", "cpu", validData, http.StatusTooManyRequests)
	if ipResp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on IP rate limit")
	}
	ipResp.Body.Close()
	sawProject, sawIP := false, false
	for _, k := range ipLimiter.seenKeys {
		if strings.HasPrefix(k, "profile:ingest:project:") {
			sawProject = true
		}
		if strings.HasPrefix(k, "profile:ingest:ip:") {
			sawIP = true
		}
	}
	if !sawProject || !sawIP {
		t.Fatalf("expected both project and IP limiter checks, got keys=%v", ipLimiter.seenKeys)
	}

	// 3. Limiter backend failure fails closed with 503.
	errApp := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Profiles: store, Blobs: blobs,
		Limits: &keyMatchLimiter{errPrefix: "profile:ingest:ip:"},
	}).App()
	uploadProfileFixture(t, errApp, token, "svc", "cpu", validData, http.StatusServiceUnavailable).Body.Close()

	// 4. DB metadata failure rolls back the uploaded profile blob so no orphan remains.
	failApp := newMetadataTestGateway(store, Config{
		Tokens:   store,
		Store:    store,
		Profiles: failingCreateProfileStore{ProfileStore: store, err: errors.New("db write error")},
		Blobs:    blobs,
	}).App()
	uploadProfileFixture(
		t,
		failApp,
		token,
		"rollback-svc",
		"cpu",
		validData,
		http.StatusInternalServerError,
	).Body.Close()
	objects, err := blobs.List(ctx, "profiles/default/rollback-svc/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("orphan profile blob was not rolled back: %+v", objects)
	}
}
