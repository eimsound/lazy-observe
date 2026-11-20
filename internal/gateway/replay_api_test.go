package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
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
