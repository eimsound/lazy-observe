package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
)

type errorStorage struct {
	mu            sync.Mutex
	logs          map[string][]map[string]any
	metricSamples string
	metricImports int
}

func (s *errorStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get("ProjectID")
	switch r.URL.Path {
	case "/insert/jsonline":
		scanner := bufio.NewScanner(r.Body)
		scanner.Buffer(make([]byte, 64<<10), maxVLogsErrorLineBytes)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.logs == nil {
			s.logs = make(map[string][]map[string]any)
		}
		for scanner.Scan() {
			var record map[string]any
			_ = json.Unmarshal(scanner.Bytes(), &record)
			s.logs[tenant] = append(s.logs[tenant], record)
		}
		w.WriteHeader(http.StatusNoContent)
	case "/select/logsql/query":
		query := r.URL.Query().Get("query")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		s.mu.Lock()
		records := append([]map[string]any(nil), s.logs[tenant]...)
		s.mu.Unlock()
		if strings.Contains(query, "sort by (_time) desc") {
			// Sort matched records by timestamp descending so the mock reflects
			// LogsQL's sort pipe.
			for i := 0; i < len(records); i++ {
				for j := i + 1; j < len(records); j++ {
					ti, _ := records[i]["timestamp"].(string)
					tj, _ := records[j]["timestamp"].(string)
					if tj > ti {
						records[i], records[j] = records[j], records[i]
					}
				}
			}
		} else {
			// Reverse to keep legacy insertion order when unsorted.
			for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
				records[i], records[j] = records[j], records[i]
			}
		}
		encoder := json.NewEncoder(w)
		written := 0
		dedup := strings.Contains(query, "uniq by (event_id")
		seen := make(map[string]struct{})
		for _, record := range records {
			if !queryMatchesRecord(query, record) {
				continue
			}
			if dedup {
				if eid, _ := record["event_id"].(string); eid != "" {
					if _, dup := seen[eid]; dup {
						continue
					}
					seen[eid] = struct{}{}
				}
			}
			_ = encoder.Encode(record)
			written++
			if limit > 0 && written == limit {
				break
			}
		}
	case "/select/logsql/stats_query_range":
		query := r.URL.Query().Get("query")
		start, startErr := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
		end, endErr := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
		if startErr != nil || endErr != nil || !end.After(start) {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		records := append([]map[string]any(nil), s.logs[tenant]...)
		s.mu.Unlock()
		dedup := strings.Contains(query, "count_uniq(event_id)")
		type bucketKey struct {
			fingerprint string
			dayUnix     int64
		}
		counts := make(map[bucketKey]int64)
		seenEvents := make(map[string]struct{})
		byFP := make(map[string]map[int64]int64)
		for _, record := range records {
			fingerprint, _ := record["fingerprint"].(string)
			if fingerprint == "" || !strings.Contains(query, strconv.Quote(fingerprint)) {
				continue
			}
			atText, _ := record["timestamp"].(string)
			at, err := time.Parse(time.RFC3339Nano, atText)
			if err != nil || at.Before(start) || at.After(end) {
				continue
			}
			dayUnix := at.UTC().Truncate(24 * time.Hour).Unix()
			if dedup {
				if eid, _ := record["event_id"].(string); eid != "" {
					key := fmt.Sprintf("%s|%d|%s", fingerprint, dayUnix, eid)
					if _, dup := seenEvents[key]; dup {
						continue
					}
					seenEvents[key] = struct{}{}
				}
			}
			counts[bucketKey{fingerprint: fingerprint, dayUnix: dayUnix}]++
			if byFP[fingerprint] == nil {
				byFP[fingerprint] = make(map[int64]int64)
			}
			byFP[fingerprint][dayUnix] = counts[bucketKey{fingerprint: fingerprint, dayUnix: dayUnix}]
		}
		result := make([]map[string]any, 0, len(byFP))
		for fingerprint, days := range byFP {
			values := make([][]any, 0, len(days))
			for dayUnix, count := range days {
				values = append(values, []any{dayUnix, strconv.FormatInt(count, 10)})
			}
			result = append(result, map[string]any{
				"metric": map[string]string{"__name__": "hits", "fingerprint": fingerprint},
				"values": values,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "matrix", "result": result},
		})
	case "/select/logsql/hits":
		query := r.URL.Query().Get("query")
		start, startErr := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
		end, endErr := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
		if startErr != nil || endErr != nil || !end.After(start) {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		records := append([]map[string]any(nil), s.logs[tenant]...)
		s.mu.Unlock()
		byFingerprint := make(map[string][]int64)
		for _, record := range records {
			fingerprint, _ := record["fingerprint"].(string)
			if fingerprint == "" || !strings.Contains(query, strconv.Quote(fingerprint)) {
				continue
			}
			atText, _ := record["timestamp"].(string)
			at, err := time.Parse(time.RFC3339Nano, atText)
			if err != nil || at.Before(start) || at.After(end) {
				continue
			}
			values := byFingerprint[fingerprint]
			if values == nil {
				values = make([]int64, issueTrendBuckets)
			}
			bucket := int(at.Sub(start) / (24 * time.Hour))
			if bucket >= issueTrendBuckets {
				bucket = issueTrendBuckets - 1
			}
			values[bucket]++
			byFingerprint[fingerprint] = values
		}
		timestamps := make([]string, issueTrendBuckets)
		for i := range timestamps {
			timestamps[i] = start.Add(time.Duration(i) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
		}
		hits := make([]map[string]any, 0, len(byFingerprint))
		for fingerprint, values := range byFingerprint {
			var total int64
			for _, value := range values {
				total += value
			}
			hits = append(hits, map[string]any{
				"fields":     map[string]string{"fingerprint": fingerprint},
				"timestamps": timestamps, "values": values, "total": total,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": hits})
	case "/api/v1/import/prometheus":
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.metricSamples += string(body)
		s.metricImports++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func queryMatchesRecord(query string, record map[string]any) bool {
	for _, key := range []string{"fingerprint", "event_id"} {
		marker := key + `:="`
		start := strings.Index(query, marker)
		if start < 0 {
			continue
		}
		start += len(marker)
		end := strings.Index(query[start:], `"`)
		if end < 0 || record[key] != query[start:start+end] {
			return false
		}
	}
	return true
}

func TestErrorsIngestDurablyAcceptsBeforeDerivation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	store, err := meta.OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Durable", "durable")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "durable-admin", "password", "admin")
	storage := &errorStorage{}
	var writerUnavailable atomic.Bool
	writerUnavailable.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/insert/jsonline" && writerUnavailable.Load() {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		storage.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	first := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL})

	eventAt := time.Now().UTC().Truncate(time.Millisecond)
	postErrorsTest(t, first.App(), token, []any{map[string]any{
		"event_id": "durable-event", "timestamp": eventAt.Format(time.RFC3339Nano),
		"platform": "go", "level": "error",
		"exception": map[string]any{
			"type": "DurableError", "value": "survives restart",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "main.handle", "filename": "main.go", "lineno": 42, "in_app": true,
			}}},
		},
	}}, http.StatusOK)
	if err := first.Shutdown(); err != nil {
		t.Fatalf("shutdown first gateway: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	writerUnavailable.Store(false)
	reopened, err := meta.OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	second := newMetadataTestGateway(reopened, Config{Tokens: reopened, Store: reopened, Logs: upstream.URL})
	t.Cleanup(func() { _ = second.Shutdown() })

	var issue map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		issues := listIssuesTest(t, second.App(), cookie, project.Slug)
		if len(issues) == 1 && issues[0]["count"] == float64(1) {
			issue = issues[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if issue == nil {
		t.Fatal("durably accepted issue did not become queryable after restart")
	}
	issueID := int(issue["id"].(float64))
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/issues/"+strconv.Itoa(issueID)+"/events/durable-event",
		nil,
	)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err := second.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"event_id":"durable-event"`)) {
		t.Fatalf("event status=%d body=%s", resp.StatusCode, body)
	}
}

func TestErrorsIngestDeduplicatesEventID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Identity", "identity")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "identity-admin", "password", "admin")
	upstream := httptest.NewServer(&errorStorage{})
	t.Cleanup(upstream.Close)
	gateway := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL})
	t.Cleanup(func() { _ = gateway.Shutdown() })

	eventAt := time.Now().UTC().Truncate(time.Millisecond)
	first := map[string]any{
		"event_id": "stable-event", "timestamp": eventAt.Format(time.RFC3339Nano),
		"platform": "go", "level": "error",
		"exception": map[string]any{
			"type": "StableError", "value": "first accepted fact",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "main.stable", "filename": "main.go", "lineno": 7, "in_app": true,
			}}},
		},
	}
	postErrorsTest(t, gateway.App(), token, []any{first}, http.StatusOK)
	postErrorsTest(t, gateway.App(), token, []any{first}, http.StatusOK)

	conflict := map[string]any{
		"event_id": "stable-event", "timestamp": eventAt.Format(time.RFC3339Nano),
		"platform": "go", "level": "error",
		"exception": map[string]any{
			"type": "StableError", "value": "conflicting replacement",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "main.stable", "filename": "main.go", "lineno": 7, "in_app": true,
			}}},
		},
	}
	postErrorsTest(t, gateway.App(), token, []any{conflict}, http.StatusConflict)
	newEvent := map[string]any{
		"event_id": "must-not-be-accepted", "timestamp": eventAt.Format(time.RFC3339Nano),
		"platform": "go", "level": "error",
		"exception": map[string]any{
			"type": "AtomicBatchError", "value": "rolled back",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "main.atomic", "filename": "atomic.go", "lineno": 9, "in_app": true,
			}}},
		},
	}
	postErrorsTest(t, gateway.App(), token, []any{newEvent, conflict}, http.StatusConflict)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		issues := listIssuesTest(t, gateway.App(), cookie, project.Slug)
		if len(issues) == 1 && issues[0]["count"] == float64(1) &&
			issues[0]["title"] == "StableError: first accepted fact" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	issues := listIssuesTest(t, gateway.App(), cookie, project.Slug)
	t.Fatalf("event identity projection=%#v", issues)
}

func TestIssueHTTPWireContract(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Default", "default")
	other, _ := store.CreateProject(ctx, "Other", "other")
	token, _ := store.CreateProjectAPIToken(ctx, project.ID)
	otherToken, _ := store.CreateProjectAPIToken(ctx, other.ID)
	cookie := sessionCookie(t, store, "issues-contract-admin", "password", "admin")
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	defer upstream.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Logs: upstream.URL,
		Now: func() time.Time { return now },
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	eventAt := now.Add(-time.Hour)
	event := map[string]any{
		"event_id": "contract-event", "timestamp": eventAt.Format(time.RFC3339Nano),
		"platform": "browser", "level": "error", "message": "contract failed",
		"exception": map[string]any{
			"type": "TypeError", "value": "contract failed",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "submitOrder", "filename": "src/checkout.ts", "lineno": 42, "colno": 7, "in_app": true,
			}}},
		},
		"contexts": map[string]any{
			"trace":   map[string]any{"trace_id": "trace-contract", "span_id": "span-contract"},
			"browser": map[string]any{"name": "Firefox", "version": "128"},
			"os":      map[string]any{"name": "Linux", "version": "6.9"},
		},
		"session_id": "session-contract", "tags": map[string]string{"release": "web@1"},
		"url": "https://example.test/checkout", "user": map[string]string{"id": "user-contract", "ip": "127.0.0.1"},
	}
	postErrorsTest(t, gw.App(), token, []any{event}, http.StatusOK)
	otherEvent := map[string]any{
		"event_id":  "other-event",
		"timestamp": eventAt.Format(time.RFC3339Nano),
		"platform":  "go",
		"level":     "error",
		"exception": map[string]any{
			"type":  "OtherProjectError",
			"value": "private",
			"stacktrace": map[string]any{
				"frames": []map[string]any{{"function": "other", "filename": "other.go", "in_app": true}},
			},
		},
	}
	postErrorsTest(t, gw.App(), otherToken, []any{otherEvent}, http.StatusOK)
	waitForIssueLengthTest(t, gw.App(), cookie, "default", 1)

	req := httptest.NewRequest(http.MethodGet, "/api/issues?status=open&page=1", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", "default")
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(`"project_id"`)) || bytes.Contains(body, []byte(`"edges"`)) ||
		bytes.Contains(body, []byte("OtherProjectError")) {
		t.Fatalf("list leaked internal or cross-project data: %s", body)
	}
	var listing struct {
		Issues []struct {
			ID        int     `json:"id"`
			FirstSeen int64   `json:"first_seen"`
			LastSeen  int64   `json:"last_seen"`
			Trend     []int64 `json:"trend"`
		} `json:"issues"`
		Total    int `json:"total"`
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("decode list: %v body=%s", err, body)
	}
	if listing.Total != 1 || listing.Page != 1 || listing.PageSize != meta.IssuePageSize || len(listing.Issues) != 1 {
		t.Fatalf("pagination=%#v", listing)
	}
	issue := listing.Issues[0]
	if issue.FirstSeen != eventAt.UnixMilli() || issue.LastSeen != eventAt.UnixMilli() ||
		len(issue.Trend) != issueTrendBuckets {
		t.Fatalf("issue timestamps/trend=%#v", issue)
	}
	var trendTotal int64
	for _, count := range issue.Trend {
		trendTotal += count
	}
	if trendTotal != 1 {
		t.Fatalf("trend=%v total=%d", issue.Trend, trendTotal)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(issue.ID), nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", "default")
	resp, err = gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var detail struct {
		Culprit string `json:"culprit"`
		Events  []struct {
			EventID   string `json:"event_id"`
			Timestamp int64  `json:"timestamp"`
			TraceID   string `json:"trace_id"`
			SessionID string `json:"session_id"`
		} `json:"events"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &detail) != nil ||
		detail.Culprit != "submitOrder(src/checkout.ts)" ||
		len(detail.Events) != 1 {
		t.Fatalf("detail status=%d detail=%#v body=%s", resp.StatusCode, detail, body)
	}
	if got := detail.Events[0]; got.EventID != "contract-event" || got.Timestamp != eventAt.UnixMilli() ||
		got.TraceID != "trace-contract" ||
		got.SessionID != "session-contract" {
		t.Fatalf("event summary=%#v", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(issue.ID)+"/events/contract-event", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", "default")
	resp, err = gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var occurrence issueEventDTO
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &occurrence) != nil {
		t.Fatalf("event status=%d body=%s", resp.StatusCode, body)
	}
	if occurrence.Timestamp != eventAt.UnixMilli() || occurrence.Contexts.Trace == nil ||
		occurrence.Contexts.Browser == nil ||
		occurrence.Contexts.OS == nil ||
		occurrence.User == nil ||
		occurrence.Tags["release"] != "web@1" {
		t.Fatalf("event contract=%#v", occurrence)
	}
}

func TestErrorTrackingLifecycleRawEventsIsolationAndVitals(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	p1, _ := store.CreateProject(ctx, "Default", "default")
	p2, _ := store.CreateProject(ctx, "P2", "p2")
	token1, _ := store.CreateProjectAPIToken(ctx, p1.ID)
	token2, _ := store.CreateProjectAPIToken(ctx, p2.ID)
	cookie := sessionCookie(t, store, "issues-admin", "password", "admin")
	viewerCookie := sessionCookie(t, store, "issues-viewer", "password", "viewer")
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	defer upstream.Close()
	gw := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, Logs: upstream.URL, Metrics: upstream.URL},
	)
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()

	baseEvent := func(id, typ, value string, line int) map[string]any {
		return map[string]any{
			"event_id":  id,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"platform":  "go",
			"level":     "error",
			"message":   "request failed",
			"exception": map[string]any{
				"type":  typ,
				"value": value,
				"stacktrace": map[string]any{
					"frames": []map[string]any{
						{
							"function": "svc.Handle.func1",
							"filename": "/build/svc/handler.go",
							"lineno":   line,
							"in_app":   true,
						},
					},
				},
			},
			"contexts":   map[string]any{"trace": map[string]any{"trace_id": "trace-abc", "span_id": "span-def"}},
			"session_id": "user-1",
		}
	}
	postErrorsTest(
		t,
		app,
		token1,
		[]any{
			baseEvent("e1", "panic", "user 1", 10),
			baseEvent("e2", "panic", "user 2", 20),
			baseEvent("e3", "panic", "user 3", 30),
		},
		http.StatusOK,
	)
	postErrorsTest(t, app, token1, []any{baseEvent("e-other", "TypeError", "user 1", 10)}, http.StatusOK)
	issues := waitForIssueLengthTest(t, app, cookie, "default", 2)
	if len(issues) != 2 {
		t.Fatalf("issues=%#v", issues)
	}
	group := issueWithCount(issues, 3)
	if group == nil {
		t.Fatalf("missing count=3 group: %#v", issues)
	}
	id := int(group["id"].(float64))
	if got := len(listIssuesTest(t, app, viewerCookie, "default")); got != 2 {
		t.Fatalf("viewer issue count=%d", got)
	}
	viewerPatch, _ := json.Marshal(map[string]string{"status": "resolved"})
	req := httptest.NewRequest(http.MethodPatch, "/api/issues/"+strconv.Itoa(id), bytes.NewReader(viewerPatch))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LO-Project", "default")
	req.AddCookie(viewerCookie)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer patch status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	req = httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	req.Header.Set("Authorization", "Bearer "+token1)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token issue read status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	patchIssueTest(t, app, cookie, "default", id, "resolved")
	postErrorsTest(t, app, token1, []any{baseEvent("e4", "panic", "dynamic 4", 99)}, http.StatusOK)
	group = waitForIssueCountTest(t, app, cookie, "default", 4)
	if group == nil || group["status"] != "open" || group["regressed_at"] == nil {
		t.Fatalf("regression=%#v", group)
	}
	patchIssueTest(t, app, cookie, "default", id, "ignored")
	postErrorsTest(t, app, token1, []any{baseEvent("e5", "panic", "dynamic 5", 101)}, http.StatusOK)
	group = waitForIssueCountTest(t, app, cookie, "default", 5)
	if group == nil || group["status"] != "ignored" {
		t.Fatalf("ignored=%#v", group)
	}

	postErrorsTest(t, app, token2, []any{baseEvent("p2-event", "P2Only", "secret", 1)}, http.StatusOK)
	if got := len(listIssuesTest(t, app, cookie, "default")); got != 2 {
		t.Fatalf("default issue count after p2=%d", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(id)+"/events/e1", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", "default")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"trace_id":"trace-abc"`)) {
		t.Fatalf("raw event status=%d body=%s", resp.StatusCode, raw)
	}

	vitals := map[string]any{"vitals": []any{
		map[string]any{
			"name":      "LCP",
			"value":     123.4,
			"page":      "/checkout?x=1",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}
	body, _ := json.Marshal(vitals)
	req = httptest.NewRequest(http.MethodPost, "/api/events/vitals", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token1)
	resp, err = app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("vitals status=%d err=%v", resp.StatusCode, err)
	}
	storage.mu.Lock()
	samples := storage.metricSamples
	storage.mu.Unlock()
	if !strings.Contains(samples, `web_vitals_lcp{project_id="default",page="/checkout"} 123.4`) {
		t.Fatalf("samples=%q", samples)
	}
}

type metricIngestSetup struct {
	gateway *Gateway
	token   string
	storage *errorStorage
	logs    *strings.Builder
}

// newMetricIngestGateway wires a gateway whose metrics upstream records the Prometheus
// payload, so vitals and SDK diagnostics tests can assert both on the HTTP response and
// on what actually reached storage.
func newMetricIngestGateway(t *testing.T, slug string) metricIngestSetup {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	t.Cleanup(upstream.Close)
	logs := &strings.Builder{}
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Metrics: upstream.URL,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	t.Cleanup(func() { _ = gw.Shutdown() })
	return metricIngestSetup{gateway: gw, token: token, storage: storage, logs: logs}
}

func (s metricIngestSetup) postVitals(t *testing.T, vitals []any) (int, []byte) {
	t.Helper()
	return s.postJSON(t, "/api/events/vitals", map[string]any{"vitals": vitals})
}

func (s metricIngestSetup) postDiagnostics(t *testing.T, measurements []any) (int, []byte) {
	t.Helper()
	return s.postJSON(t, "/api/events/diagnostics", map[string]any{"measurements": measurements})
}

func (s metricIngestSetup) postJSON(t *testing.T, path string, body map[string]any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.token)
	response, err := s.gateway.App().Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, payload
}

func (s metricIngestSetup) samples() string {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	return s.storage.metricSamples
}

// importCount separates "imported an empty payload" from "did not call the metrics
// upstream at all", which samples() cannot tell apart because it only concatenates.
func (s metricIngestSetup) importCount() int {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	return s.storage.metricImports
}

func vitalPayload(name string, value float64, page string, at time.Time) map[string]any {
	return map[string]any{
		"name": name, "value": value, "page": page, "timestamp": at.Format(time.RFC3339Nano),
	}
}

func TestVitalsIngestAcceptsKnownMetricsAndSkipsUnknownNames(t *testing.T) {
	setup := newMetricIngestGateway(t, "vitals-partial")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postVitals(t, []any{
		vitalPayload("LCP", 123.4, "/checkout", at),
		vitalPayload("LOAF", 42, "/checkout", at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 1 || result.Rejected != 1 {
		t.Fatalf("response=%s want accepted=1 rejected=1", payload)
	}

	samples := setup.samples()
	want := `web_vitals_lcp{project_id="vitals-partial",page="/checkout"} 123.4 1787488496789`
	if !strings.Contains(samples, want) {
		t.Fatalf("samples=%q want substring=%q", samples, want)
	}
	if strings.Contains(strings.ToLower(samples), "loaf") {
		t.Fatalf("unknown vital reached storage: %q", samples)
	}
	if logged := setup.logs.String(); !strings.Contains(logged, "loaf") ||
		!strings.Contains(logged, "vitals-partial") {
		t.Fatalf("missing warn log with project and skipped name: %q", logged)
	}
	if imports := setup.importCount(); imports != 1 {
		t.Fatalf("metrics imports=%d want 1", imports)
	}
}

func TestVitalsIngestAllUnknownNamesWritesNothing(t *testing.T) {
	setup := newMetricIngestGateway(t, "vitals-all-unknown")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postVitals(t, []any{
		vitalPayload("LOAF", 42, "/checkout", at),
		vitalPayload("INP_Experimental", 7, "/checkout", at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 0 || result.Rejected != 2 {
		t.Fatalf("response=%s want accepted=0 rejected=2", payload)
	}
	if samples := setup.samples(); samples != "" {
		t.Fatalf("fully rejected batch reached storage: %q", samples)
	}
	if imports := setup.importCount(); imports != 0 {
		t.Fatalf("metrics imports=%d want 0: a fully rejected batch must not POST to the upstream", imports)
	}
	logged := setup.logs.String()
	if !strings.Contains(logged, "loaf") || !strings.Contains(logged, "inp_experimental") {
		t.Fatalf("warn log missing skipped names: %q", logged)
	}
}

func TestVitalsIngestEmptyBatchWritesNothing(t *testing.T) {
	setup := newMetricIngestGateway(t, "vitals-empty")

	status, payload := setup.postVitals(t, []any{})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 0 || result.Rejected != 0 {
		t.Fatalf("response=%s want accepted=0 rejected=0", payload)
	}
	if imports := setup.importCount(); imports != 0 {
		t.Fatalf("metrics imports=%d want 0: an empty batch must not POST to the upstream", imports)
	}
	if logged := setup.logs.String(); logged != "" {
		t.Fatalf("empty batch logged: %q", logged)
	}
}

func TestVitalsIngestBoundsUnknownNameLogging(t *testing.T) {
	setup := newMetricIngestGateway(t, "vitals-flood")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	// "指" is three bytes, so a 64-byte cut lands mid-rune at 63 bytes and must be
	// backed up to 21 whole runes.
	longName := strings.Repeat("指", 2000)
	wantPrefix := strings.Repeat("指", 21) + "…"

	status, payload := setup.postVitals(t, []any{
		vitalPayload(longName, 1, "/checkout", at),
		vitalPayload(longName+"a", 1, "/checkout", at),
		vitalPayload(longName+"b", 1, "/checkout", at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 0 || result.Rejected != 3 {
		t.Fatalf("response=%s want accepted=0 rejected=3", payload)
	}
	logged := setup.logs.String()
	if strings.Count(logged, wantPrefix) != 1 {
		t.Fatalf("log=%q want exactly one truncated name %q (de-duplicated after truncation)",
			logged, wantPrefix)
	}
	if strings.Contains(logged, strings.Repeat("指", 30)) {
		t.Fatalf("log kept an untruncated name: %q", logged)
	}
	if len(logged) > 1024 {
		t.Fatalf("log line=%d bytes, want a bounded warning: %q", len(logged), logged)
	}
}

func TestVitalsIngestSummarizesOverflowingUnknownNames(t *testing.T) {
	setup := newMetricIngestGateway(t, "vitals-overflow")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	var vitals []any
	for _, name := range []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"} {
		vitals = append(vitals, vitalPayload(name, 1, "/checkout", at))
	}

	status, payload := setup.postVitals(t, vitals)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	logged := setup.logs.String()
	// Sorted so the warning is deterministic, capped at five names, remainder counted.
	if !strings.Contains(logged, "aaa,bbb,ccc,ddd,eee,+2 more") {
		t.Fatalf("log=%q want sorted cap with overflow count", logged)
	}
	if strings.Contains(logged, "fff") || strings.Contains(logged, "ggg") {
		t.Fatalf("log=%q kept names beyond the cap", logged)
	}
}

func TestVitalsIngestKeepsStructuralErrorsAsWholeBatchRejection(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	for _, tc := range []struct {
		name    string
		vital   map[string]any
		wantErr string
	}{
		{
			name:    "empty page",
			vital:   vitalPayload("LCP", 1, "", at),
			wantErr: "invalid vital at index 0",
		},
		{
			name:    "empty name",
			vital:   vitalPayload("", 1, "/checkout", at),
			wantErr: "invalid vital at index 0",
		},
		{
			name:    "blank name",
			vital:   vitalPayload("   ", 1, "/checkout", at),
			wantErr: "invalid vital at index 0",
		},
		{
			name:    "invalid timestamp",
			vital:   map[string]any{"name": "LCP", "value": 1, "page": "/checkout", "timestamp": "yesterday"},
			wantErr: "invalid vital timestamp at index 0",
		},
		{
			// Structural validity is a property of the batch: an unknown name is
			// skipped, but a malformed entry still rejects the request.
			name:    "unknown name with invalid timestamp",
			vital:   map[string]any{"name": "LOAF", "value": 1, "page": "/checkout", "timestamp": "yesterday"},
			wantErr: "invalid vital timestamp at index 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := newMetricIngestGateway(t, "vitals-structural")
			status, payload := setup.postVitals(t, []any{
				vitalPayload("LCP", 1, "/checkout", at),
				tc.vital,
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s", status, payload)
			}
			var rejection struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &rejection); err != nil {
				t.Fatalf("decode response: %v body=%s", err, payload)
			}
			wantErr := strings.Replace(tc.wantErr, "index 0", "index 1", 1)
			if rejection.Error != wantErr {
				t.Fatalf("error=%q want=%q", rejection.Error, wantErr)
			}
			if samples := setup.samples(); samples != "" {
				t.Fatalf("rejected batch reached storage: %q", samples)
			}
		})
	}
}

func TestVitalsIngestIsCaseInsensitive(t *testing.T) {
	setup := newMetricIngestGateway(t, "vitals-case")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postVitals(t, []any{
		vitalPayload("FcP", 1, "/checkout", at),
		vitalPayload(" fcp ", 2, "/checkout", at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	samples := setup.samples()
	if strings.Count(samples, "web_vitals_fcp{") != 2 {
		t.Fatalf("samples=%q", samples)
	}
}

func TestVitalsIngestBatchLimitPageNormalizationAndTimestampBounds(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	t.Run("rejects batch exceeding max size with 400", func(t *testing.T) {
		setup := newMetricIngestGateway(t, "vitals-batch-limit")
		vitals := make([]any, 0, 101)
		for i := range 101 {
			vitals = append(vitals, vitalPayload("LCP", float64(i), "/checkout", at))
		}
		status, payload := setup.postVitals(t, vitals)
		if status != http.StatusBadRequest {
			t.Fatalf("oversized batch status=%d want=400 body=%s", status, payload)
		}
		if setup.importCount() != 0 {
			t.Fatalf("oversized batch reached upstream %d times", setup.importCount())
		}
	})

	t.Run("normalizes page strips query hash control chars dynamic segments and bounds length", func(t *testing.T) {
		setup := newMetricIngestGateway(t, "vitals-page-norm")
		longPath := "/products/" + strings.Repeat("a", 300)
		status, payload := setup.postVitals(t, []any{
			vitalPayload("LCP", 100, "/checkout?session=abc#payment", at),
			vitalPayload("FCP", 110, "https://shop.example.com/orders/987654321?utm=1#top", at),
			vitalPayload("INP", 120, "/users/550e8400-e29b-41d4-a716-446655440000/profile", at),
			vitalPayload("CLS", 0.1, "/items/0123456789abcdef0123456789abcdef/detail", at),
			vitalPayload("TTFB", 80, "/dash\x00\x1fboard//overview/", at),
			vitalPayload("LCP", 200, longPath, at),
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, payload)
		}
		samples := setup.samples()
		for _, want := range []string{
			`web_vitals_lcp{project_id="vitals-page-norm",page="/checkout"} 100`,
			`web_vitals_fcp{project_id="vitals-page-norm",page="/orders/:id"} 110`,
			`web_vitals_inp{project_id="vitals-page-norm",page="/users/:id/profile"} 120`,
			`web_vitals_cls{project_id="vitals-page-norm",page="/items/:id/detail"} 0.1`,
			`web_vitals_ttfb{project_id="vitals-page-norm",page="/dashboard/overview"} 80`,
		} {
			if !strings.Contains(samples, want) {
				t.Fatalf("samples missing %q:\n%s", want, samples)
			}
		}
		if strings.Contains(samples, "session=abc") || strings.Contains(samples, "payment") ||
			strings.Contains(samples, "987654321") || strings.Contains(samples, "550e8400") ||
			strings.Contains(samples, strings.Repeat("a", 260)) {
			t.Fatalf("unnormalized/high-cardinality page leaked into samples:\n%s", samples)
		}
	})

	t.Run("rejects excessive distinct pages in one batch with 400", func(t *testing.T) {
		setup := newMetricIngestGateway(t, "vitals-distinct-pages")
		vitals := make([]any, 0, 40)
		for i := range 40 {
			vitals = append(vitals, vitalPayload("LCP", 10, fmt.Sprintf("/route-%c-%c", 'a'+(i/26), 'a'+(i%26)), at))
		}
		status, payload := setup.postVitals(t, vitals)
		if status != http.StatusBadRequest {
			t.Fatalf("high-cardinality batch status=%d want=400 body=%s", status, payload)
		}
		if setup.importCount() != 0 {
			t.Fatalf("high-cardinality batch reached upstream")
		}
	})

	t.Run("rejects invalid and out-of-range metric timestamps", func(t *testing.T) {
		badTimestamps := []any{
			0,
			-1,
			-1700000000,
			1e30,
			9999999999999999999.0,
			"1969-12-31T23:59:59Z",
			"0001-01-01T00:00:00Z",
			"9999-01-01T00:00:00Z",
		}
		for _, ts := range badTimestamps {
			setup := newMetricIngestGateway(t, "vitals-bad-ts")
			status, payload := setup.postVitals(t, []any{
				map[string]any{"name": "LCP", "value": 100, "page": "/home", "timestamp": ts},
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("timestamp %#v status=%d want=422 body=%s", ts, status, payload)
			}
			if setup.importCount() != 0 {
				t.Fatalf("invalid timestamp %#v reached upstream", ts)
			}
		}
	})
}

func diagnosticPayload(signal, reason string, dropped int, at time.Time) map[string]any {
	return map[string]any{
		"measurement_id": "0123456789abcdef0123456789abcdef",
		"timestamp":      at.Format(time.RFC3339Nano),
		"service":        "checkout-browser",
		"signal":         signal,
		"reason":         reason,
		"dropped":        dropped,
	}
}

func withDiagnosticField(base map[string]any, key string, value any) map[string]any {
	changed := make(map[string]any, len(base))
	for k, v := range base {
		changed[k] = v
	}
	changed[key] = value
	return changed
}

func TestSDKDiagnosticsIngestWritesLowCardinalityCounter(t *testing.T) {
	setup := newMetricIngestGateway(t, "browser-diagnostics")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postDiagnostics(t, []any{
		diagnosticPayload("replay", "retry_exhausted", 3, at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 1 || result.Rejected != 0 {
		t.Fatalf("response=%s want accepted=1 rejected=0", payload)
	}
	want := `lazy_observe_sdk_dropped_telemetry_delta{project_id="browser-diagnostics",` +
		`service_name="checkout-browser",sdk_language="browser",signal="replay",` +
		`reason="retry_exhausted"} 3 1787488496789`
	if samples := setup.samples(); !strings.Contains(samples, want) {
		t.Fatalf("samples=%q want substring=%q", samples, want)
	}
	if logged := setup.logs.String(); logged != "" {
		t.Fatalf("clean batch logged: %q", logged)
	}
}

func TestSDKDiagnosticsIngestSkipsUnknownEnumsAndCountsRejected(t *testing.T) {
	setup := newMetricIngestGateway(t, "browser-diagnostics-partial")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postDiagnostics(t, []any{
		diagnosticPayload("error", "queue_full", 1, at),
		// Values a newer SDK adds are accepted by this server, not rejected.
		diagnosticPayload("vitals", "server_rejected", 2, at),
		diagnosticPayload("profile", "queue_full", 4, at),
		diagnosticPayload("replay", "future_reason", 8, at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 2 || result.Rejected != 2 {
		t.Fatalf("response=%s want accepted=2 rejected=2", payload)
	}

	samples := setup.samples()
	for _, want := range []string{
		`signal="error",reason="queue_full"} 1 `,
		`signal="vitals",reason="server_rejected"} 2 `,
	} {
		if !strings.Contains(samples, want) {
			t.Fatalf("samples=%q want substring=%q", samples, want)
		}
	}
	for _, unwanted := range []string{"profile", "future_reason"} {
		if strings.Contains(samples, unwanted) {
			t.Fatalf("unknown enum value reached storage: %q", samples)
		}
	}
	logged := setup.logs.String()
	if !strings.Contains(logged, "profile") || !strings.Contains(logged, "future_reason") ||
		!strings.Contains(logged, "browser-diagnostics-partial") {
		t.Fatalf("warn log missing project and skipped values: %q", logged)
	}
	if imports := setup.importCount(); imports != 1 {
		t.Fatalf("metrics imports=%d want 1", imports)
	}
}

func TestSDKDiagnosticsIngestAllUnknownEnumsWritesNothing(t *testing.T) {
	setup := newMetricIngestGateway(t, "browser-diagnostics-unknown")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postDiagnostics(t, []any{
		diagnosticPayload("profile", "queue_full", 1, at),
		diagnosticPayload("replay", "future_reason", 2, at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 0 || result.Rejected != 2 {
		t.Fatalf("response=%s want accepted=0 rejected=2", payload)
	}
	if samples := setup.samples(); samples != "" {
		t.Fatalf("fully rejected batch reached storage: %q", samples)
	}
	if imports := setup.importCount(); imports != 0 {
		t.Fatalf("metrics imports=%d want 0: a fully rejected batch must not POST to the upstream", imports)
	}
	logged := setup.logs.String()
	if !strings.Contains(logged, "profile") || !strings.Contains(logged, "future_reason") {
		t.Fatalf("warn log missing skipped values: %q", logged)
	}
}

func TestSDKDiagnosticsIngestEmptyBatchWritesNothing(t *testing.T) {
	setup := newMetricIngestGateway(t, "browser-diagnostics-empty")

	status, payload := setup.postDiagnostics(t, []any{})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 0 || result.Rejected != 0 {
		t.Fatalf("response=%s want accepted=0 rejected=0", payload)
	}
	if imports := setup.importCount(); imports != 0 {
		t.Fatalf("metrics imports=%d want 0: an empty batch must not POST to the upstream", imports)
	}
	if logged := setup.logs.String(); logged != "" {
		t.Fatalf("empty batch logged: %q", logged)
	}
}

func TestSDKDiagnosticsIngestBoundsUnknownEnumLogging(t *testing.T) {
	setup := newMetricIngestGateway(t, "browser-diagnostics-flood")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	// "指" is three bytes, so a 64-byte cut lands mid-rune at 63 bytes and must be
	// backed up to 21 whole runes.
	longSignal := strings.Repeat("指", 2000)
	wantPrefix := strings.Repeat("指", 21) + "…"

	status, payload := setup.postDiagnostics(t, []any{
		diagnosticPayload(longSignal, "queue_full", 1, at),
		diagnosticPayload(longSignal+"a", "queue_full", 1, at),
		diagnosticPayload(longSignal+"b", "queue_full", 1, at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	var result struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response: %v body=%s", err, payload)
	}
	if result.Accepted != 0 || result.Rejected != 3 {
		t.Fatalf("response=%s want accepted=0 rejected=3", payload)
	}
	logged := setup.logs.String()
	if strings.Count(logged, wantPrefix) != 1 {
		t.Fatalf("log=%q want exactly one truncated value %q (de-duplicated after truncation)",
			logged, wantPrefix)
	}
	if strings.Contains(logged, strings.Repeat("指", 30)) {
		t.Fatalf("log kept an untruncated value: %q", logged)
	}
	if len(logged) > 1024 {
		t.Fatalf("log line=%d bytes, want a bounded warning: %q", len(logged), logged)
	}
}

func TestSDKDiagnosticsIngestSummarizesOverflowingUnknownEnums(t *testing.T) {
	setup := newMetricIngestGateway(t, "browser-diagnostics-overflow")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	var measurements []any
	for _, signal := range []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"} {
		measurements = append(measurements, diagnosticPayload(signal, "queue_full", 1, at))
	}

	status, payload := setup.postDiagnostics(t, measurements)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	logged := setup.logs.String()
	// Sorted so the warning is deterministic, capped at five values, remainder counted.
	if !strings.Contains(logged, "aaa,bbb,ccc,ddd,eee,+2 more") {
		t.Fatalf("log=%q want sorted cap with overflow count", logged)
	}
	if strings.Contains(logged, "fff") || strings.Contains(logged, "ggg") {
		t.Fatalf("log=%q kept values beyond the cap", logged)
	}
}

func TestSDKDiagnosticsIngestKeepsStructuralErrorsAsWholeBatchRejection(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	valid := diagnosticPayload("replay", "queue_full", 1, at)
	for _, tc := range []struct {
		name        string
		measurement map[string]any
	}{
		{name: "missing measurement id", measurement: withDiagnosticField(valid, "measurement_id", "")},
		{name: "malformed measurement id", measurement: withDiagnosticField(valid, "measurement_id", "not-stable")},
		{name: "invalid timestamp", measurement: withDiagnosticField(valid, "timestamp", "yesterday")},
		{name: "missing timestamp", measurement: withDiagnosticField(valid, "timestamp", nil)},
		{name: "empty service", measurement: withDiagnosticField(valid, "service", "")},
		{name: "blank service", measurement: withDiagnosticField(valid, "service", "  ")},
		{
			name:        "service over 128 bytes",
			measurement: withDiagnosticField(valid, "service", strings.Repeat("s", 129)),
		},
		{name: "missing signal", measurement: withDiagnosticField(valid, "signal", "")},
		{name: "missing reason", measurement: withDiagnosticField(valid, "reason", "")},
		{name: "zero dropped", measurement: withDiagnosticField(valid, "dropped", 0)},
		{name: "negative dropped", measurement: withDiagnosticField(valid, "dropped", -1)},
		{name: "dropped over cap", measurement: withDiagnosticField(valid, "dropped", 1_000_000_001)},
		{
			// Structural validity is a property of the batch: an unknown enum value is
			// skipped, but a malformed entry still rejects the whole request.
			name: "unknown signal with invalid timestamp",
			measurement: withDiagnosticField(
				withDiagnosticField(valid, "signal", "profile"),
				"timestamp",
				"yesterday",
			),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := newMetricIngestGateway(t, "browser-diagnostics-structural")
			status, payload := setup.postDiagnostics(t, []any{valid, tc.measurement})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s", status, payload)
			}
			var rejection struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &rejection); err != nil {
				t.Fatalf("decode response: %v body=%s", err, payload)
			}
			if want := "invalid SDK diagnostic at index 1"; rejection.Error != want {
				t.Fatalf("error=%q want=%q", rejection.Error, want)
			}
			if samples := setup.samples(); samples != "" {
				t.Fatalf("rejected batch reached storage: %q", samples)
			}
			if imports := setup.importCount(); imports != 0 {
				t.Fatalf("metrics imports=%d want 0 for a rejected batch", imports)
			}
		})
	}
}

func TestErrorIngestLimits(t *testing.T) {
	store := testStore(t)
	p, _ := store.CreateProject(context.Background(), "Default", "default")
	token, _ := store.CreateProjectAPIToken(context.Background(), p.ID)
	upstream := httptest.NewServer(&errorStorage{})
	defer upstream.Close()
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL})
	t.Cleanup(func() { _ = gw.Shutdown() })
	events := make([]any, 101)
	for i := range events {
		events[i] = map[string]any{}
	}
	postErrorsTest(t, gw.App(), token, events, http.StatusUnprocessableEntity)
	large := map[string]any{
		"event_id":  "large",
		"timestamp": time.Now().UTC(),
		"platform":  "go",
		"level":     "error",
		"message":   strings.Repeat("x", maxErrorEventSize),
		"exception": map[string]any{"type": "panic"},
	}
	postErrorsTest(t, gw.App(), token, []any{large}, http.StatusRequestEntityTooLarge)
}

type blockedErrorWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockedErrorWriter) WriteErrorEvent(
	ctx context.Context,
	_ errortrack.Project,
	_ json.RawMessage,
	_ errortrack.Event,
) error {
	select {
	case w.started <- struct{}{}:
	default:
	}
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestErrorInboxCapacityReadinessAndDiagnostics(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Inbox operations", "inbox-operations")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "inbox-admin", "password", "admin")
	member := sessionCookie(t, store, "inbox-member", "password", "member")
	writer := &blockedErrorWriter{started: make(chan struct{}, 1), release: make(chan struct{})}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Capacity: errortrack.InboxCapacity{
			MaxItems: 1, MaxBytes: 1 << 20, ReadinessItems: 1, ReadinessBytes: 1 << 20,
		},
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, ErrorTracker: inbox})
	t.Cleanup(func() { _ = gw.Shutdown() })
	gw.MarkReady()

	event := func(id string) map[string]any {
		return map[string]any{
			"event_id": id, "timestamp": time.Now().UTC(), "platform": "go", "level": "error",
			"exception": map[string]any{
				"type": "CapacityError", "value": "boom",
				"stacktrace": map[string]any{"frames": []map[string]any{{
					"function": "main.capacity", "filename": "main.go", "in_app": true,
				}}},
			},
		}
	}
	postErrorsTest(t, gw.App(), token, []any{event("capacity-first")}, http.StatusOK)
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("first capacity event was not claimed")
	}
	payload, _ := json.Marshal(map[string]any{"events": []any{event("capacity-second")}})
	req := httptest.NewRequest(http.MethodPost, "/api/events/errors", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" ||
		bytes.Contains(body, []byte("capacity-first")) {
		t.Fatalf("capacity status=%d retry=%q body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}

	assertProbe := func(path string, want int) []byte {
		t.Helper()
		response, requestErr := gw.App().Test(httptest.NewRequest(http.MethodGet, path, nil))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		responseBody, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s status=%d want=%d body=%s", path, response.StatusCode, want, responseBody)
		}
		return responseBody
	}
	assertProbe("/livez", http.StatusOK)
	healthBody := assertProbe("/healthz", http.StatusOK)
	if !bytes.Contains(healthBody, []byte(`"error_inbox":"active_items=1 active_bytes=`)) {
		t.Fatalf("health body=%s", healthBody)
	}
	readyBody := assertProbe("/readyz", http.StatusServiceUnavailable)
	if !bytes.Contains(readyBody, []byte(`"error_inbox"`)) {
		t.Fatalf("readiness body=%s", readyBody)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/system/error-inbox", nil)
	req.AddCookie(member)
	resp, err = gw.App().Test(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member diagnostics status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	req = httptest.NewRequest(http.MethodGet, "/api/system/error-inbox", nil)
	req.AddCookie(admin)
	resp, err = gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"active_items":1`)) ||
		!bytes.Contains(body, []byte(`"rejections_total":1`)) || bytes.Contains(body, []byte(`"payload"`)) {
		t.Fatalf("admin diagnostics status=%d body=%s", resp.StatusCode, body)
	}

	close(writer.release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, requestErr := gw.App().Test(httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness did not recover after Inbox drained")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSourceMapUploadSymbolicationFingerprintFallbackAndIsolation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	p1, _ := store.CreateProject(ctx, "Default", "default")
	p2, _ := store.CreateProject(ctx, "P2", "p2")
	token1, _ := store.CreateProjectAPIToken(ctx, p1.ID)
	cookie := sessionCookie(t, store, "source-map-admin", "password", "admin")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	defer upstream.Close()
	gw := newMetadataTestGateway(store,
		Config{Tokens: store, Store: store, Logs: upstream.URL, Blobs: blobs},
	)
	t.Cleanup(func() { _ = gw.Shutdown() })
	app := gw.App()
	mapData, err := os.ReadFile(filepath.Join("..", "symbolicate", "testdata", "app.min.js.map"))
	if err != nil {
		t.Fatal(err)
	}
	upload1 := createUploadTokenTest(t, store, p1.ID)
	upload2 := createUploadTokenTest(t, store, p2.ID)
	uploadSourceMapTest(t, app, upload1, "release-a", "one.min.js", mapData, http.StatusCreated)
	uploadSourceMapTest(t, app, upload1, "release-b", "two.min.js", mapData, http.StatusCreated)
	uploadSourceMapTest(t, app, upload2, "p2-release", "private.min.js", mapData, http.StatusCreated)

	jsEvent := func(id, release, filename, function string) map[string]any {
		return map[string]any{
			"event_id":  id,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"platform":  "javascript",
			"release":   release,
			"level":     "error",
			"exception": map[string]any{
				"type":  "Error",
				"value": "boom",
				"stacktrace": map[string]any{
					"frames": []map[string]any{
						{
							"function": function,
							"filename": "https://cdn.example/" + filename,
							"lineno":   1,
							"colno":    37,
							"in_app":   true,
						},
					},
				},
			},
		}
	}
	postErrorsTest(t, app, token1, []any{jsEvent("mapped-a", "release-a", "one.min.js", "a")}, http.StatusOK)
	postErrorsTest(t, app, token1, []any{jsEvent("mapped-b", "release-b", "two.min.js", "b")}, http.StatusOK)
	group := waitForIssueCountTest(t, app, cookie, "default", 2)
	if group == nil {
		t.Fatal("different releases did not share a symbolicated issue")
	}
	id := int(group["id"].(float64))
	req := httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(id)+"/events/mapped-a", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", "default")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"filename":"lazy-observe-original.js"`)) ||
		!bytes.Contains(body, []byte(`"lineno":3`)) ||
		!bytes.Contains(body, []byte(`"colno":12`)) ||
		!bytes.Contains(body, []byte(`"symbolicated":true`)) {
		t.Fatalf("symbolicated issue event status=%d body=%s", resp.StatusCode, body)
	}

	postErrorsTest(t, app, token1, []any{jsEvent("missing", "no-map", "missing.min.js", "rawMissing")}, http.StatusOK)
	postErrorsTest(
		t,
		app,
		token1,
		[]any{jsEvent("isolated", "p2-release", "private.min.js", "rawPrivate")},
		http.StatusOK,
	)
	issues := waitForIssueLengthTest(t, app, cookie, "default", 3)
	if len(issues) != 3 {
		t.Fatalf("fallback/isolation issue count=%d issues=%#v", len(issues), issues)
	}
	storage.mu.Lock()
	records := append([]map[string]any(nil), storage.logs[strconv.Itoa(p1.ID)]...)
	storage.mu.Unlock()
	if len(records) < 2 || records[0]["raw_stacktrace"] == records[0]["symbolicated_stacktrace"] ||
		records[0]["raw_event"] == nil ||
		records[0]["symbolicated_event"] == nil {
		t.Fatalf("VLogs raw/symbolicated records=%#v", records)
	}
}

func TestAcceptedErrorPinsItsSourceMapRevision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Pinned map", "pinned-map")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "pinned-map-admin", "password", "admin")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storage := &errorStorage{}
	var unavailable atomic.Bool
	unavailable.Store(true)
	attempted := make(chan struct{})
	var attemptedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/insert/jsonline" && unavailable.Load() {
			attemptedOnce.Do(func() { close(attempted) })
			http.Error(w, "paused after symbolication", http.StatusServiceUnavailable)
			return
		}
		storage.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL, Blobs: blobs})
	t.Cleanup(func() { _ = gw.Shutdown() })

	mapData, err := os.ReadFile(filepath.Join("..", "symbolicate", "testdata", "app.min.js.map"))
	if err != nil {
		t.Fatal(err)
	}
	uploadSourceMapTest(
		t, gw.App(), createUploadTokenTest(t, store, project.ID), "pinned-release", "app.min.js", mapData,
		http.StatusCreated,
	)
	line, column := 1, 37
	postErrorsTest(t, gw.App(), token, []any{map[string]any{
		"event_id": "pinned-event", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"platform": "javascript", "release": "pinned-release", "level": "error",
		"exception": map[string]any{
			"type": "Error", "value": "pinned",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "a", "filename": "https://cdn.example/app.min.js", "lineno": line, "colno": column,
				"in_app": true,
			}}},
		},
	}}, http.StatusOK)
	select {
	case <-attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("Inbox worker did not reach the paused VLogs writer")
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/releases/pinned-release/sourcemaps", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete source maps status=%d", resp.StatusCode)
	}
	unavailable.Store(false)

	issue := waitForIssueCountTest(t, gw.App(), cookie, project.Slug, 1)
	if issue == nil {
		t.Fatal("accepted event did not resume")
	}
	req = httptest.NewRequest(
		http.MethodGet,
		"/api/issues/"+strconv.Itoa(int(issue["id"].(float64)))+"/events/pinned-event",
		nil,
	)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err = gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"filename":"lazy-observe-original.js"`)) ||
		!bytes.Contains(body, []byte(`"symbolicated":true`)) {
		t.Fatalf("pinned source map event status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAcceptedErrorDoesNotAdoptALaterSourceMap(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Missing map", "missing-map")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "missing-map-admin", "password", "admin")
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storage := &errorStorage{}
	var unavailable atomic.Bool
	unavailable.Store(true)
	attempted := make(chan struct{})
	var attemptedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/insert/jsonline" && unavailable.Load() {
			attemptedOnce.Do(func() { close(attempted) })
			http.Error(w, "paused without source map", http.StatusServiceUnavailable)
			return
		}
		storage.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL, Blobs: blobs})
	t.Cleanup(func() { _ = gw.Shutdown() })

	postErrorsTest(t, gw.App(), token, []any{map[string]any{
		"event_id": "missing-map-event", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"platform": "javascript", "release": "uploaded-later", "level": "error",
		"exception": map[string]any{
			"type": "Error", "value": "must stay minified",
			"stacktrace": map[string]any{"frames": []map[string]any{{
				"function": "a", "filename": "https://cdn.example/app.min.js", "lineno": 1, "colno": 37,
				"in_app": true,
			}}},
		},
	}}, http.StatusOK)
	select {
	case <-attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("Inbox worker did not reach the paused VLogs writer")
	}

	mapData, err := os.ReadFile(filepath.Join("..", "symbolicate", "testdata", "app.min.js.map"))
	if err != nil {
		t.Fatal(err)
	}
	uploadSourceMapTest(
		t, gw.App(), createUploadTokenTest(t, store, project.ID), "uploaded-later", "app.min.js", mapData,
		http.StatusCreated,
	)
	unavailable.Store(false)

	issue := waitForIssueCountTest(t, gw.App(), cookie, project.Slug, 1)
	if issue == nil {
		t.Fatal("accepted event did not resume")
	}
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/issues/"+strconv.Itoa(int(issue["id"].(float64)))+"/events/missing-map-event",
		nil,
	)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"function":"a"`)) ||
		!bytes.Contains(body, []byte(`"filename":"https://cdn.example/app.min.js"`)) ||
		bytes.Contains(body, []byte(`"symbolicated":true`)) {
		t.Fatalf("later source map changed accepted event status=%d body=%s", resp.StatusCode, body)
	}
}

func TestSourceMapUploadRejectsMoreThanTwoHundredFiles(t *testing.T) {
	store := testStore(t)
	p, _ := store.CreateProject(context.Background(), "Default", "default")
	token := createUploadTokenTest(t, store, p.ID)
	blobs, _ := blob.NewLocal(t.TempDir())
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Blobs: blobs}).App()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i <= maxSourceMapFiles; i++ {
		name := fmt.Sprintf("chunk-%03d.js", i)
		part, err := w.CreateFormFile(name, name+".map")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(`{"version":3,"sources":[],"names":[],"mappings":""}`))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/releases/too-many/sourcemaps", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want=413", resp.StatusCode)
	}
}

func TestPostgresIssueCountAcrossGatewayReplicas(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL replica test")
	}
	ctx := context.Background()
	dbURL := startEmbeddedPostgresE2E(t)
	storeA, err := meta.OpenStore(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := meta.OpenStore(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	project, err := storeA.CreateProject(ctx, "Replica", "replica")
	if err != nil {
		t.Fatal(err)
	}
	token, err := storeA.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(&errorStorage{})
	defer upstream.Close()
	gwA := newMetadataTestGateway(
		storeA,
		Config{Tokens: storeA, Store: storeA, Logs: upstream.URL},
	)
	gwB := newMetadataTestGateway(
		storeB,
		Config{Tokens: storeB, Store: storeB, Logs: upstream.URL},
	)
	defer gwA.Shutdown()
	defer gwB.Shutdown()

	listeners := make([]net.Listener, 2)
	gateways := []*Gateway{gwA, gwB}
	for i := range gateways {
		listeners[i], err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func(g *Gateway, listener net.Listener) { _ = g.app.Listener(listener) }(gateways[i], listeners[i])
	}
	const perReplica = 25
	for replica, listener := range listeners {
		for i := 0; i < perReplica; i++ {
			event := map[string]any{
				"event_id":  fmt.Sprintf("replica-%d-%d", replica, i),
				"timestamp": time.Now().UTC(),
				"platform":  "go",
				"level":     "error",
				"exception": map[string]any{
					"type":  "SharedError",
					"value": "same",
					"stacktrace": map[string]any{
						"frames": []map[string]any{
							{"function": "service.Handle", "filename": "service.go", "lineno": 42, "in_app": true},
						},
					},
				},
			}
			body, _ := json.Marshal(map[string]any{"events": []any{event}})
			req, _ := http.NewRequest(
				http.MethodPost,
				"http://"+listener.Addr().String()+"/api/events/errors",
				bytes.NewReader(body),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			resp, requestErr := http.DefaultClient.Do(req)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("replica %d ingest status=%d", replica, resp.StatusCode)
			}
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, listErr := storeA.ListIssues(ctx, project.ID, meta.IssueListOptions{})
		if listErr == nil && len(rows) == 1 && rows[0].Count == 2*perReplica {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := storeA.ListIssues(ctx, project.ID, meta.IssueListOptions{})
	t.Fatalf("replica issue rows=%#v err=%v", rows, err)
}

func uploadSourceMapTest(t *testing.T, app httpTestApp, token, release, filename string, data []byte, want int) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile(filename, filename+".map")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/releases/"+release+"/sourcemaps", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("upload status=%d want=%d body=%s", resp.StatusCode, want, response)
	}
}

func postErrorsTest(t *testing.T, app httpTestApp, token string, events []any, want int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"events": events})
	req := httptest.NewRequest(http.MethodPost, "/api/events/errors", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("errors status=%d want=%d body=%s", resp.StatusCode, want, response)
	}
}

func listIssuesTest(t *testing.T, app httpTestApp, cookie *http.Cookie, project string) []map[string]any {
	t.Helper()
	return waitForIssuesTest(t, app, cookie, project, func(issues []map[string]any) bool { return len(issues) > 0 })
}

func waitForIssueCountTest(
	t *testing.T,
	app httpTestApp,
	cookie *http.Cookie,
	project string,
	count float64,
) map[string]any {
	t.Helper()
	issues := waitForIssuesTest(t, app, cookie, project, func(issues []map[string]any) bool {
		return issueWithCount(issues, count) != nil
	})
	return issueWithCount(issues, count)
}

func waitForIssueLengthTest(
	t *testing.T,
	app httpTestApp,
	cookie *http.Cookie,
	project string,
	length int,
) []map[string]any {
	t.Helper()
	return waitForIssuesTest(t, app, cookie, project, func(issues []map[string]any) bool {
		return len(issues) == length
	})
}

func waitForIssuesTest(
	t *testing.T,
	app httpTestApp,
	cookie *http.Cookie,
	project string,
	ready func([]map[string]any) bool,
) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var issues []map[string]any
	for {
		req := httptest.NewRequest(http.MethodGet, "/api/issues?sort=last_seen", nil)
		req.AddCookie(cookie)
		req.Header.Set("X-LO-Project", project)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
		}
		var output struct {
			Issues []map[string]any `json:"issues"`
		}
		if json.Unmarshal(body, &output) != nil {
			t.Fatalf("list body=%s", body)
		}
		issues = output.Issues
		if ready(issues) || time.Now().After(deadline) {
			return issues
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func patchIssueTest(t *testing.T, app httpTestApp, cookie *http.Cookie, project string, id int, status string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"status": status})
	req := httptest.NewRequest(http.MethodPatch, "/api/issues/"+strconv.Itoa(id), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LO-Project", project)
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", resp.StatusCode, response)
	}
}

func issueWithCount(rows []map[string]any, count float64) map[string]any {
	for _, row := range rows {
		if row["count"] == count {
			return row
		}
	}
	return nil
}

func TestLargeErrorEventRoundTripsThroughVLogsAndDeduplicates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Large", "large")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, store, "large-admin", "password", "admin")
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	defer upstream.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	gw := newMetadataTestGateway(store, Config{
		Tokens: store, Store: store, Logs: upstream.URL,
		Now: func() time.Time { return now },
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	// 150 KiB valid event with quotes that expand past 400 KiB once stored in
	// VLogs as both raw_event and symbolicated_event escaped strings.
	largeMsg := strings.Repeat(`"quoted\segment"`, 9000)
	event := map[string]any{
		"event_id":  "large-valid-event",
		"timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano),
		"platform":  "go",
		"level":     "error",
		"message":   largeMsg,
		"exception": map[string]any{
			"type":  "LargePayloadError",
			"value": "still valid under 200 KiB",
			"stacktrace": map[string]any{
				"frames": []map[string]any{{"function": "svc.Big", "filename": "big.go", "in_app": true}},
			},
		},
	}
	postErrorsTest(t, gw.App(), token, []any{event}, http.StatusOK)
	issues := waitForIssueLengthTest(t, gw.App(), cookie, project.Slug, 1)
	issueID := int(issues[0]["id"].(float64))

	// Duplicate the same physical record in VLogs (simulating at-least-once
	// retry recovery) and add an older event to test dedup + _time desc sort.
	storage.mu.Lock()
	tenant := strconv.Itoa(project.ID)
	if len(storage.logs[tenant]) == 1 {
		dup := make(map[string]any, len(storage.logs[tenant][0]))
		for k, v := range storage.logs[tenant][0] {
			dup[k] = v
		}
		storage.logs[tenant] = append(storage.logs[tenant], dup)
	}
	storage.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(issueID), nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("large event issue detail status=%d body=%s", resp.StatusCode, body)
	}
	var detail struct {
		Trend  []int64 `json:"trend"`
		Events []struct {
			EventID string `json:"event_id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v body=%s", err, body)
	}
	if len(detail.Events) != 1 || detail.Events[0].EventID != "large-valid-event" {
		t.Fatalf("expected deduplicated single event, got %#v", detail.Events)
	}
	var trendSum int64
	for _, n := range detail.Trend {
		trendSum += n
	}
	if trendSum != 1 {
		t.Fatalf("expected deduplicated trend sum=1, got %d (%v)", trendSum, detail.Trend)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/issues/"+strconv.Itoa(issueID)+"/events/large-valid-event", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-LO-Project", project.Slug)
	resp, err = gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"event_id":"large-valid-event"`)) {
		t.Fatalf("large event detail status=%d len=%d", resp.StatusCode, len(body))
	}
}

func TestSourceMapUploadRejectsInvalidV3StripsSourcesContentAndPreservesPaths(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	p, _ := store.CreateProject(ctx, "Maps", "maps")
	uploadToken := createUploadTokenTest(t, store, p.ID)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Blobs: blobs})
	t.Cleanup(func() { _ = gw.Shutdown() })

	// Malformed v3 maps that trigger go-sourcemap panics must be rejected at upload time.
	for _, bad := range []string{
		`{"version":3,"sections":[{}]}`,
		`{"version":3,"sources":["a.js"],"names":[],"mappings":"ACAA"}`,
		`not-json`,
	} {
		uploadSourceMapTest(t, gw.App(), uploadToken, "bad-rel", "app.js", []byte(bad), http.StatusBadRequest)
	}

	// Upload two files with the same basename under different paths and with sourcesContent.
	mapWithContent := []byte(
		`{"version":3,"sources":["src/a.ts"],"sourcesContent":["secret source"],"names":["fn"],"mappings":"AAAAA"}`,
	)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	p1, _ := w.CreateFormFile("chunks/index.js", "index.js.map")
	_, _ = p1.Write(mapWithContent)
	p2, _ := w.CreateFormFile("pages/index.js", "index.js.map")
	_, _ = p2.Write(mapWithContent)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/releases/paths-v1/sourcemaps", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+uploadToken)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("multi-path upload status=%d body=%s", resp.StatusCode, respBody)
	}
	artifacts, err := store.SourceMapArtifacts(ctx, p.ID, "paths-v1")
	if err != nil || len(artifacts) != 2 ||
		artifacts[0].Filename != "chunks/index.js" || artifacts[1].Filename != "pages/index.js" {
		t.Fatalf("artifacts=%#v err=%v", artifacts, err)
	}
	r, err := blobs.Get(ctx, sourceMapRevisionKey(p.Slug, artifacts[0].Sha256))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(r)
	_ = r.Close()
	if bytes.Contains(stored, []byte("sourcesContent")) || bytes.Contains(stored, []byte("secret source")) {
		t.Fatalf("stored source map retained sourcesContent: %s", stored)
	}
}

func TestPrometheusLabelEscapingAvoidsNonStandardGoEscapes(t *testing.T) {
	// Directly guard formatPromLabelValue against Go-only strconv.Quote escapes (\t, \r, \u....).
	formatted := formatPromLabelValue("/页\t面\"a\\b\nc\r")
	if strings.Contains(formatted, `\t`) || strings.Contains(formatted, `\r`) || strings.Contains(formatted, `\u`) {
		t.Fatalf("formatPromLabelValue produced non-standard Prometheus escape: %q", formatted)
	}
	wantFormatted := `"/页` + "\t" + `面\"a\\b\nc"`
	if formatted != wantFormatted {
		t.Fatalf("formatPromLabelValue=%q want=%q", formatted, wantFormatted)
	}

	// End-to-end via vitalsIngest: control characters are stripped during page normalization,
	// while quotes, backslashes, and UTF-8 survive with standard Prometheus escaping.
	setup := newMetricIngestGateway(t, "prom-escape")
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)

	status, payload := setup.postVitals(t, []any{
		vitalPayload("lcp", 12, "/页\t面\"a\\b\nc\r", at),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, payload)
	}
	samples := setup.samples()
	if strings.Contains(samples, `\t`) || strings.Contains(samples, `\r`) || strings.Contains(samples, `\u`) {
		t.Fatalf("non-standard Prometheus escape written: %q", samples)
	}
	want := `page="/页面\"a\\bc"`
	if !strings.Contains(samples, want) {
		t.Fatalf("samples=%q want substring=%q", samples, want)
	}
}

func TestBatchUpdateIssuesRejectsZeroIDAndListIssuesHandlesOverflowPage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	p, _ := store.CreateProject(ctx, "Batch", "batch")
	cookie := sessionCookie(t, store, "batch-admin", "password", "admin")
	storage := &errorStorage{}
	upstream := httptest.NewServer(storage)
	defer upstream.Close()
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Logs: upstream.URL})
	t.Cleanup(func() { _ = gw.Shutdown() })

	body := []byte(`{"ids":[0,1],"status":"resolved"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/issues/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LO-Project", p.Slug)
	req.AddCookie(cookie)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("batch with zero id status=%d want 400", resp.StatusCode)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/issues?page=9223372036854775807", nil)
	req.Header.Set("X-LO-Project", p.Slug)
	req.AddCookie(cookie)
	resp, err = gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(respBody, []byte(`"issues":[]`)) {
		t.Fatalf("overflow page status=%d body=%s", resp.StatusCode, respBody)
	}
}

func TestListIssuesDegradesGracefullyWhenVLogsTrendsTimeoutOrFail(t *testing.T) {
	origTimeout := issueTrendTimeout
	issueTrendTimeout = 60 * time.Millisecond
	t.Cleanup(func() { issueTrendTimeout = origTimeout })

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "vlogs 503 error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/select/logsql/stats_query_range" {
					http.Error(w, "vlogs down", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			},
		},
		{
			name: "vlogs timeout",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/select/logsql/stats_query_range" {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(2 * time.Second):
						w.WriteHeader(http.StatusOK)
						return
					}
				}
				w.WriteHeader(http.StatusNoContent)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			p, err := store.CreateProject(ctx, "Degrade", "degrade")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{{
				ProjectID:   p.ID,
				Fingerprint: "fp-degrade",
				Title:       "DegradeErr: vlogs flaky",
				Level:       "error",
				Count:       5,
				FirstSeen:   now,
				LastSeen:    now,
				LastEventID: "ev-degrade",
				AcceptedAt:  now,
				AppliedAt:   now,
			}}); err != nil {
				t.Fatal(err)
			}
			cookie := sessionCookie(t, store, "degrade-admin", "password", "admin")
			upstream := httptest.NewServer(tc.handler)
			defer upstream.Close()
			var logsBuf bytes.Buffer
			gw := newMetadataTestGateway(store, Config{
				Tokens: store,
				Store:  store,
				Logs:   upstream.URL,
				Logger: slog.New(slog.NewTextHandler(&logsBuf, nil)),
			})
			t.Cleanup(func() { _ = gw.Shutdown() })

			start := time.Now()
			req := httptest.NewRequest(http.MethodGet, "/api/issues?sort=last_seen", nil)
			req.Header.Set("X-LO-Project", p.Slug)
			req.AddCookie(cookie)
			resp, err := gw.App().Test(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			elapsed := time.Since(start)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d want=200 body=%s", resp.StatusCode, body)
			}
			if elapsed > time.Second {
				t.Fatalf("listIssues took %s; expected independent short trend timeout", elapsed)
			}
			var decoded struct {
				Issues []struct {
					Title string  `json:"title"`
					Count int64   `json:"count"`
					Trend []int64 `json:"trend"`
				} `json:"issues"`
				Total int `json:"total"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode response: %v body=%s", err, body)
			}
			if decoded.Total != 1 || len(decoded.Issues) != 1 ||
				decoded.Issues[0].Title != "DegradeErr: vlogs flaky" ||
				decoded.Issues[0].Count != 5 ||
				len(decoded.Issues[0].Trend) != issueTrendBuckets {
				t.Fatalf("degraded list response=%#v", decoded)
			}
			if !strings.Contains(logsBuf.String(), "degrade issue list trends") {
				t.Fatalf("expected structured warning log on trend degradation, got %q", logsBuf.String())
			}
		})
	}
}
