package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
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
