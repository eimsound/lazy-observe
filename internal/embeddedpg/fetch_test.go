package embeddedpg

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedServer answers each request with the next status in script and with 200 and body
// once the script runs out. It records every request path.
type scriptedServer struct {
	mu     sync.Mutex
	script []int
	retry  string
	paths  []string
	body   string
}

func newScriptedServer(t *testing.T, script ...int) (*scriptedServer, *httptest.Server) {
	t.Helper()

	scripted := &scriptedServer{script: script, body: "payload"}
	server := httptest.NewServer(scripted)
	t.Cleanup(server.Close)

	return scripted, server
}

func (s *scriptedServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, request.URL.Path)
	status := http.StatusOK
	if len(s.script) > 0 {
		status, s.script = s.script[0], s.script[1:]
	}
	retryAfter := s.retry
	s.mu.Unlock()

	if status != http.StatusOK {
		if retryAfter != "" {
			writer.Header().Set("Retry-After", retryAfter)
		}
		writer.WriteHeader(status)

		return
	}
	_, _ = io.WriteString(writer, s.body)
}

func (s *scriptedServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.paths...)
}

// sleepRecorder replaces the real wait: it records the delay and returns at once.
type sleepRecorder struct{ delays []time.Duration }

func (r *sleepRecorder) sleep(_ context.Context, delay time.Duration) error {
	r.delays = append(r.delays, delay)

	return nil
}

func testTransport(recorder *sleepRecorder) retryTransport {
	return retryTransport{
		next:     http.DefaultTransport,
		attempts: 4,
		backoff:  time.Second,
		maxDelay: 30 * time.Second,
		sleep:    recorder.sleep,
		logf:     func(string, ...any) {},
	}
}

func get(t *testing.T, transport http.RoundTripper, url string) (*http.Response, error) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if response != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}

	return response, err
}

func TestRetryTransportRetriesRateLimitWithBackoff(t *testing.T) {
	scripted, server := newScriptedServer(
		t,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
		http.StatusBadGateway,
	)
	recorder := &sleepRecorder{}

	response, err := get(t, testTransport(recorder), server.URL+"/a")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %v, %v; want 200", response, err)
	}

	if n := len(scripted.requests()); n != 4 {
		t.Fatalf("requests = %d, want 4 (three failures then 200)", n)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(recorder.delays) != len(want) {
		t.Fatalf("delays = %v, want %v", recorder.delays, want)
	}
	for i := range want {
		if recorder.delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", recorder.delays, want)
		}
	}
}

func TestRetryTransportRecoversFromTransientForbiddenAndNotFound(t *testing.T) {
	scripted, server := newScriptedServer(t, http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout)

	response, err := get(t, testTransport(&sleepRecorder{}), server.URL+"/a")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %v, %v; want 200", response, err)
	}
	if n := len(scripted.requests()); n != 4 {
		t.Fatalf("requests = %d, want 4", n)
	}
}

func TestRetryTransportReturnsLastStatusWhenAttemptsRunOut(t *testing.T) {
	scripted, server := newScriptedServer(t, 404, 404, 404, 404, 404)

	response, err := get(t, testTransport(&sleepRecorder{}), server.URL+"/a")

	if err != nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %v, %v; want the final 404 so the caller can report it", response, err)
	}
	if n := len(scripted.requests()); n != 4 {
		t.Fatalf("requests = %d, want the 4 allowed attempts", n)
	}
}

func TestRetryTransportDoesNotRetryPermanentClientErrors(t *testing.T) {
	scripted, server := newScriptedServer(t, http.StatusBadRequest)
	recorder := &sleepRecorder{}

	response, err := get(t, testTransport(recorder), server.URL+"/a")

	if err != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %v, %v; want 400", response, err)
	}
	if n := len(scripted.requests()); n != 1 || len(recorder.delays) != 0 {
		t.Fatalf("requests = %d, sleeps = %v; want one request and no retry", n, recorder.delays)
	}
}

func TestRetryTransportHonorsRetryAfterWithinCap(t *testing.T) {
	scripted, server := newScriptedServer(t, 429, 429)
	scripted.retry = "7"
	recorder := &sleepRecorder{}

	if _, err := get(t, testTransport(recorder), server.URL+"/a"); err != nil {
		t.Fatal(err)
	}
	if len(recorder.delays) != 2 || recorder.delays[0] != 7*time.Second || recorder.delays[1] != 7*time.Second {
		t.Fatalf("delays = %v, want Retry-After of 7s twice", recorder.delays)
	}

	scripted.mu.Lock()
	scripted.script, scripted.retry = []int{429}, "3600"
	scripted.mu.Unlock()
	recorder = &sleepRecorder{}
	if _, err := get(t, testTransport(recorder), server.URL+"/a"); err != nil {
		t.Fatal(err)
	}
	if len(recorder.delays) != 1 || recorder.delays[0] != 30*time.Second {
		t.Fatalf("delays = %v, want Retry-After capped at 30s", recorder.delays)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestRetryTransportRetriesNetworkErrorsAndReturnsTheLastOne(t *testing.T) {
	calls := 0
	transport := testTransport(&sleepRecorder{})
	transport.next = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++

		return nil, errors.New("connection reset by peer")
	})

	_, err := get(t, transport, "http://example.invalid/a")

	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, want the last network error", err)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want the 4 allowed attempts", calls)
	}
}

func TestRetryTransportStopsWaitingWhenContextIsDone(t *testing.T) {
	_, server := newScriptedServer(t, 429, 429, 429)
	transport := testTransport(&sleepRecorder{})
	ctx, cancel := context.WithCancel(t.Context())
	transport.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()

		return ctx.Err()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/a", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = transport.RoundTrip(request)

	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want context.Canceled that names the last 429", err)
	}
}

// TestPrepareFollowsTheLibraryAssetContract pins that prepare goes through the library's own
// request (/VERSION/postgresql-VERSION-TARGET.tar.gz under BaseURL) with the retrying client,
// and fails closed on a payload whose SHA-256 differs from the manifest pin instead of caching it.
func TestPrepareFollowsTheLibraryAssetContract(t *testing.T) {
	scripted, server := newScriptedServer(t, http.StatusTooManyRequests)
	scripted.body = "not the pinned distribution"
	cache := t.TempDir()
	provider := newProvider(cache, testTransport(&sleepRecorder{}))
	provider.BaseURL = server.URL

	err := prepare(provider, Version)

	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("prepare error = %v, want a SHA-256 mismatch (fail closed)", err)
	}
	requests := scripted.requests()
	if len(requests) != 2 || requests[0] != requests[1] {
		t.Fatalf("requests = %v, want the 429 retried once on the same path", requests)
	}
	version := string(Version)
	name := path.Base(requests[0])
	if path.Dir(requests[0]) != "/"+version ||
		!strings.HasPrefix(name, "postgresql-"+version+"-") ||
		!strings.HasSuffix(name, ".tar.gz") {
		t.Fatalf("requested %s, want /%s/postgresql-%s-<target>.tar.gz", requests[0], version, version)
	}

	entries, readErr := os.ReadDir(cache)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "postgresql-") && !strings.HasSuffix(entry.Name(), ".lock") {
			t.Fatalf("cache entry %s exists after a failed download", entry.Name())
		}
	}
}
