package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestReplayEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
	reader := userQueryToken(t, store, "replay-reader")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recordingStart := time.Date(2026, 7, 17, 11, 59, 0, 0, time.UTC)
	defaultProject, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	receiptPayload := []byte(`{"event_id":"event-1"}`)
	acceptedAt := recordingStart.Add(5 * time.Second)
	if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
		ProjectID: defaultProject.ID, EventID: "event-1", CanonicalDigest: "replay-event-1",
		Payload: receiptPayload, PayloadBytes: int64(len(receiptPayload)),
		OccurredAt: recordingStart.Add(2 * time.Second), AcceptedAt: acceptedAt,
	}}, errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimErrorEvents(ctx, "replay-worker", 1, acceptedAt.Add(time.Second), time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim replay error receipt=%#v err=%v", claimed, err)
	}
	changes, err := store.CompleteErrorEvent(
		ctx,
		claimed[0].ID,
		"replay-worker",
		claimed[0].ClaimEpoch,
		errortrack.IssueDelta{
			ProjectID: defaultProject.ID, Fingerprint: "replay-error", Title: "checkout failed", Level: "warning",
			Count: 1, FirstSeen: recordingStart.Add(2 * time.Second), LastSeen: recordingStart.Add(2 * time.Second),
			LastEventID: "event-1", AcceptedAt: acceptedAt, AppliedAt: acceptedAt.Add(2 * time.Second),
		},
		acceptedAt.Add(2*time.Second),
	)
	if err != nil || len(changes) != 1 {
		t.Fatalf("complete replay error receipt changes=%#v err=%v", changes, err)
	}
	issueID := strconv.Itoa(changes[0].IssueID)
	app := newMetadataTestGateway(store,
		Config{
			Tokens:  store,
			Store:   store,
			Replays: store,
			Blobs:   blobs,
			Now:     func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		},
	).App()
	originals := [][]byte{
		[]byte(
			fmt.Sprintf(
				`[{"type":4,"data":{"href":"https://example.test/start"},"timestamp":%d}]`,
				recordingStart.UnixMilli(),
			),
		),
		[]byte(
			fmt.Sprintf(
				`[{"type":3,"data":{"source":1,"text":"hello"},"timestamp":%d}]`,
				recordingStart.Add(time.Second).UnixMilli(),
			),
		),
		[]byte(fmt.Sprintf(`[
			{"type":5,"data":{"tag":"lazy-observe:error","payload":{"timestamp":%q,"event_id":"event-1","title":"checkout failed","level":"warning"}},"timestamp":%d},
			{"type":5,"data":{"tag":"lazy-observe:http","payload":{"timestamp":%d,"method":"POST","path":"https://example.test/api/orders/42?token=private#card","status":503,"duration_ms":12.5}},"timestamp":%d},
			{"type":5,"data":{"tag":"lazy-observe:vital","payload":{"timestamp":%d,"name":"LCP","value":2450.5,"rating":"needs-improvement"}},"timestamp":%d}
		]`,
			recordingStart.Add(2*time.Second).Format(time.RFC3339Nano), recordingStart.Add(2*time.Second).UnixMilli(),
			recordingStart.Add(2500*time.Millisecond).UnixMilli(), recordingStart.Add(3*time.Second).UnixMilli(),
			recordingStart.Add(3500*time.Millisecond).UnixMilli(), recordingStart.Add(4*time.Second).UnixMilli(),
		)),
	}
	for seq, original := range originals {
		uploadReplay(t, app, defaultToken, "shared-session", seq, original, seq == 2, http.StatusAccepted, seq == 1)
	}

	list := replayRequest(t, app, reader, "default", "/api/replay/sessions?has_error=true")
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.StatusCode, readBody(list))
	}
	var listing struct {
		Sessions []struct {
			SessionID    string `json:"session_id"`
			StartedAt    int64  `json:"started_at"`
			LastSeenAt   int64  `json:"last_seen_at"`
			HasError     bool   `json:"has_error"`
			SegmentCount int    `json:"segment_count"`
			PageURL      string `json:"page_url"`
		} `json:"sessions"`
		Total    int `json:"total"`
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}
	decodeResponse(t, list, &listing)
	if len(listing.Sessions) != 1 || listing.Sessions[0].SessionID != "shared-session" ||
		!listing.Sessions[0].HasError ||
		listing.Sessions[0].SegmentCount != 3 {
		t.Fatalf("unexpected listing: %+v", listing.Sessions)
	}
	if listing.Total != 1 || listing.Page != 1 || listing.PageSize != replayPageSize {
		t.Fatalf("unexpected pagination: total=%d page=%d page_size=%d", listing.Total, listing.Page, listing.PageSize)
	}
	if listing.Sessions[0].StartedAt != recordingStart.UnixMilli() ||
		listing.Sessions[0].LastSeenAt != recordingStart.Add(4*time.Second).UnixMilli() {
		t.Fatalf("unexpected session range: %+v", listing.Sessions[0])
	}

	detail := replayRequest(t, app, reader, "default", "/api/replay/sessions/shared-session")
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.StatusCode, readBody(detail))
	}
	detailBody := readBody(detail)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(detailBody, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, exists := envelope["error_markers"]; exists {
		t.Fatal("Replay detail exposed the removed error_markers compatibility projection")
	}
	var session struct {
		SessionID string `json:"session_id"`
		Segments  []struct {
			Seq        int   `json:"seq"`
			StartedAt  int64 `json:"started_at"`
			EventCount int   `json:"event_count"`
			SizeBytes  int64 `json:"size_bytes"`
		} `json:"segments"`
		Markers []struct {
			Type       string  `json:"type"`
			Time       int64   `json:"time"`
			IssueID    string  `json:"issue_id"`
			EventID    string  `json:"event_id"`
			Title      string  `json:"title"`
			Level      string  `json:"level"`
			Method     string  `json:"method"`
			Path       string  `json:"path"`
			Status     int     `json:"status"`
			DurationMS float64 `json:"duration_ms"`
			Name       string  `json:"name"`
			Value      float64 `json:"value"`
			Rating     string  `json:"rating"`
		} `json:"markers"`
	}
	if err := json.Unmarshal(detailBody, &session); err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "shared-session" || len(session.Segments) != 3 || session.Segments[2].Seq != 2 ||
		session.Segments[2].EventCount != 3 ||
		session.Segments[2].SizeBytes <= 0 {
		t.Fatalf("unexpected detail: %+v", session)
	}
	if len(session.Markers) != 3 || session.Markers[0].Type != "error" ||
		session.Markers[0].Time != recordingStart.Add(2*time.Second).UnixMilli() ||
		session.Markers[0].IssueID != issueID ||
		session.Markers[0].EventID != "event-1" ||
		session.Markers[0].Title != "checkout failed" ||
		session.Markers[0].Level != "warning" {
		t.Fatalf("unexpected markers: %+v", session.Markers)
	}
	if marker := session.Markers[1]; marker.Type != "http" ||
		marker.Time != recordingStart.Add(2500*time.Millisecond).UnixMilli() ||
		marker.Method != "POST" ||
		marker.Path != "/api/orders/42" ||
		marker.Status != 503 ||
		marker.DurationMS != 12.5 {
		t.Fatalf("unexpected HTTP marker: %+v", marker)
	}
	if marker := session.Markers[2]; marker.Type != "vital" ||
		marker.Time != recordingStart.Add(3500*time.Millisecond).UnixMilli() ||
		marker.Name != "LCP" ||
		marker.Value != 2450.5 ||
		marker.Rating != "needs-improvement" {
		t.Fatalf("unexpected Web Vital marker: %+v", marker)
	}

	for seq, original := range originals {
		resp := replayRequest(
			t,
			app,
			reader,
			"default",
			"/api/replay/sessions/shared-session/segments/"+string(rune('0'+seq)),
		)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("segment %d status=%d encoding=%q", seq, resp.StatusCode, resp.Header.Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		zr.Close()
		resp.Body.Close()
		if !bytes.Equal(got, original) {
			t.Fatalf("segment %d = %s, want %s", seq, got, original)
		}
	}

	uploadReplay(t, app, defaultToken, "shared-session", 1, originals[1], false, http.StatusAccepted)
	list = replayRequest(t, app, reader, "default", "/api/replay/sessions")
	decodeResponse(t, list, &listing)
	if listing.Sessions[0].SegmentCount != 3 {
		t.Fatalf("duplicate changed count to %d", listing.Sessions[0].SegmentCount)
	}
	uploadReplay(
		t,
		app,
		defaultToken,
		"too-big",
		0,
		bytes.Repeat([]byte(" "), maxReplaySegment+1),
		false,
		http.StatusRequestEntityTooLarge,
	)

	uploadReplay(t, app, p2Token, "private-session", 0, []byte(`[]`), false, http.StatusAccepted)
	list = replayRequest(t, app, reader, "default", "/api/replay/sessions")
	body := readBody(list)
	if bytes.Contains(body, []byte("private-session")) {
		t.Fatalf("p2 session leaked into default list: %s", body)
	}
	direct := replayRequest(t, app, reader, "default", "/api/replay/sessions/private-session")
	if direct.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-project direct GET status=%d", direct.StatusCode)
	}

	viewer, err := store.CreateUser(ctx, "replay-viewer", "password", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	cookieToken := metadataSession(
		t, store, viewer.ID,
		time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/replay/sessions", nil)
	req.Header.Set("X-LO-Project", "p2")
	req.AddCookie(&http.Cookie{Name: "lo_session", Value: cookieToken})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	sessionBody := readBody(resp)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(sessionBody, []byte("private-session")) {
		t.Fatalf("session-channel p2 list status=%d body=%s", resp.StatusCode, sessionBody)
	}
}

func uploadReplay(
	t *testing.T,
	app httpTestApp,
	token, sessionID string,
	seq int,
	original []byte,
	hasError bool,
	want int,
	naked ...bool,
) {
	t.Helper()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	target := "/api/replay/segments?" + url.Values{"session_id": {sessionID}, "seq": {strconv.Itoa(seq)}}.Encode()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(compressed.Bytes()))
	req.Header.Set("Authorization", "Bearer "+token)
	if len(naked) == 0 || !naked[0] {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if hasError {
		req.Header.Set("X-LO-Replay-Error", "1")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload %s/%d status=%d want=%d body=%s", sessionID, seq, resp.StatusCode, want, body)
	}
}

func replayRequest(t *testing.T, app httpTestApp, token, project, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if project != "" {
		req.Header.Set("X-LO-Project", project)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func decodeResponse(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
func readBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

func TestReplayIdentityCompressionOverwriteProtectionAndTimestampClamp(t *testing.T) {
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
	reader := userQueryToken(t, store, "replay-reader-2")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	app := newMetadataTestGateway(store, Config{
		Tokens:  store,
		Store:   store,
		Replays: store,
		Blobs:   blobs,
		Now:     func() time.Time { return fixedNow },
	}).App()

	// 1. Identity compression fallback must be accepted.
	payload := []byte(
		`[{"type":4,"data":{"href":"https://user:pass@example.test/pay?secret=1#frag"},"timestamp":` +
			strconv.FormatInt(fixedNow.Add(-time.Minute).UnixMilli(), 10) + `}]`,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/replay/segments?session_id=id-sess&seq=0",
		bytes.NewReader(payload),
	)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-LO-Replay-Compression", "identity")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("identity upload status=%d want=202 body=%s", resp.StatusCode, body)
	}

	// 2. Overwriting existing (session_id, seq) with different payload must return 409.
	tampered := []byte(`[{"type":3,"data":{"text":"tampered"},"timestamp":` +
		strconv.FormatInt(fixedNow.UnixMilli(), 10) + `}]`)
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/api/replay/segments?session_id=id-sess&seq=0",
		bytes.NewReader(tampered),
	)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("X-LO-Replay-Compression", "identity")
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2 := readBody(resp2)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("overwrite status=%d want=409 body=%s", resp2.StatusCode, body2)
	}

	// 3. Future / huge timestamps must be clamped to prevent retention escape.
	futurePayload := []byte(
		`[{"type":4,"data":{"href":"https://example.test/f"},"timestamp":253402214400000},{"type":3,"data":{},"timestamp":1e18}]`,
	)
	uploadReplay(t, app, token, "future-sess", 0, futurePayload, false, http.StatusAccepted)
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetReplaySession(ctx, project.ID, "future-sess")
	if err != nil {
		t.Fatal(err)
	}
	if sess.LastSeenAt.After(fixedNow.Add(5 * time.Minute)) {
		t.Fatalf("future timestamp escaped clamping: last_seen_at=%s now=%s", sess.LastSeenAt, fixedNow)
	}

	// 4. Query with non-UTC RFC3339 and verify page_url stripped query/fragment/userinfo.
	listResp := replayRequest(
		t,
		app,
		reader,
		"default",
		"/api/replay/sessions?start=2026-07-17T19:58:00%2B08:00&end=2026-07-17T20:01:00%2B08:00",
	)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("non-UTC list status=%d body=%s", listResp.StatusCode, readBody(listResp))
	}
	var listing struct {
		Sessions []replaySessionDTO `json:"sessions"`
	}
	decodeResponse(t, listResp, &listing)
	foundID := false
	for _, s := range listing.Sessions {
		if s.SessionID == "id-sess" {
			foundID = true
			if s.PageURL != "https://example.test/pay" {
				t.Fatalf("page_url=%q want=https://example.test/pay", s.PageURL)
			}
		}
	}
	if !foundID {
		t.Fatalf("non-UTC list missed id-sess: %+v", listing.Sessions)
	}

	// 5. Huge page rejected with 400; long session_id rejected with 400; seq over quota rejected with 413.
	badPage := replayRequest(t, app, reader, "default", "/api/replay/sessions?page=999999999999999")
	if badPage.StatusCode != http.StatusBadRequest {
		t.Fatalf("huge page status=%d want=400", badPage.StatusCode)
	}
	badPage.Body.Close()

	longID := strings.Repeat("a", maxReplaySessionIDLen+1)
	uploadReplay(t, app, token, longID, 0, []byte(`[]`), false, http.StatusBadRequest)
	uploadReplay(
		t,
		app,
		token,
		"quota-sess",
		maxReplaySegmentsPerSession,
		[]byte(`[]`),
		false,
		http.StatusRequestEntityTooLarge,
	)
}

type failingUpsertReplayStore struct {
	ReplayStore
	err error
}

func (s failingUpsertReplayStore) UpsertReplaySegment(
	context.Context, meta.ReplaySegmentInput,
) (meta.ReplaySegmentUpsertResult, error) {
	return meta.ReplaySegmentUpsertResult{}, s.err
}

func TestReplayFieldsSanitizationPersistedSummaryAndRollback(t *testing.T) {
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
	reader := userQueryToken(t, store, "replay-san-reader")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: blobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	// Segment 0 has a DOM snapshot with an unrelated href and a malformed event timestamp.
	// It must not hijack page_url or cause detail read to 500.
	seg0 := []byte(fmt.Sprintf(`[
		{"type":2,"data":{"node":{"attributes":{"href":"javascript:evil"}}},"timestamp":%d},
		{"type":5,"data":{"tag":"custom","payload":"not-an-object"},"timestamp":"bad-ts"}
	]`, fixedNow.Add(-2*time.Minute).UnixMilli()))
	uploadReplay(t, app, token, "san-sess", 0, seg0, false, http.StatusAccepted)

	// Segment 1 backfills page_url (with UTF-8 multibyte chars > 2048 bytes) and release.
	longPath := "/" + strings.Repeat("中", 900)
	seg1 := []byte(fmt.Sprintf(`[
		{"type":5,"data":{"tag":"lazy-observe:page","payload":{"page_url":"https://example.test%s?secret=1#frag","release":"rel-1.2\u0000"}},"timestamp":%d}
	]`, longPath, fixedNow.Add(-time.Minute).UnixMilli()))
	uploadReplay(t, app, token, "san-sess", 1, seg1, false, http.StatusAccepted)

	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.GetReplaySession(ctx, project.ID, "san-sess")
	if err != nil {
		t.Fatal(err)
	}
	if row.PageURL == nil || strings.Contains(*row.PageURL, "javascript:") ||
		strings.Contains(*row.PageURL, "secret=") || !utf8.ValidString(*row.PageURL) || len(*row.PageURL) > 2048 {
		t.Fatalf("unexpected sanitized page_url=%v", row.PageURL)
	}
	if row.Release == nil || *row.Release != "rel-1.2" {
		t.Fatalf("unexpected backfilled release=%v", row.Release)
	}

	// Corrupt the raw .json.gz for seq 0 while keeping .meta.json intact to verify
	// getReplaySession serves from the persisted summary without decompressing raw blobs.
	if err := blobs.Put(ctx, "replay/default/san-sess/0.json.gz", strings.NewReader("not-gzip")); err != nil {
		t.Fatal(err)
	}
	detail := replayRequest(t, app, reader, "default", "/api/replay/sessions/san-sess")
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.StatusCode, readBody(detail))
	}
	var parsedDetail struct {
		Segments []replaySegmentDTO `json:"segments"`
	}
	decodeResponse(t, detail, &parsedDetail)
	if len(parsedDetail.Segments) != 2 {
		t.Fatalf("segments=%d want=2", len(parsedDetail.Segments))
	}

	// Upsert failure must roll back new blobs and not leak internal error text.
	failApp := newMetadataTestGateway(store, Config{
		Tokens:  store,
		Store:   store,
		Replays: failingUpsertReplayStore{ReplayStore: store, err: errors.New("secret-db-dsn://internal")},
		Blobs:   blobs,
		Now:     func() time.Time { return fixedNow },
	}).App()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/replay/segments?session_id=rollback-sess&seq=0",
		bytes.NewReader([]byte(`[{"type":4,"data":{"href":"https://example.test/r"},"timestamp":1773720000000}]`)),
	)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-LO-Replay-Compression", "identity")
	failResp, err := failApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	failBody := readBody(failResp)
	if failResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("upsert failure status=%d want=500", failResp.StatusCode)
	}
	if bytes.Contains(failBody, []byte("secret-db-dsn")) {
		t.Fatalf("500 response leaked internal error: %s", failBody)
	}
	if _, err := blobs.Get(ctx, "replay/default/rollback-sess/0.json.gz"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("orphan segment blob was not rolled back: err=%v", err)
	}
	if _, err := blobs.Get(ctx, "replay/default/rollback-sess/0.meta.json"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("orphan summary blob was not rolled back: err=%v", err)
	}
}

type noListBlobStore struct {
	blob.Store
	listCalls int
}

func (s *noListBlobStore) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	s.listCalls++
	return s.Store.List(ctx, prefix)
}

type countingIssueStore struct {
	IssueStore
	batch       replayIssueBatchLookup
	singleCalls int
	batchCalls  int
}

func (s *countingIssueStore) IssueIDForErrorEvent(
	ctx context.Context,
	projectID int,
	eventID string,
) (int, bool, error) {
	s.singleCalls++
	return s.IssueStore.IssueIDForErrorEvent(ctx, projectID, eventID)
}

func (s *countingIssueStore) IssueIDsForErrorEvents(
	ctx context.Context, projectID int, eventIDs []string,
) (map[string]int, error) {
	s.batchCalls++
	return s.batch.IssueIDsForErrorEvents(ctx, projectID, eventIDs)
}

func TestReplayDetailResolvesMultipleErrorMarkersInSingleBatch(t *testing.T) {
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
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	reader := userQueryToken(t, store, "replay-batch-reader")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	for _, evID := range []string{"batch-ev-1", "batch-ev-2"} {
		payload := []byte(`{"event_id":"` + evID + `"}`)
		if err := store.AcceptErrorEvents(ctx, []errortrack.InboxEntry{{
			ProjectID: project.ID, EventID: evID, CanonicalDigest: evID,
			Payload: payload, PayloadBytes: int64(len(payload)),
			OccurredAt: fixedNow.Add(-time.Minute), AcceptedAt: fixedNow.Add(-time.Minute),
		}}, errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimErrorEvents(ctx, "batch-worker", 1, fixedNow, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim %s: %v", evID, err)
		}
		if _, err := store.CompleteErrorEvent(
			ctx, claimed[0].ID, "batch-worker", claimed[0].ClaimEpoch,
			errortrack.IssueDelta{
				ProjectID: project.ID, Fingerprint: "fp-" + evID, Title: "err " + evID, Level: "error",
				Count: 1, FirstSeen: fixedNow.Add(-time.Minute), LastSeen: fixedNow.Add(-time.Minute),
				LastEventID: evID, AcceptedAt: fixedNow.Add(-time.Minute), AppliedAt: fixedNow,
			}, fixedNow,
		); err != nil {
			t.Fatal(err)
		}
	}

	countedIssues := &countingIssueStore{IssueStore: store, batch: store}
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Issues: countedIssues, Replays: store, Blobs: blobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	seg0 := []byte(fmt.Sprintf(
		`[{"type":5,"data":{"tag":"lazy-observe:error","payload":{"event_id":"batch-ev-1","title":"e1"}},"timestamp":%d}]`,
		fixedNow.Add(-2*time.Minute).UnixMilli(),
	))
	seg1 := []byte(fmt.Sprintf(
		`[{"type":5,"data":{"tag":"lazy-observe:error","payload":{"event_id":"batch-ev-2","title":"e2"}},"timestamp":%d}]`,
		fixedNow.Add(-time.Minute).UnixMilli(),
	))
	uploadReplay(t, app, token, "batch-sess", 0, seg0, true, http.StatusAccepted)
	uploadReplay(t, app, token, "batch-sess", 1, seg1, true, http.StatusAccepted)

	detail := replayRequest(t, app, reader, "default", "/api/replay/sessions/batch-sess")
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.StatusCode, readBody(detail))
	}
	var parsed struct {
		Markers []replayMarkerDTO `json:"markers"`
	}
	decodeResponse(t, detail, &parsed)
	if len(parsed.Markers) != 2 || parsed.Markers[0].IssueID == "" || parsed.Markers[1].IssueID == "" {
		t.Fatalf("expected both markers resolved, got %+v", parsed.Markers)
	}
	if countedIssues.batchCalls != 1 || countedIssues.singleCalls != 0 {
		t.Fatalf("expected 1 bulk lookup and 0 N+1 lookups, got batch=%d single=%d",
			countedIssues.batchCalls, countedIssues.singleCalls)
	}
}

func TestReplayIngestAndReadDecoupledFromBlobListAndGuardLimits(t *testing.T) {
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
	reader := userQueryToken(t, store, "replay-nolist-reader")
	localBlobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trackedBlobs := &noListBlobStore{Store: localBlobs}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: trackedBlobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	// 1. Ingest and read detail must NEVER call blobs.List.
	seg0 := []byte(fmt.Sprintf(
		`[{"type":4,"data":{"href":"https://example.test/checkout"},"timestamp":%d}]`,
		fixedNow.Add(-2*time.Minute).UnixMilli(),
	))
	seg1 := []byte(fmt.Sprintf(
		`[{"type":5,"data":{"tag":"lazy-observe:http","payload":{"method":"GET","path":"/api/cart","status":200,"duration_ms":15}},"timestamp":%d}]`,
		fixedNow.Add(-time.Minute).UnixMilli(),
	))
	uploadReplay(t, app, token, "nolist-sess", 0, seg0, false, http.StatusAccepted)
	uploadReplay(t, app, token, "nolist-sess", 1, seg1, false, http.StatusAccepted)

	detail := replayRequest(t, app, reader, "default", "/api/replay/sessions/nolist-sess")
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.StatusCode, readBody(detail))
	}
	var parsed struct {
		Segments []replaySegmentDTO `json:"segments"`
		Markers  []replayMarkerDTO  `json:"markers"`
	}
	decodeResponse(t, detail, &parsed)
	if len(parsed.Segments) != 2 || len(parsed.Markers) != 1 {
		t.Fatalf("unexpected detail segments=%d markers=%d", len(parsed.Segments), len(parsed.Markers))
	}
	if trackedBlobs.listCalls != 0 {
		t.Fatalf("expected 0 blobs.List calls on replay ingest+read, got %d", trackedBlobs.listCalls)
	}

	// 2. Legacy session (replay_sessions row without replay_segments) smoothly falls back to blob storage.
	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	var legacyGz bytes.Buffer
	zw := gzip.NewWriter(&legacyGz)
	_, _ = zw.Write(seg0)
	_ = zw.Close()
	if err := localBlobs.Put(
		ctx,
		"replay/default/legacy-sess/0.json.gz",
		bytes.NewReader(legacyGz.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertReplaySession(
		ctx, project.ID, "legacy-sess", fixedNow.Add(-2*time.Minute), fixedNow.Add(-2*time.Minute),
		1, int64(legacyGz.Len()), false, "", "https://example.test/checkout",
	); err != nil {
		t.Fatal(err)
	}
	legacyDetail := replayRequest(t, app, reader, "default", "/api/replay/sessions/legacy-sess")
	if legacyDetail.StatusCode != http.StatusOK {
		t.Fatalf("legacy detail status=%d body=%s", legacyDetail.StatusCode, readBody(legacyDetail))
	}
	var legacyParsed struct {
		Segments []replaySegmentDTO `json:"segments"`
	}
	decodeResponse(t, legacyDetail, &legacyParsed)
	if len(legacyParsed.Segments) != 1 || legacyParsed.Segments[0].Seq != 0 {
		t.Fatalf("legacy fallback segments=%+v", legacyParsed.Segments)
	}

	// 3. Single-request event count quota rejects > maxReplayEventsPerSegment with 413.
	var hugeEvents strings.Builder
	hugeEvents.WriteByte('[')
	for i := 0; i < maxReplayEventsPerSegment+1; i++ {
		if i > 0 {
			hugeEvents.WriteByte(',')
		}
		hugeEvents.WriteString("{}")
	}
	hugeEvents.WriteByte(']')
	uploadReplay(
		t, app, token, "many-events", 0, []byte(hugeEvents.String()), false, http.StatusRequestEntityTooLarge,
	)

	// 4. publicIngestAdmission enforces the per-replica local per-IP quota
	// (429 + Retry-After + CORS) while the shared Redis limiter is down: no
	// fail-closed 503 and no shared-limiter calls (ADR 0020).
	shared := &unavailableSharedLimiter{}
	ipGW := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: localBlobs, Limits: shared,
	})
	ipGW.replayGuard.ip = newLocalIngestWindow(1, ipGW.now)
	ipApp := ipGW.App()
	uploadReplay(t, ipApp, token, "rl-ip-first", 0, seg0, false, http.StatusAccepted)
	req := httptest.NewRequest(http.MethodPost, "/api/replay/segments?session_id=rl-ip&seq=0", bytes.NewReader(seg0))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://app.example.test")
	req.Header.Set("X-LO-Replay-Compression", "identity")
	rlResp, err := ipApp.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if rlResp.StatusCode != http.StatusTooManyRequests ||
		rlResp.Header.Get("Retry-After") == "" ||
		rlResp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("replay IP rate limit status=%d retry=%q cors=%q",
			rlResp.StatusCode, rlResp.Header.Get("Retry-After"), rlResp.Header.Get("Access-Control-Allow-Origin"))
	}
	rlResp.Body.Close()

	// 5. Per-session local quota: the same session is throttled while a new
	// session from the same project and IP is still admitted.
	sessionGW := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: localBlobs, Limits: shared,
	})
	sessionGW.replaySessionLimit = newLocalIngestWindow(1, sessionGW.now)
	sessionApp := sessionGW.App()
	uploadReplay(t, sessionApp, token, "rl-session", 0, seg0, false, http.StatusAccepted)
	uploadReplay(t, sessionApp, token, "rl-session", 1, seg0, false, http.StatusTooManyRequests)
	uploadReplay(t, sessionApp, token, "rl-session-other", 0, seg0, false, http.StatusAccepted)
	if n := shared.calls.Load(); n != 0 {
		t.Fatalf("replay ingest consulted shared Redis limiter %d times", n)
	}
}

func gzipWithComment(t *testing.T, data []byte, comment string, level int) []byte {
	t.Helper()
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, level)
	if err != nil {
		t.Fatal(err)
	}
	zw.Comment = comment
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func readReplayBlobBytes(t *testing.T, blobs blob.Store, key string) []byte {
	t.Helper()
	r, err := blobs.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get blob %s: %v", key, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read blob %s: %v", key, err)
	}
	return data
}

func gunzipSingleMember(t *testing.T, raw []byte) []byte {
	t.Helper()
	br := bytes.NewReader(raw)
	zr, err := gzip.NewReader(br)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	zr.Multistream(false)
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read single gzip member: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("close gzip reader: %v", err)
	}
	if br.Len() != 0 {
		t.Fatalf("stored blob had %d trailing bytes after single gzip member", br.Len())
	}
	return decoded
}

func TestReplayMetadataHeaderAndQueryContract(t *testing.T) {
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
		Tokens: store, Store: store, Replays: store, Blobs: blobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	payload := []byte(fmt.Sprintf(`[{"type":4,"data":{"href":"https://example.test/m"},"timestamp":%d}]`,
		fixedNow.UnixMilli()))
	gz := gzipBytes(t, payload)

	cases := []struct {
		name          string
		target        string
		headerSession []string
		headerSeq     []string
		wantStatus    int
		wantSeq       int
	}{
		{
			name:          "header_only",
			target:        "/api/replay/segments",
			headerSession: []string{"hdr-only"},
			headerSeq:     []string{"0"},
			wantStatus:    http.StatusAccepted,
			wantSeq:       0,
		},
		{
			name:       "query_fallback_only",
			target:     "/api/replay/segments?session_id=qry-only&seq=1",
			wantStatus: http.StatusAccepted,
			wantSeq:    1,
		},
		{
			name:          "mixed_header_session_and_query_seq",
			target:        "/api/replay/segments?seq=2",
			headerSession: []string{"mixed-1"},
			wantStatus:    http.StatusAccepted,
			wantSeq:       2,
		},
		{
			name:       "mixed_query_session_and_header_seq",
			target:     "/api/replay/segments?session_id=mixed-2",
			headerSeq:  []string{"3"},
			wantStatus: http.StatusAccepted,
			wantSeq:    3,
		},
		{
			name:          "matching_header_and_query_with_whitespace",
			target:        "/api/replay/segments?session_id=%20agree-sess%20&seq=%204%20",
			headerSession: []string{" agree-sess "},
			headerSeq:     []string{" 4 "},
			wantStatus:    http.StatusAccepted,
			wantSeq:       4,
		},
		{
			name:          "mismatch_session_returns_400",
			target:        "/api/replay/segments?session_id=sess-a&seq=0",
			headerSession: []string{"sess-b"},
			headerSeq:     []string{"0"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "mismatch_seq_returns_400",
			target:        "/api/replay/segments?session_id=sess-m&seq=0",
			headerSession: []string{"sess-m"},
			headerSeq:     []string{"1"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "leading_zero_seq_mismatch_returns_400",
			target:        "/api/replay/segments?session_id=sess-lz&seq=1",
			headerSession: []string{"sess-lz"},
			headerSeq:     []string{"01"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "plus_sign_seq_mismatch_returns_400",
			target:        "/api/replay/segments?session_id=sess-plus&seq=1",
			headerSession: []string{"sess-plus"},
			headerSeq:     []string{"+1"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "invalid_header_seq_does_not_fallback_to_query",
			target:        "/api/replay/segments?session_id=sess-inv&seq=0",
			headerSession: []string{"sess-inv"},
			headerSeq:     []string{"abc"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "duplicate_session_headers_returns_400",
			target:        "/api/replay/segments",
			headerSession: []string{"dup-a", "dup-b"},
			headerSeq:     []string{"0"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "duplicate_seq_headers_returns_400",
			target:        "/api/replay/segments",
			headerSession: []string{"dup-seq"},
			headerSeq:     []string{"0", "1"},
			wantStatus:    http.StatusBadRequest,
		},
		{
			name:          "header_seq_over_quota_returns_413",
			target:        "/api/replay/segments",
			headerSession: []string{"quota-hdr"},
			headerSeq:     []string{strconv.Itoa(maxReplaySegmentsPerSession)},
			wantStatus:    http.StatusRequestEntityTooLarge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.target, bytes.NewReader(gz))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Encoding", "gzip")
			for _, v := range tc.headerSession {
				req.Header.Add(replaySessionHeader, v)
			}
			for _, v := range tc.headerSeq {
				req.Header.Add(replaySeqHeader, v)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			body := readBody(resp)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantStatus == http.StatusAccepted {
				var out struct {
					Accepted bool `json:"accepted"`
					Seq      int  `json:"seq"`
				}
				if err := json.Unmarshal(body, &out); err != nil || !out.Accepted || out.Seq != tc.wantSeq {
					t.Fatalf("unexpected 202 body=%s err=%v", body, err)
				}
			}
		})
	}
}

func TestReplaySinglePassGzipPassthroughAndFallbacks(t *testing.T) {
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
	reader := userQueryToken(t, store, "replay-pass-reader")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: blobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	project, err := store.ProjectBySlug(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	// Build a compressible segment and tag its gzip header with a distinctive comment:
	// passthrough preserves the exact client gzip bytes (including the comment), whereas
	// server re-compression produces fresh bytes without the comment.
	decoded := []byte(fmt.Sprintf(
		`[{"type":4,"data":{"href":"https://example.test/pass"},"timestamp":%d},`+
			`{"type":3,"data":{"source":1,"text":%q},"timestamp":%d}]`,
		fixedNow.Add(-time.Minute).UnixMilli(),
		strings.Repeat("compressible-replay-event-", 20),
		fixedNow.UnixMilli(),
	))
	digest := sha256.Sum256(decoded)
	wantSHA := hex.EncodeToString(digest[:])

	for i, enc := range []string{"gzip", "x-gzip"} {
		sessID := "pass-" + enc
		rawGzip := gzipWithComment(t, decoded, "client-passthrough-"+enc, gzip.BestCompression)
		if len(rawGzip) >= len(decoded) {
			t.Fatalf("fixture not smaller when compressed: raw=%d decoded=%d", len(rawGzip), len(decoded))
		}
		req := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(rawGzip))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Encoding", enc)
		req.Header.Set(replaySessionHeader, sessID)
		req.Header.Set(replaySeqHeader, strconv.Itoa(i))
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(resp)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s status=%d want=202 body=%s", enc, resp.StatusCode, body)
		}

		stored := readReplayBlobBytes(t, blobs, fmt.Sprintf("replay/default/%s/%d.json.gz", sessID, i))
		if !bytes.Equal(stored, rawGzip) {
			t.Fatalf("%s stored blob was re-compressed instead of passed through (stored=%d raw=%d)",
				enc, len(stored), len(rawGzip))
		}

		segMeta, err := store.GetReplaySegment(ctx, project.ID, sessID, i)
		if err != nil {
			t.Fatal(err)
		}
		if segMeta.SizeBytes != int64(len(rawGzip)) || segMeta.PayloadSha256 != wantSHA {
			t.Fatalf("%s seg meta mismatch: size=%d want=%d sha=%s want=%s",
				enc, segMeta.SizeBytes, len(rawGzip), segMeta.PayloadSha256, wantSHA)
		}

		// Idempotent retry with identity encoding must match sha256(decoded) and return 202.
		idReq := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(decoded))
		idReq.Header.Set("Authorization", "Bearer "+token)
		idReq.Header.Set("X-LO-Replay-Compression", "identity")
		idReq.Header.Set(replaySessionHeader, sessID)
		idReq.Header.Set(replaySeqHeader, strconv.Itoa(i))
		idResp, err := app.Test(idReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = idResp.Body.Close()
		if idResp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s idempotent identity retry status=%d want=202", enc, idResp.StatusCode)
		}

		// Conflict check: same (session, seq) with different decoded payload returns 409.
		diffReq := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader([]byte(`[]`)))
		diffReq.Header.Set("Authorization", "Bearer "+token)
		diffReq.Header.Set("X-LO-Replay-Compression", "identity")
		diffReq.Header.Set(replaySessionHeader, sessID)
		diffReq.Header.Set(replaySeqHeader, strconv.Itoa(i))
		diffResp, err := app.Test(diffReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = diffResp.Body.Close()
		if diffResp.StatusCode != http.StatusConflict {
			t.Fatalf("%s conflict status=%d want=409", enc, diffResp.StatusCode)
		}

		// Round-trip GET must return gzip that decompresses to exact decoded bytes.
		getResp := replayRequest(
			t,
			app,
			reader,
			"default",
			fmt.Sprintf("/api/replay/sessions/%s/segments/%d", sessID, i),
		)
		if getResp.StatusCode != http.StatusOK || getResp.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf(
				"%s GET segment status=%d encoding=%q",
				enc,
				getResp.StatusCode,
				getResp.Header.Get("Content-Encoding"),
			)
		}
		roundTripBody := readBody(getResp)
		if !bytes.Equal(gunzipSingleMember(t, roundTripBody), decoded) {
			t.Fatalf("%s round-trip decoded bytes mismatch", enc)
		}
	}

	// Fallback re-compression cases (must accept 202 and NOT store the raw upload bytes):
	// 1. len(rawGzip) >= len(decoded) using gzip.NoCompression on small JSON.
	incompressible := gzipWithComment(t, []byte(`[{"type":4,"timestamp":1000}]`), "no-pass-nocomp", gzip.NoCompression)
	reqNoComp := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(incompressible))
	reqNoComp.Header.Set("Authorization", "Bearer "+token)
	reqNoComp.Header.Set("Content-Encoding", "gzip")
	reqNoComp.Header.Set(replaySessionHeader, "fallback-sess")
	reqNoComp.Header.Set(replaySeqHeader, "0")
	respNoComp, err := app.Test(reqNoComp)
	if err != nil {
		t.Fatal(err)
	}
	_ = respNoComp.Body.Close()
	if respNoComp.StatusCode != http.StatusAccepted {
		t.Fatalf("NoCompression status=%d want=202", respNoComp.StatusCode)
	}
	storedNoComp := readReplayBlobBytes(t, blobs, "replay/default/fallback-sess/0.json.gz")
	if bytes.Equal(storedNoComp, incompressible) {
		t.Fatal("stored blob passed through raw gzip even though len(rawGzip) >= len(decoded)")
	}
	if !bytes.Equal(gunzipSingleMember(t, storedNoComp), []byte(`[{"type":4,"timestamp":1000}]`)) {
		t.Fatal("re-compressed NoCompression blob content mismatch")
	}

	// 2. Multi-layer Content-Encoding: gzip, gzip must decode and re-compress cleanly.
	innerGz := gzipWithComment(t, decoded, "inner-comment", gzip.DefaultCompression)
	doubleGz := gzipWithComment(t, innerGz, "outer-comment", gzip.DefaultCompression)
	reqMultiEnc := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(doubleGz))
	reqMultiEnc.Header.Set("Authorization", "Bearer "+token)
	reqMultiEnc.Header.Set("Content-Encoding", "gzip, gzip")
	reqMultiEnc.Header.Set(replaySessionHeader, "fallback-sess")
	reqMultiEnc.Header.Set(replaySeqHeader, "1")
	respMultiEnc, err := app.Test(reqMultiEnc)
	if err != nil {
		t.Fatal(err)
	}
	_ = respMultiEnc.Body.Close()
	if respMultiEnc.StatusCode != http.StatusAccepted {
		t.Fatalf("gzip,gzip status=%d want=202", respMultiEnc.StatusCode)
	}
	storedMultiEnc := readReplayBlobBytes(t, blobs, "replay/default/fallback-sess/1.json.gz")
	if bytes.Equal(storedMultiEnc, doubleGz) || bytes.Equal(storedMultiEnc, innerGz) {
		t.Fatal("multi-layer gzip bypassed re-compression")
	}
	if !bytes.Equal(gunzipSingleMember(t, storedMultiEnc), decoded) {
		t.Fatal("re-compressed multi-layer gzip content mismatch")
	}

	// 3. Valid multi-member gzip stream must bypass passthrough and be stored as a single clean member.
	partA := []byte(`[{"type":4,"data":{"href":"https://example.test/multi"},"timestamp":1000},`)
	partB := []byte(fmt.Sprintf(`{"type":3,"data":{"text":%q},"timestamp":2000}]`, strings.Repeat("m", 200)))
	multiMember := append(gzipWithComment(t, partA, "m1", gzip.BestCompression),
		gzipWithComment(t, partB, "m2", gzip.BestCompression)...)
	reqMultiMem := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(multiMember))
	reqMultiMem.Header.Set("Authorization", "Bearer "+token)
	reqMultiMem.Header.Set("Content-Encoding", "gzip")
	reqMultiMem.Header.Set(replaySessionHeader, "fallback-sess")
	reqMultiMem.Header.Set(replaySeqHeader, "2")
	respMultiMem, err := app.Test(reqMultiMem)
	if err != nil {
		t.Fatal(err)
	}
	_ = respMultiMem.Body.Close()
	if respMultiMem.StatusCode != http.StatusAccepted {
		t.Fatalf("multi-member gzip status=%d want=202", respMultiMem.StatusCode)
	}
	storedMultiMem := readReplayBlobBytes(t, blobs, "replay/default/fallback-sess/2.json.gz")
	if bytes.Equal(storedMultiMem, multiMember) {
		t.Fatal("multi-member gzip passed through raw bytes instead of re-compressing to single member")
	}
	wantCombined := append(append([]byte(nil), partA...), partB...)
	if !bytes.Equal(gunzipSingleMember(t, storedMultiMem), wantCombined) {
		t.Fatal("re-compressed multi-member gzip content mismatch")
	}

	// 4. Strict rejection of malformed, truncated, trailing-garbage, zip-bomb, and unsupported-encoding inputs.
	validSingle := gzipBytes(t, decoded)
	badCRC := append([]byte(nil), validSingle...)
	badCRC[len(badCRC)-5] ^= 0xff
	singleBomb := gzipOfZeros(t, maxReplaySegment+1)
	multiBomb := append(gzipOfZeros(t, (maxReplaySegment/2)+100), gzipOfZeros(t, (maxReplaySegment/2)+100)...)

	errorCases := []struct {
		name     string
		encoding string
		body     []byte
		want     int
	}{
		{
			"trailing_garbage_text",
			"gzip",
			append(append([]byte(nil), validSingle...), []byte("garbage")...),
			http.StatusBadRequest,
		},
		{"trailing_null_byte", "gzip", append(append([]byte(nil), validSingle...), 0x00), http.StatusBadRequest},
		{
			"multi_member_corrupt_second",
			"gzip",
			append(append([]byte(nil), validSingle...), validSingle[:len(validSingle)-4]...),
			http.StatusBadRequest,
		},
		{"corrupt_header", "gzip", []byte("not-gzip"), http.StatusBadRequest},
		{"truncated_stream", "gzip", validSingle[:len(validSingle)-6], http.StatusBadRequest},
		{"bad_crc32", "gzip", badCRC, http.StatusBadRequest},
		{"single_member_zip_bomb", "gzip", singleBomb, http.StatusRequestEntityTooLarge},
		{"multi_member_zip_bomb", "gzip", multiBomb, http.StatusRequestEntityTooLarge},
		{"unsupported_encoding_lz4", "lz4", []byte(`[]`), http.StatusUnsupportedMediaType},
	}
	for idx, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Encoding", tc.encoding)
			req.Header.Set(replaySessionHeader, "err-sess")
			req.Header.Set(replaySeqHeader, strconv.Itoa(idx))
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestReplayGzipPassthroughByteRateLimitAccounting(t *testing.T) {
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
	limiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 100,
		Burst:             100,
		BytesPerSecond:    1000,
		ByteBurst:         1000,
		Now:               func() time.Time { return fixedNow },
	})
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: blobs,
		IngestLimiter: limiter,
		Now:           func() time.Time { return fixedNow },
	}).App()

	plain := []byte(fmt.Sprintf(
		`[{"type":3,"data":{"text":%q},"timestamp":%d}]`,
		strings.Repeat("x", 750),
		fixedNow.UnixMilli(),
	))
	if len(plain) < 750 || len(plain) > 950 {
		t.Fatalf("unexpected decoded len=%d", len(plain))
	}
	gz := gzipBytes(t, plain)
	if len(gz) >= 200 {
		t.Fatalf("expected compressed len < 200, got %d", len(gz))
	}

	req1 := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(gz))
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("Content-Encoding", "gzip")
	req1.Header.Set(replaySessionHeader, "rl-decoded")
	req1.Header.Set(replaySeqHeader, "0")
	resp1, err := app.Test(req1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first passthrough status=%d want=202", resp1.StatusCode)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(gz))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Encoding", "gzip")
	req2.Header.Set(replaySessionHeader, "rl-decoded")
	req2.Header.Set(replaySeqHeader, "1")
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests || resp2.Header.Get("Retry-After") == "" {
		t.Fatalf("second passthrough status=%d retry-after=%q want=429 with Retry-After",
			resp2.StatusCode, resp2.Header.Get("Retry-After"))
	}
}

func TestReplayConcurrentPassthroughAndFallbackRace(t *testing.T) {
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
	reader := userQueryToken(t, store, "replay-race-reader")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	app := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Replays: store, Blobs: blobs,
		Now: func() time.Time { return fixedNow },
	}).App()

	const workers = 12
	expected := make(map[string][]byte, workers)
	var wg sync.WaitGroup
	for i := range workers {
		sessID := fmt.Sprintf("race-sess-%d", i)
		plain := []byte(fmt.Sprintf(
			`[{"type":3,"data":{"text":%q},"timestamp":%d}]`,
			fmt.Sprintf("worker-%d-%s", i, strings.Repeat("r", 120)),
			fixedNow.UnixMilli()+int64(i),
		))
		expected[sessID] = plain
		wg.Add(1)
		go func(id int, sID string, p []byte) {
			defer wg.Done()
			var body []byte
			var encHeader, compHeader string
			switch id % 3 {
			case 0:
				body = gzipWithComment(t, p, "race-pass", gzip.BestCompression)
				encHeader = "gzip"
			case 1:
				body = gzipWithComment(t, p, "race-nocomp", gzip.NoCompression)
				encHeader = "gzip"
			case 2:
				body = p
				compHeader = "identity"
			}
			req := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set(replaySessionHeader, sID)
			req.Header.Set(replaySeqHeader, "0")
			if encHeader != "" {
				req.Header.Set("Content-Encoding", encHeader)
			}
			if compHeader != "" {
				req.Header.Set("X-LO-Replay-Compression", compHeader)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Errorf("worker %d request err=%v", id, err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Errorf("worker %d status=%d want=202", id, resp.StatusCode)
			}

			// Interleave an invalid gzip upload to stress pool reset under concurrency.
			badReq := httptest.NewRequest(http.MethodPost, "/api/replay/segments", bytes.NewReader([]byte("bad")))
			badReq.Header.Set("Authorization", "Bearer "+token)
			badReq.Header.Set("Content-Encoding", "gzip")
			badReq.Header.Set(replaySessionHeader, "race-bad-"+strconv.Itoa(id))
			badReq.Header.Set(replaySeqHeader, "0")
			badResp, err := app.Test(badReq)
			if err == nil {
				_ = badResp.Body.Close()
			}
		}(i, sessID, plain)
	}
	wg.Wait()

	for sessID, want := range expected {
		resp := replayRequest(t, app, reader, "default", "/api/replay/sessions/"+sessID+"/segments/0")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s GET status=%d", sessID, resp.StatusCode)
		}
		got := gunzipSingleMember(t, readBody(resp))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s round-trip mismatch", sessID)
		}
	}
}
