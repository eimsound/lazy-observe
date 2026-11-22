package gateway

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

func zstdOfZeros(t *testing.T, n int) []byte {
	t.Helper()
	zeros := make([]byte, n)
	var out bytes.Buffer
	if _, err := fasthttp.WriteZstdLevel(&out, zeros, fasthttp.CompressZstdBestCompression); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := fasthttp.WriteZstdLevel(&out, data, fasthttp.CompressZstdDefault); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// gzipOfZeros returns a small gzip stream whose decoded size is n bytes.
func gzipOfZeros(t *testing.T, n int) []byte {
	t.Helper()
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for remaining := n; remaining > 0; {
		size := min(remaining, len(chunk))
		if _, err := zw.Write(chunk[:size]); err != nil {
			t.Fatal(err)
		}
		remaining -= size
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestBoundedRequestDecodingRejectsDecompressionBombs(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics: upstream.URL, Logs: upstream.URL, Traces: upstream.URL,
	}).App()
	bomb := gzipOfZeros(t, maxDecodedRequestBody+1)
	if len(bomb) > 1<<20 {
		t.Fatalf("fixture is not a compression bomb: %d compressed bytes", len(bomb))
	}
	for _, test := range []struct {
		name, path, contentType string
		bearer                  bool
	}{
		{"otlp logs proxy", "/v1/logs", "application/x-protobuf", true},
		{"browser errors", "/api/events/errors", "application/json", true},
		{"unauthenticated login", "/api/auth/login", "application/json", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(bomb))
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("Content-Encoding", "gzip")
			if test.bearer {
				req.Header.Set("Authorization", "Bearer lou_valid")
			}
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status=%d want=413 body=%s", resp.StatusCode, readBody(resp))
			}
		})
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("decompression bomb reached storage upstream %d times", calls)
	}
}

func TestBoundedRequestDecodingRejectsZstdDecompressionBombs(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics: upstream.URL, Logs: upstream.URL, Traces: upstream.URL,
	}).App()
	bomb := zstdOfZeros(t, maxDecodedRequestBody+1)
	if len(bomb) > 1<<20 {
		t.Fatalf("fixture is not a zstd compression bomb: %d compressed bytes", len(bomb))
	}
	for _, test := range []struct {
		name, path, contentType string
		bearer                  bool
	}{
		{"otlp logs proxy", "/v1/logs", "application/x-protobuf", true},
		{"otlp traces proxy", "/v1/traces", "application/x-protobuf", true},
		{"browser errors", "/api/events/errors", "application/json", true},
		{"unauthenticated login", "/api/auth/login", "application/json", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(bomb))
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("Content-Encoding", "zstd")
			if test.bearer {
				req.Header.Set("Authorization", "Bearer lou_valid")
			}
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status=%d want=413 body=%s", resp.StatusCode, readBody(resp))
			}
		})
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("zstd decompression bomb reached storage upstream %d times", calls)
	}
}

func TestBoundedRequestDecodingRejectsCorruptAndUnsupportedEncodings(t *testing.T) {
	app := newTestGateway(Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}}).App()
	for _, test := range []struct {
		name, path string
		encodings  []string
		body       []byte
		want       int
	}{
		{"corrupt gzip", "/api/events/errors", []string{"gzip"}, []byte("not gzip"), http.StatusBadRequest},
		{"corrupt zstd", "/api/events/errors", []string{"zstd"}, []byte("not zstd"), http.StatusBadRequest},
		{"unknown encoding stacked on gzip", "/api/events/errors", []string{"snappy, gzip"}, gzipBytes(t, []byte("{}")), http.StatusUnsupportedMediaType},
		{"snappy rejected on logs ingest", "/v1/logs", []string{"snappy"}, []byte("payload"), http.StatusUnsupportedMediaType},
		{"unknown encoding rejected on logs ingest", "/v1/logs", []string{"lz4"}, []byte("payload"), http.StatusUnsupportedMediaType},
		{"multi-header unknown encoding rejected", "/v1/logs", []string{"gzip", "lz4"}, gzipBytes(t, []byte("{}")), http.StatusUnsupportedMediaType},
		{"too many stacked layers rejected", "/v1/logs", []string{"gzip, gzip, gzip, gzip, gzip"}, gzipBytes(t, []byte("{}")), http.StatusUnsupportedMediaType},
		{"multi-header stacked layers exceeding limit rejected", "/v1/logs", []string{"gzip", "gzip", "gzip", "gzip", "gzip"}, gzipBytes(t, []byte("{}")), http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			for _, enc := range test.encodings {
				req.Header.Add("Content-Encoding", enc)
			}
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != test.want {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, test.want, readBody(resp))
			}
		})
	}
}

// OTel SDKs send gzip-compressed OTLP by default. The gateway decodes the body
// once (bounded) and must forward a body that matches the forwarded headers.
func TestTelemetryProxyForwardsDecodedBodyWithoutContentEncoding(t *testing.T) {
	type captured struct {
		encoding string
		body     []byte
	}
	got := make(chan captured, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{encoding: r.Header.Get("Content-Encoding"), body: body}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics: upstream.URL, Logs: upstream.URL, Traces: upstream.URL,
	}).App()
	plain := []byte("otlp-protobuf-payload")
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(gzipBytes(t, plain)))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%v err=%v", resp.StatusCode, err)
	}
	forwarded := <-got
	if forwarded.encoding != "" || !bytes.Equal(forwarded.body, plain) {
		t.Fatalf(
			"upstream got Content-Encoding=%q body=%q; want identity %q",
			forwarded.encoding,
			forwarded.body,
			plain,
		)
	}
}

func TestProfileIngestBoundsPprofDecompression(t *testing.T) {
	app, token, _, _ := profileTestApp(t)
	bomb := gzipOfZeros(t, maxDecodedRequestBody+1)
	resp := uploadProfileFixture(t, app, token, "svc", "cpu", bomb, http.StatusRequestEntityTooLarge)
	resp.Body.Close()
}

// Zstd is decoded bounded in middleware and stripped before reaching storage.
// Snappy is exempted only on /api/v1/write (Prometheus remote-write), which
// applies its own bounded snappy decode.
func TestProxyDecodesZstdBoundedAndRestrictsSnappyToRemoteWrite(t *testing.T) {
	type seen struct {
		body     []byte
		encoding string
	}
	received := make(chan seen, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- seen{body: body, encoding: r.Header.Get("Content-Encoding")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics: upstream.URL, Logs: upstream.URL, Traces: upstream.URL,
	}).App()

	plain := []byte("otlp-zstd-protobuf-payload")
	for _, path := range []string{"/v1/logs", "/v1/traces"} {
		t.Run("zstd_"+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(zstdBytes(t, plain)))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("Content-Encoding", "zstd")
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status=%d want=204 body=%s", resp.StatusCode, readBody(resp))
			}
			got := <-received
			if got.encoding != "" || !bytes.Equal(got.body, plain) {
				t.Fatalf(
					"upstream saw encoding=%q body=%q, want identity %q",
					got.encoding,
					got.body,
					plain,
				)
			}
		})
	}

	t.Run("multi_header_zstd_then_gzip", func(t *testing.T) {
		doubleCompressed := gzipBytes(t, zstdBytes(t, plain))
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(doubleCompressed))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Add("Content-Encoding", "zstd")
		req.Header.Add("Content-Encoding", "gzip")
		req.Header.Set("Authorization", "Bearer lou_valid")
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status=%d want=204 body=%s", resp.StatusCode, readBody(resp))
		}
		got := <-received
		if got.encoding != "" || !bytes.Equal(got.body, plain) {
			t.Fatalf("upstream saw encoding=%q body=%q want identity %q", got.encoding, got.body, plain)
		}
	})

	t.Run("snappy_allowed_on_remote_write", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/write",
			bytes.NewReader([]byte(remoteWriteBody(t, "evil"))),
		)
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "snappy")
		req.Header.Set("Authorization", "Bearer lou_valid")
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status=%d want=204 body=%s", resp.StatusCode, readBody(resp))
		}
		got := <-received
		if got.encoding != "snappy" {
			t.Fatalf("remote-write encoding=%q want=snappy", got.encoding)
		}
	})
}

func TestIsLoopbackUpstreamURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want bool
	}{
		{"http://127.0.0.1:18428", true},
		{"http://127.1.2.3:9428/insert/0", true},
		{"http://[::1]:10428", true},
		{"http://[::ffff:127.0.0.1]:9428", true},
		{"http://localhost:19428", true},
		{"http://vm.localhost:18428", true},
		{"", true},
		{"://bad-url", true},
		{"http://10.0.0.5:8480", false},
		{"http://vminsert:8480/insert/0", false},
		{"https://victoria-logs.monitoring.svc.cluster.local:9428", false},
	}
	for _, tc := range cases {
		if got := isLoopbackUpstreamURL(tc.raw); got != tc.want {
			t.Errorf("isLoopbackUpstreamURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestOTLPGzipPassthroughAutoRemoteVictoriaAndExclusions(t *testing.T) {
	type captured struct {
		host     string
		path     string
		query    string
		encoding string
		account  string
		project  string
		body     []byte
	}
	var mu sync.Mutex
	var seen []captured

	remoteClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				return nil, err
			}
			mu.Lock()
			seen = append(seen, captured{
				host:     r.URL.Host,
				path:     r.URL.Path,
				query:    r.URL.RawQuery,
				encoding: r.Header.Get("Content-Encoding"),
				account:  r.Header.Get("AccountID"),
				project:  r.Header.Get("ProjectID"),
				body:     raw,
			})
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       http.NoBody,
				Request:    r,
			}, nil
		}),
	}

	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics: "http://vminsert.internal:8480",
		Logs:    "http://vlogs.internal:9428",
		Traces:  "http://vtraces.internal:10428",
		Client:  remoteClient,
	}).App()

	plain := []byte("otlp-remote-payload")
	gz := gzipBytes(t, plain)

	for _, tc := range []struct {
		path     string
		enc      string
		wantHost string
		wantPath string
	}{
		{"/v1/metrics", "gzip", "vminsert.internal:8480", "/opentelemetry/v1/metrics"},
		{"/v1/logs", "x-gzip", "vlogs.internal:9428", "/insert/opentelemetry/v1/logs"},
		{"/v1/traces", "identity, GZIP", "vtraces.internal:10428", "/insert/opentelemetry/v1/traces"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(gz))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", tc.enc)
		req.Header.Set("Authorization", "Bearer lo_valid")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s (%s): status=%d want=204 body=%s", tc.path, tc.enc, resp.StatusCode, readBody(resp))
		}
	}

	mu.Lock()
	if len(seen) != 3 {
		mu.Unlock()
		t.Fatalf("captured=%d want=3", len(seen))
	}
	if seen[0].host != "vminsert.internal:8480" || seen[0].path != "/opentelemetry/v1/metrics" ||
		seen[0].query != "extra_label=project_id%3Dalpha" ||
		seen[0].encoding != "gzip" || !bytes.Equal(seen[0].body, gz) {
		mu.Unlock()
		t.Fatalf("metrics passthrough unexpected: %+v", seen[0])
	}
	if seen[1].host != "vlogs.internal:9428" || seen[1].path != "/insert/opentelemetry/v1/logs" ||
		seen[1].encoding != "gzip" || seen[1].account != "0" || seen[1].project != "7" ||
		!bytes.Equal(seen[1].body, gz) {
		mu.Unlock()
		t.Fatalf("logs passthrough unexpected: %+v", seen[1])
	}
	if seen[2].host != "vtraces.internal:10428" || seen[2].path != "/insert/opentelemetry/v1/traces" ||
		seen[2].encoding != "gzip" || seen[2].account != "0" || seen[2].project != "7" ||
		!bytes.Equal(seen[2].body, gz) {
		mu.Unlock()
		t.Fatalf("traces passthrough unexpected: %+v", seen[2])
	}
	// Explicit disable forces plaintext decode even for remote upstreams.
	seen = nil
	mu.Unlock()

	noPassApp := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics:             "http://vminsert.internal:8480",
		Logs:                "http://vlogs.internal:9428",
		Traces:              "http://vtraces.internal:10428",
		Client:              remoteClient,
		OTLPGzipPassthrough: boolPtr(false),
	}).App()
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(gz))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer lo_valid")
	resp, err := noPassApp.Test(req)
	if err != nil {
		t.Fatalf("disabled passthrough err=%v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disabled passthrough status=%d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].encoding != "" || !bytes.Equal(seen[0].body, plain) {
		t.Fatalf("explicit disable did not decode to plaintext: %+v", seen)
	}
}

func TestOTLPGzipPassthroughExplicitRealTCPAndPrometheusFormExclusions(t *testing.T) {
	type captured struct {
		path     string
		encoding string
		body     []byte
	}
	got := make(chan captured, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got <- captured{path: r.URL.Path, encoding: r.Header.Get("Content-Encoding"), body: raw}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	plain := []byte("otlp-tcp-payload")
	gz := gzipBytes(t, plain)

	vmApp := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics:             upstream.URL,
		Logs:                upstream.URL,
		Traces:              upstream.URL,
		OTLPGzipPassthrough: boolPtr(true),
	}).App()

	for _, path := range []string{"/v1/metrics", "/v1/logs", "/v1/traces"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(gz))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("Authorization", "Bearer lo_valid")
		resp, err := vmApp.Test(req)
		if err != nil {
			t.Fatalf("%s err=%v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
		capReq := <-got
		if capReq.encoding != "gzip" || !bytes.Equal(capReq.body, gz) {
			t.Fatalf(
				"%s forwarded encoding=%q len=%d want gzip len=%d",
				path,
				capReq.encoding,
				len(capReq.body),
				len(gz),
			)
		}
	}

	// Form-encoded /v1/metrics must still decode so reserved isolation params are stripped.
	formBody := gzipBytes(t, []byte("extra_label=project_id%3Devil&sample=1"))
	formReq := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(formBody))
	formReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formReq.Header.Set("Content-Encoding", "gzip")
	formReq.Header.Set("Authorization", "Bearer lo_valid")
	formResp, err := vmApp.Test(formReq)
	if err != nil {
		t.Fatalf("form /v1/metrics err=%v", err)
	}
	_ = formResp.Body.Close()
	if formResp.StatusCode != http.StatusNoContent {
		t.Fatalf("form /v1/metrics status=%d", formResp.StatusCode)
	}
	capForm := <-got
	if capForm.encoding != "" || strings.Contains(string(capForm.body), "extra_label") {
		t.Fatalf("form /v1/metrics leaked encoding=%q body=%q", capForm.encoding, capForm.body)
	}

	// Multi-layer gzip or zstd must still decode to plaintext even when passthrough is enabled.
	doubleGz := gzipBytes(t, gz)
	multiReq := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(doubleGz))
	multiReq.Header.Set("Content-Type", "application/x-protobuf")
	multiReq.Header.Set("Content-Encoding", "gzip, gzip")
	multiReq.Header.Set("Authorization", "Bearer lo_valid")
	multiResp, err := vmApp.Test(multiReq)
	if err != nil {
		t.Fatalf("double gzip err=%v", err)
	}
	_ = multiResp.Body.Close()
	if multiResp.StatusCode != http.StatusNoContent {
		t.Fatalf("double gzip status=%d", multiResp.StatusCode)
	}
	capMulti := <-got
	if capMulti.encoding != "" || !bytes.Equal(capMulti.body, plain) {
		t.Fatalf("double gzip must decode to plaintext: encoding=%q body=%q", capMulti.encoding, capMulti.body)
	}

	zstdReq := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(zstdBytes(t, plain)))
	zstdReq.Header.Set("Content-Type", "application/x-protobuf")
	zstdReq.Header.Set("Content-Encoding", "zstd")
	zstdReq.Header.Set("Authorization", "Bearer lo_valid")
	zstdResp, err := vmApp.Test(zstdReq)
	if err != nil {
		t.Fatalf("zstd err=%v", err)
	}
	_ = zstdResp.Body.Close()
	if zstdResp.StatusCode != http.StatusNoContent {
		t.Fatalf("zstd status=%d", zstdResp.StatusCode)
	}
	capZstd := <-got
	if capZstd.encoding != "" || !bytes.Equal(capZstd.body, plain) {
		t.Fatalf("zstd must decode to plaintext: encoding=%q body=%q", capZstd.encoding, capZstd.body)
	}

	// Prometheus metrics backend must NEVER pass through gzip on /v1/metrics.
	promBackend, err := storagepkg.New(
		storagepkg.MetricsPrometheus,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	promApp := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Storage:             promBackend,
		OTLPGzipPassthrough: boolPtr(true),
	}).App()
	promReq := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(gz))
	promReq.Header.Set("Content-Type", "application/x-protobuf")
	promReq.Header.Set("Content-Encoding", "gzip")
	promReq.Header.Set("Authorization", "Bearer lo_valid")
	promResp, err := promApp.Test(promReq)
	if err != nil {
		t.Fatalf("prometheus /v1/metrics err=%v", err)
	}
	_ = promResp.Body.Close()
	if promResp.StatusCode != http.StatusNoContent {
		t.Fatalf("prometheus /v1/metrics status=%d", promResp.StatusCode)
	}
	capProm := <-got
	if capProm.encoding != "" || !bytes.Equal(capProm.body, plain) {
		t.Fatalf("prometheus metrics must decode to plaintext: encoding=%q body=%q", capProm.encoding, capProm.body)
	}
}

func TestOTLPGzipPassthroughRejectsBombsAndMalformedGzip(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	app := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Metrics:             upstream.URL,
		Logs:                upstream.URL,
		Traces:              upstream.URL,
		OTLPGzipPassthrough: boolPtr(true),
	}).App()

	bomb := gzipOfZeros(t, maxDecodedRequestBody+1)
	valid := gzipBytes(t, []byte("valid-otlp-payload"))
	truncated := valid[:len(valid)-6]
	badCRC := append([]byte(nil), valid...)
	badCRC[len(badCRC)-5] ^= 0xff
	trailing := append(append([]byte(nil), valid...), []byte("garbage")...)

	cases := []struct {
		name string
		path string
		body []byte
		want int
	}{
		{"metrics bomb", "/v1/metrics", bomb, http.StatusRequestEntityTooLarge},
		{"logs bomb", "/v1/logs", bomb, http.StatusRequestEntityTooLarge},
		{"traces bomb", "/v1/traces", bomb, http.StatusRequestEntityTooLarge},
		{"corrupt header", "/v1/logs", []byte("not-gzip"), http.StatusBadRequest},
		{"truncated stream", "/v1/logs", truncated, http.StatusBadRequest},
		{"bad crc32", "/v1/traces", badCRC, http.StatusBadRequest},
		{"trailing garbage", "/v1/metrics", trailing, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("Authorization", "Bearer lo_valid")
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.want, readBody(resp))
			}
		})
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("invalid or oversized gzip reached upstream %d times", calls)
	}

	// Exact boundary check on validateGzipBounded.
	exactLimit := gzipOfZeros(t, 4096)
	n, err := validateGzipBounded(exactLimit, 4096)
	if err != nil || n != 4096 {
		t.Fatalf("validateGzipBounded(exact) = (%d, %v), want (4096, nil)", n, err)
	}
	overLimit := gzipOfZeros(t, 4097)
	if _, err := validateGzipBounded(overLimit, 4096); !errors.Is(err, errDecodedBodyTooLarge) {
		t.Fatalf("validateGzipBounded(over) err=%v want errDecodedBodyTooLarge", err)
	}
}

func TestDecodeBufferPoolDropsOversizedBurstBuffers(t *testing.T) {
	// Drain any pooled buffers left by earlier tests.
	for decodeBufferPool.Get().(*pooledDecodeBuf).buf.Cap() > 0 {
	}

	largePayload := bytes.Repeat([]byte("A"), maxPooledDecodeBufCap+8192)
	gzLarge := gzipBytes(t, largePayload)

	app := newFiberApp(defaultReadTimeout, defaultIdleTimeout, nil, nil)
	app.Use(boundedRequestDecoding)
	app.Post("/burst", func(c fiber.Ctx) error {
		if len(requestBody(c)) != len(largePayload) {
			return fiber.NewError(fiber.StatusBadRequest, "length mismatch")
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/burst", bytes.NewReader(gzLarge))
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// Ensure no oversized buffer was retained in decodeBufferPool.
	for range 4 {
		pb := decodeBufferPool.Get().(*pooledDecodeBuf)
		if pb.buf.Cap() > maxPooledDecodeBufCap {
			t.Fatalf("decodeBufferPool retained oversized buffer cap=%d > %d", pb.buf.Cap(), maxPooledDecodeBufCap)
		}
	}
}

func TestPooledGzipDecodeAndPassthroughConcurrentRace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "bad") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	loopbackApp := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Logs:                upstream.URL,
		OTLPGzipPassthrough: boolPtr(false),
	}).App()
	passthroughApp := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		Logs:                upstream.URL,
		OTLPGzipPassthrough: boolPtr(true),
	}).App()

	payload := bytes.Repeat([]byte("concurrent-otlp-log-entry-"), 256)
	gz := gzipBytes(t, payload)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 20 {
				targetApp := loopbackApp
				if (id+j)%2 == 1 {
					targetApp = passthroughApp
				}
				path := "/v1/logs"
				want := http.StatusNoContent
				if j%5 == 0 {
					path = "/v1/logs?bad=1"
					want = http.StatusBadRequest
				}
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(gz))
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Set("Content-Encoding", "gzip")
				req.Header.Set("Authorization", "Bearer lo_valid")
				resp, err := targetApp.Test(req)
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode != want {
					t.Errorf("status=%d want=%d", resp.StatusCode, want)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func BenchmarkGzipDecodePooled(b *testing.B) {
	payload := bytes.Repeat(
		[]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"benchmark log record"}}]}]}]}`),
		64,
	)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(payload); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	gz := out.Bytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		pb, decoded, err := decodeBoundedPooled("gzip", gz, maxDecodedRequestBody)
		if err != nil || len(decoded) != len(payload) {
			b.Fatalf("decode failed: len=%d err=%v", len(decoded), err)
		}
		pb.release()
	}
}

func BenchmarkGzipValidatePassthrough(b *testing.B) {
	payload := bytes.Repeat(
		[]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"benchmark log record"}}]}]}]}`),
		64,
	)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(payload); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	gz := out.Bytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		n, err := validateGzipBounded(gz, maxDecodedRequestBody)
		if err != nil || n != int64(len(payload)) {
			b.Fatalf("validate failed: n=%d err=%v", n, err)
		}
	}
}
