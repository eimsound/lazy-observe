package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

func TestTraceSearchForwardsReproducibleQuery(t *testing.T) {
	captured := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	backend, err := storage.New(
		storage.MetricsPrometheus,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Storage: backend,
		Client:  upstream.Client(),
	}).App()

	query := url.Values{
		"service":     {"checkout api"},
		"operation":   {"GET /orders/:id"},
		"start":       {"1787472000000000"},
		"end":         {"1787475600000000"},
		"minDuration": {"12345us"},
		"maxDuration": {"500ms"},
		"lookback":    {"1h"},
		"limit":       {"20"},
		"tags":        {"error=true"},
		"untrusted":   {"drop-me"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/traces/search?"+query.Encode(), nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	got := <-captured
	if got.URL.Path != "/select/jaeger/api/traces" {
		t.Fatalf("path=%q", got.URL.Path)
	}
	want := query
	want.Del("untrusted")
	if got.URL.Query().Encode() != want.Encode() {
		t.Fatalf("query=%q want=%q", got.URL.RawQuery, want.Encode())
	}
	if got.Header.Get("AccountID") != "0" || got.Header.Get("ProjectID") != "42" {
		t.Fatalf(
			"tenant headers AccountID=%q ProjectID=%q",
			got.Header.Get("AccountID"),
			got.Header.Get("ProjectID"),
		)
	}
}

func TestTraceOperationsSingleEncodesServiceNameAndRejectsInvalidInput(t *testing.T) {
	type capturedReq struct {
		escapedPath string
		rawPath     string
		path        string
	}
	captured := make(chan capturedReq, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedReq{
			escapedPath: r.URL.EscapedPath(),
			rawPath:     r.URL.RawPath,
			path:        r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":["op1"]}`)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Traces: upstream.URL,
	}).App()

	validCases := []struct {
		name            string
		rawServiceParam string
		wantEscapedPath string
	}{
		{
			name:            "service with space",
			rawServiceParam: "checkout%20api",
			wantEscapedPath: "/select/jaeger/api/services/checkout%20api/operations",
		},
		{
			name:            "service with colon",
			rawServiceParam: "billing%3Av2",
			wantEscapedPath: "/select/jaeger/api/services/billing:v2/operations",
		},
		{
			name:            "service with slash",
			rawServiceParam: "team%2Fcheckout",
			wantEscapedPath: "/select/jaeger/api/services/team%2Fcheckout/operations",
		},
	}
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodGet,
				"/api/traces/services/"+tc.rawServiceParam+"/operations",
				nil,
			)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d want=200", resp.StatusCode)
			}
			got := <-captured
			if got.escapedPath != tc.wantEscapedPath {
				t.Fatalf(
					"upstream EscapedPath=%q (path=%q rawPath=%q) want=%q",
					got.escapedPath,
					got.path,
					got.rawPath,
					tc.wantEscapedPath,
				)
			}
		})
	}

	invalidParams := []string{
		"..%2Fsecret",
		"%2E%2E",
		"svc%00null",
		"svc%0Anewline",
		"svc%5Cbackslash",
	}
	for _, rawParam := range invalidParams {
		t.Run("invalid_"+rawParam, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodGet,
				"/api/traces/services/"+rawParam+"/operations",
				nil,
			)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("rawParam=%q status=%d want=400", rawParam, resp.StatusCode)
			}
		})
	}
	t.Run("invalid_malformed_percent_escape_over_tcp", func(t *testing.T) {
		gw := newTestGateway(Config{
			Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Traces: upstream.URL,
		})
		if status := rawTCPStatus(
			t,
			gw,
			"/api/traces/services/bad%ZZescape/operations",
		); status != http.StatusBadRequest {
			t.Fatalf("status=%d want=400", status)
		}
	})
	if len(captured) != 0 {
		t.Fatalf("invalid service names reached upstream %d times", len(captured))
	}
}

func TestTraceByIDValidatesHexAllowlistAndSingleEncodes(t *testing.T) {
	captured := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Traces: upstream.URL,
	}).App()

	validID := "0123456789abcdef0123456789ABCDEF"
	req := httptest.NewRequest(http.MethodGet, "/api/traces/"+validID, nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid traceID status=%d want=200", resp.StatusCode)
	}
	if got := <-captured; got != "/select/jaeger/api/traces/"+validID {
		t.Fatalf("upstream path=%q want=/select/jaeger/api/traces/%s", got, validID)
	}

	for _, bad := range []string{
		"not-hex-id",
		"0123456789abcdef0123456789abcdef0",
		"dead%2Fbeef",
		"..%2Fservices",
		"dead%20beef",
	} {
		t.Run("invalid_"+bad, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/traces/"+bad, nil)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("bad traceID %q status=%d want=400", bad, resp.StatusCode)
			}
		})
	}
	t.Run("invalid_malformed_percent_escape_over_tcp", func(t *testing.T) {
		gw := newTestGateway(Config{
			Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
			Traces: upstream.URL,
		})
		if status := rawTCPStatus(t, gw, "/api/traces/bad%ZZ"); status != http.StatusBadRequest {
			t.Fatalf("status=%d want=400", status)
		}
	})
	if len(captured) != 0 {
		t.Fatalf("invalid traceIDs reached upstream %d times", len(captured))
	}
}

func rawTCPStatus(t *testing.T, gw *Gateway, rawPath string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = gw.Drain(ctx)
		<-serveDone
	}()

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rawReq := "GET " + rawPath + " HTTP/1.1\r\n" +
		"Host: " + ln.Addr().String() + "\r\n" +
		"Authorization: Bearer lou_valid\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := io.WriteString(conn, rawReq); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
