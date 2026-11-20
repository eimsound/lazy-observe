package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
)

func startTCPTestGateway(t *testing.T, gw *Gateway) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = gw.Drain(ctx)
		<-serveDone
	})
	return ln.Addr().String()
}

func TestTCPPercentEncodedTraceServiceQueryAndValidation(t *testing.T) {
	captured := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":["GET /checkout"]}`)
	}))
	defer upstream.Close()

	gw := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Traces: upstream.URL,
	})
	addr := startTCPTestGateway(t, gw)

	for _, tc := range []struct {
		rawPath         string
		wantEscapedPath string
	}{
		{
			rawPath:         "/api/traces/services/checkout%20api/operations",
			wantEscapedPath: "/select/jaeger/api/services/checkout%20api/operations",
		},
		{
			rawPath:         "/api/traces/services/team%2Fcheckout/operations",
			wantEscapedPath: "/select/jaeger/api/services/team%2Fcheckout/operations",
		},
		{
			rawPath:         "/api/traces/services/billing%3Av2/operations",
			wantEscapedPath: "/select/jaeger/api/services/billing:v2/operations",
		},
	} {
		status, body := rawTCPRequest(t, addr, http.MethodGet, tc.rawPath, "Bearer lou_valid")
		if status != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.rawPath, status, body)
		}
		if got := <-captured; got != tc.wantEscapedPath {
			t.Fatalf("%s upstream=%q want=%q", tc.rawPath, got, tc.wantEscapedPath)
		}
	}

	for _, badPath := range []string{
		"/api/traces/services/..%2Fsecret/operations",
		"/api/traces/services/%2E%2E/operations",
		"/api/traces/services/svc%00null/operations",
		"/api/traces/services/bad%ZZescape/operations",
		"/api/traces/not-hex-id",
		"/api/traces/bad%ZZ",
	} {
		status, _ := rawTCPRequest(t, addr, http.MethodGet, badPath, "Bearer lou_valid")
		if status != http.StatusBadRequest {
			t.Fatalf("bad path %s status=%d want=400", badPath, status)
		}
	}
	if len(captured) != 0 {
		t.Fatalf("invalid trace path reached upstream %d times", len(captured))
	}
}

func TestTCPProxySanitizesUpstream502And504And499Errors(t *testing.T) {
	t.Run("502_unreachable_over_tcp", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		gw := newTestGateway(Config{
			Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Logs:   deadURL,
		})
		addr := startTCPTestGateway(t, gw)

		status, body := rawTCPRequest(t, addr, http.MethodGet, "/api/v1/logs/query?query=*", "Bearer lou_valid")
		if status != http.StatusBadGateway {
			t.Fatalf("status=%d want=502 body=%s", status, body)
		}
		assertSanitizedProxyError(t, body, "upstream_unavailable")
	})

	t.Run("504_timeout_over_tcp", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer slow.Close()

		gw := newTestGateway(Config{
			Tokens:       &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Logs:         slow.URL,
			ProxyTimeout: 80 * time.Millisecond,
		})
		addr := startTCPTestGateway(t, gw)

		status, body := rawTCPRequest(t, addr, http.MethodGet, "/api/v1/logs/query?query=*", "Bearer lou_valid")
		if status != http.StatusGatewayTimeout {
			t.Fatalf("status=%d want=504 body=%s", status, body)
		}
		assertSanitizedProxyError(t, body, "upstream_timeout")
	})

	t.Run("499_client_canceled_sanitized", func(t *testing.T) {
		gw := newTestGateway(Config{
			Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Logs:   "http://10.99.0.5:9428",
			Client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return nil, &url.Error{
						Op:  "Get",
						URL: req.URL.String(),
						Err: context.Canceled,
					}
				}),
			},
		})
		addr := startTCPTestGateway(t, gw)

		status, body := rawTCPRequest(t, addr, http.MethodGet, "/api/v1/logs/query?query=*", "Bearer lou_valid")
		if status != statusClientClosedRequest {
			t.Fatalf("status=%d want=499 body=%s", status, body)
		}
		assertSanitizedProxyError(t, body, "client_closed_request")
		if strings.Contains(body, "10.99.0.5") {
			t.Fatalf("499 leaked upstream host: %s", body)
		}
	})
}

func TestSourceMapPathParamDecodesOnceAndRejectsDotSegments(t *testing.T) {
	store := testStore(t)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Blobs: blobs})
	t.Cleanup(func() { _ = gw.Shutdown() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Web", "web")
	if err != nil {
		t.Fatal(err)
	}
	uploadToken := createUploadTokenTest(t, store, project.ID)
	adminCookie := sessionCookie(t, store, "sm-path-admin", "password", "admin")

	// Percent-encoded valid release name is single-decoded before storage.
	status, body := uploadStatus(t, gw, uploadToken, "v1.2.3%2Drc1")
	if status != http.StatusCreated {
		t.Fatalf("encoded valid release upload status=%d body=%s", status, body)
	}
	artifacts, err := store.SourceMapArtifacts(ctx, project.ID, "v1.2.3-rc1")
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts=%v err=%v", artifacts, err)
	}

	// Dot-segments and unsafe percent-encoded values must be rejected with 400.
	// Note: bare "%2E"/"%2E%2E" path segments are normalized by fasthttp before
	// routing and never reach the release handler (tested directly on
	// unescapedPathParam below); encoded traversal/control/allowlist violations
	// reach the handler and must return 400.
	for _, badRelease := range []string{
		"..%2Fescape",
		"v1%2F..%2Fv2",
		"v1%2F.%2Fv2",
		"rel%00null",
		"rel%5Cslash",
		"rel%20space",
	} {
		st, respBody := uploadStatus(t, gw, uploadToken, badRelease)
		if st != http.StatusBadRequest {
			t.Fatalf("upload bad release %q status=%d want=400 body=%s", badRelease, st, respBody)
		}
		delReq := httptest.NewRequest(http.MethodDelete, "/api/releases/"+badRelease+"/sourcemaps", nil)
		delReq.AddCookie(adminCookie)
		delReq.Header.Set("X-LO-Project", project.Slug)
		delResp, delErr := gw.App().Test(delReq)
		if delErr != nil {
			t.Fatal(delErr)
		}
		_ = delResp.Body.Close()
		if delResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("delete bad release %q status=%d want=400", badRelease, delResp.StatusCode)
		}
	}

	// Malformed percent escape over real TCP must also return 400.
	addr := startTCPTestGateway(t, gw)
	var mpBody bytes.Buffer
	mw := multipart.NewWriter(&mpBody)
	part, err := mw.CreateFormFile("app.min.js", "app.min.js.map")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(testSourceMap))
	_ = mw.Close()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw := "POST /api/releases/bad%ZZ/sourcemaps HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer " + uploadToken + "\r\n" +
		"Content-Type: " + mw.FormDataContentType() + "\r\n" +
		"Content-Length: " + strconvItoa(mpBody.Len()) + "\r\n" +
		"Connection: close\r\n\r\n" +
		mpBody.String()
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed release over TCP status=%d want=400", resp.StatusCode)
	}
}

func TestSplitLogsAndTracesQueryAndIngestRouting(t *testing.T) {
	var logsQueryHits, logsIngestHits, tracesQueryHits, tracesIngestHits atomic.Int32
	record := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("AccountID") != "7" || r.Header.Get("ProjectID") != "42" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
	}
	metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer metricsSrv.Close()
	logsQuery := record(&logsQueryHits)
	defer logsQuery.Close()
	logsIngest := record(&logsIngestHits)
	defer logsIngest.Close()
	tracesQuery := record(&tracesQueryHits)
	defer tracesQuery.Close()
	tracesIngest := record(&tracesIngestHits)
	defer tracesIngest.Close()

	backend, err := storage.NewWithOptions(storage.BackendOptions{
		MetricsKind:      storage.MetricsVM,
		MetricsQueryURL:  metricsSrv.URL,
		MetricsIngestURL: metricsSrv.URL,
		Metrics:          storage.MetricsOptions{TenantAccountID: "7"},
		Logs: storage.SignalEndpoint{
			QueryURL:  logsQuery.URL,
			IngestURL: logsIngest.URL,
		},
		Traces: storage.SignalEndpoint{
			QueryURL:  tracesQuery.URL,
			IngestURL: tracesIngest.URL,
		},
	}, metricsSrv.Client())
	if err != nil {
		t.Fatal(err)
	}

	gw := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Storage: backend,
	})
	app := gw.App()

	doReq := func(method, target, token string) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, testErr := app.Test(req)
		if testErr != nil {
			t.Fatalf("%s %s: %v", method, target, testErr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s status=%d want=200", method, target, resp.StatusCode)
		}
	}

	doReq(http.MethodPost, "/v1/logs", "lo_project_token")
	doReq(http.MethodGet, "/api/v1/logs/query?query=*", "lou_valid")
	doReq(http.MethodPost, "/v1/traces", "lo_project_token")
	doReq(http.MethodGet, "/api/traces/services", "lou_valid")
	doReq(http.MethodGet, "/api/traces/search?service=api", "lou_valid")
	doReq(http.MethodGet, "/api/traces/0123456789abcdef", "lou_valid")

	// VictoriaLogsWriter must also target LogsIngestBase when split.
	writer := errortrack.NewVictoriaLogsWriter(backend, logsIngest.Client())
	if err := writer.WriteErrorEvent(
		context.Background(),
		errortrack.Project{ID: 42, Slug: "alpha"},
		json.RawMessage(`{"event_id":"e1"}`),
		errortrack.Event{EventID: "e1", Timestamp: time.Now().UTC(), Level: "error"},
	); err != nil {
		t.Fatalf("WriteErrorEvent: %v", err)
	}

	if logsIngestHits.Load() != 2 {
		t.Fatalf("logsIngestHits=%d want=2 (/v1/logs + error writer)", logsIngestHits.Load())
	}
	if logsQueryHits.Load() != 1 {
		t.Fatalf("logsQueryHits=%d want=1", logsQueryHits.Load())
	}
	if tracesIngestHits.Load() != 1 {
		t.Fatalf("tracesIngestHits=%d want=1", tracesIngestHits.Load())
	}
	if tracesQueryHits.Load() != 3 {
		t.Fatalf("tracesQueryHits=%d want=3", tracesQueryHits.Load())
	}
}

func TestClassifyProxyErrorMapping(t *testing.T) {
	canceledParent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	status, code, _ := classifyProxyError(canceledParent, canceledParent, context.Canceled)
	if status != statusClientClosedRequest || code != "client_closed_request" {
		t.Fatalf("client cancel => (%d, %q)", status, code)
	}

	timedOut, cancelTimeout := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelTimeout()
	status, code, _ = classifyProxyError(context.Background(), timedOut, context.DeadlineExceeded)
	if status != http.StatusGatewayTimeout || code != "upstream_timeout" {
		t.Fatalf("timeout => (%d, %q)", status, code)
	}

	status, code, _ = classifyProxyError(
		context.Background(),
		context.Background(),
		errors.New("dial tcp 10.0.0.1:9428: refused"),
	)
	if status != http.StatusBadGateway || code != "upstream_unavailable" {
		t.Fatalf("transport error => (%d, %q)", status, code)
	}
}

func TestUnescapedPathParamValidation(t *testing.T) {
	app := fiber.New()
	app.Get("/items/:val", func(c fiber.Ctx) error {
		v, err := unescapedPathParam(c, "val", validateSourceMapRelease)
		if err != nil {
			return err
		}
		return c.SendString(v)
	})

	// Direct validator checks cover dot-segments even if an HTTP router normalizes
	// bare "." / ".." segments before handler dispatch.
	for _, bad := range []string{"", " ", ".", "..", "a/../b", "a/./b", "bad\x00", "bad\n", `bad\slash`} {
		if isSafePathParam(bad) {
			t.Fatalf("isSafePathParam(%q) = true, want false", bad)
		}
		if validateSourceMapRelease(bad) && isSafePathParam(bad) {
			t.Fatalf("validateSourceMapRelease(%q) accepted unsafe input", bad)
		}
	}
	if validateSourceMapRelease(".") || validateSourceMapRelease("..") {
		t.Fatal("validateSourceMapRelease must reject dot-segments")
	}

	req := httptest.NewRequest(http.MethodGet, "/items/v1.0.0%2Drc1", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "v1.0.0-rc1" {
		t.Fatalf("valid param status=%d body=%q", resp.StatusCode, body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rawTCPRequest(t *testing.T, addr, method, rawPath, auth string) (int, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rawReq := method + " " + rawPath + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: " + auth + "\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := io.WriteString(conn, rawReq); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func assertSanitizedProxyError(t *testing.T, body, wantCode string) {
	t.Helper()
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode proxy error JSON %q: %v", body, err)
	}
	if payload.Code != wantCode {
		t.Fatalf("code=%q want=%q body=%s", payload.Code, wantCode, body)
	}
	for _, forbidden := range []string{"127.0.0.1", "dial tcp", "connection refused", "context deadline", "http://"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("proxy error leaked %q in body: %s", forbidden, body)
		}
	}
}
