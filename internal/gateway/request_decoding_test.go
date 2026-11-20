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
)

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

func TestBoundedRequestDecodingRejectsCorruptAndUnsupportedEncodings(t *testing.T) {
	app := newTestGateway(Config{Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}}}).App()
	for _, test := range []struct {
		name, encoding string
		body           []byte
		want           int
	}{
		{"corrupt gzip", "gzip", []byte("not gzip"), http.StatusBadRequest},
		{"unknown encoding stacked on gzip", "zstd, gzip", gzipBytes(t, []byte("{}")), http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/events/errors", bytes.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", test.encoding)
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

// A single coding the gateway does not decode (snappy, zstd) must reach the
// storage upstream byte for byte with its Content-Encoding header. Fiber v3's
// Ctx.Body decodes or rejects such codings itself; handlers read requestBody
// instead so the upstream, which understands them, sees the original request.
func TestProxyForwardsUndecodedContentEncodingUntouched(t *testing.T) {
	type seen struct {
		body     []byte
		encoding string
	}
	received := make(chan seen, 1)
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
	for _, coding := range []string{"zstd", "snappy"} {
		t.Run(coding, func(t *testing.T) {
			payload := []byte("\x28\xb5\x2f\xfd not really " + coding + " \x00\x01\x02")
			req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("Content-Encoding", coding)
			req.Header.Set("Authorization", "Bearer lou_valid")
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status=%d want=204 body=%s", resp.StatusCode, readBody(resp))
			}
			got := <-received
			if got.encoding != coding || !bytes.Equal(got.body, payload) {
				t.Fatalf(
					"upstream saw encoding=%q body=%q, want encoding=%q body=%q",
					got.encoding,
					got.body,
					coding,
					payload,
				)
			}
		})
	}
}
