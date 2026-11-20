package embeddedpg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

const (
	testVersion = "18.3.0"
	testTxzName = "embedded-postgres-binaries-linux-amd64-18.3.0.txz"
	testJarPath = "/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/18.3.0/" +
		"embedded-postgres-binaries-linux-amd64-18.3.0.jar"
)

// fakeMaven serves the jar and its .sha256 sibling. Each path answers with the statuses in
// script first (one per request, in order) and with the real payload once the script runs out.
type fakeMaven struct {
	mu       sync.Mutex
	script   map[string][]int
	retry    map[string]string
	requests map[string]int
	jar      []byte
	sha      string
}

func newFakeMaven(t *testing.T, txz []byte) (*fakeMaven, *httptest.Server) {
	t.Helper()

	jar := zipWithEntries(t, map[string][]byte{"META-INF/MANIFEST.MF": []byte("x"), testTxzName: txz})
	sum := sha256.Sum256(jar)
	maven := &fakeMaven{
		script:   map[string][]int{},
		retry:    map[string]string{},
		requests: map[string]int{},
		jar:      jar,
		sha:      hex.EncodeToString(sum[:]),
	}
	server := httptest.NewServer(maven)
	t.Cleanup(server.Close)

	return maven, server
}

func (m *fakeMaven) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	m.mu.Lock()
	m.requests[request.URL.Path]++
	var status int
	if queue := m.script[request.URL.Path]; len(queue) > 0 {
		status = queue[0]
		m.script[request.URL.Path] = queue[1:]
	}
	retryAfter := m.retry[request.URL.Path]
	m.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		if retryAfter != "" {
			writer.Header().Set("Retry-After", retryAfter)
		}
		writer.WriteHeader(status)

		return
	}
	switch request.URL.Path {
	case testJarPath:
		_, _ = writer.Write(m.jar)
	case testJarPath + ".sha256":
		_, _ = writer.Write([]byte(m.sha))
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (m *fakeMaven) count(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.requests[path]
}

func zipWithEntries(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

// sleepRecorder is a fake clock: sleep records the delay and advances now instead of waiting.
type sleepRecorder struct {
	delays  []time.Duration
	elapsed time.Duration
}

func (r *sleepRecorder) sleep(delay time.Duration) {
	r.delays = append(r.delays, delay)
	r.elapsed += delay
}

func (r *sleepRecorder) now() time.Time { return time.Unix(0, 0).Add(r.elapsed) }

func testFetcher(server *httptest.Server, recorder *sleepRecorder) fetcher {
	return fetcher{
		repository: server.URL,
		client:     server.Client(),
		attempts:   4,
		backoff:    time.Second,
		maxDelay:   30 * time.Second,
		budget:     time.Hour,
		now:        recorder.now,
		sleep:      recorder.sleep,
		logf:       func(string, ...any) {},
	}
}

func TestPrepareRetriesRateLimitAndWritesVerifiedArchive(t *testing.T) {
	payload := []byte("fake-txz-payload")
	maven, server := newFakeMaven(t, payload)
	maven.script[testJarPath] = []int{http.StatusTooManyRequests, http.StatusTooManyRequests}
	maven.script[testJarPath+".sha256"] = []int{http.StatusServiceUnavailable}
	recorder := &sleepRecorder{}
	cache := t.TempDir()

	if err := testFetcher(server, recorder).prepare("linux", "amd64", testVersion, cache); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(cache, testTxzName))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("cached archive = %q, %v; want %q", got, err, payload)
	}
	if n := maven.count(testJarPath); n != 3 {
		t.Fatalf("jar requests = %d, want 3 (two 429 then 200)", n)
	}
	if n := maven.count(testJarPath + ".sha256"); n != 2 {
		t.Fatalf(".sha256 requests = %d, want 2 (one 503 then 200)", n)
	}
	// Exponential backoff per request: 1s, 2s for the jar, 1s for the checksum.
	want := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if len(recorder.delays) != len(want) {
		t.Fatalf("delays = %v, want %v", recorder.delays, want)
	}
	for i := range want {
		if recorder.delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", recorder.delays, want)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(cache, "temp_*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestPrepareFailsClosedWhenArchiveStaysMissing(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	maven.script[testJarPath] = []int{404, 404, 404, 404, 404}
	cache := t.TempDir()

	err := testFetcher(server, &sleepRecorder{}).prepare("linux", "amd64", testVersion, cache)

	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("prepare error = %v, want a 404 failure", err)
	}
	if n := maven.count(testJarPath); n != 4 {
		t.Fatalf("jar requests = %d, want the 4 allowed attempts", n)
	}
	if _, statErr := os.Stat(filepath.Join(cache, testTxzName)); !os.IsNotExist(statErr) {
		t.Fatalf("archive must not exist after a failed download, stat err = %v", statErr)
	}
}

func TestPrepareRecoversFromTransientNotFound(t *testing.T) {
	// PR #69 saw one 404 for a jar that exists; the next request succeeded.
	maven, server := newFakeMaven(t, []byte("x"))
	maven.script[testJarPath] = []int{404}

	if err := testFetcher(server, &sleepRecorder{}).prepare("linux", "amd64", testVersion, t.TempDir()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
}

func TestPrepareDoesNotRetryPermanentClientErrors(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	maven.script[testJarPath] = []int{http.StatusBadRequest}
	recorder := &sleepRecorder{}

	err := testFetcher(server, recorder).prepare("linux", "amd64", testVersion, t.TempDir())

	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("prepare error = %v, want a 400 failure", err)
	}
	if n := maven.count(testJarPath); n != 1 || len(recorder.delays) != 0 {
		t.Fatalf("jar requests = %d, sleeps = %v; want 1 request and no retry", n, recorder.delays)
	}
}

func TestPrepareRejectsChecksumMismatch(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	maven.sha = strings.Repeat("0", 64)
	cache := t.TempDir()

	err := testFetcher(server, &sleepRecorder{}).prepare("linux", "amd64", testVersion, cache)

	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("prepare error = %v, want a sha256 mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, testTxzName)); !os.IsNotExist(statErr) {
		t.Fatalf("archive must not be cached after a checksum mismatch, stat err = %v", statErr)
	}
}

func TestPrepareFailsWhenChecksumUnavailable(t *testing.T) {
	// embedded-postgres itself skips verification when the .sha256 request is not a 200;
	// the prefetch must not.
	maven, server := newFakeMaven(t, []byte("x"))
	maven.script[testJarPath+".sha256"] = []int{429, 429, 429, 429}
	cache := t.TempDir()

	err := testFetcher(server, &sleepRecorder{}).prepare("linux", "amd64", testVersion, cache)

	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("prepare error = %v, want a 429 failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, testTxzName)); !os.IsNotExist(statErr) {
		t.Fatalf("unverified archive must not be cached, stat err = %v", statErr)
	}
}

func TestPrepareHonorsRetryAfterWithinCap(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	maven.script[testJarPath] = []int{429, 429}
	maven.retry[testJarPath] = "7"
	recorder := &sleepRecorder{}

	if err := testFetcher(server, recorder).prepare("linux", "amd64", testVersion, t.TempDir()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(recorder.delays) != 2 || recorder.delays[0] != 7*time.Second || recorder.delays[1] != 7*time.Second {
		t.Fatalf("delays = %v, want Retry-After of 7s twice", recorder.delays)
	}

	maven.script[testJarPath] = []int{429}
	maven.retry[testJarPath] = "3600"
	recorder = &sleepRecorder{}
	if err := testFetcher(server, recorder).prepare("linux", "amd64", testVersion, t.TempDir()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(recorder.delays) != 1 || recorder.delays[0] != 30*time.Second {
		t.Fatalf("delays = %v, want Retry-After capped at 30s", recorder.delays)
	}
}

func TestPrepareStopsRetryingWhenBudgetIsSpent(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	maven.script[testJarPath] = []int{429, 429, 429, 429}
	maven.retry[testJarPath] = "30"
	recorder := &sleepRecorder{}
	fetcher := testFetcher(server, recorder)
	fetcher.budget = 50 * time.Second

	err := fetcher.prepare("linux", "amd64", testVersion, t.TempDir())

	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("prepare error = %v, want a 429 failure that names the spent budget", err)
	}
	// 30s fits in the 50s budget; a second 30s wait would end past it.
	if n := maven.count(testJarPath); n != 2 || len(recorder.delays) != 1 {
		t.Fatalf("jar requests = %d, sleeps = %v; want 2 requests and one wait", n, recorder.delays)
	}
	if n := maven.count(testJarPath + ".sha256"); n != 0 {
		t.Fatalf(".sha256 requests = %d, want none after the jar failed", n)
	}
}

func TestPrepareDoesNotStartChecksumPastBudget(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	recorder := &sleepRecorder{}
	fetcher := testFetcher(server, recorder)
	fetcher.budget = time.Minute
	// The jar request itself takes past the deadline (a slow download on the last attempt).
	client := server.Client()
	client.Transport = advanceClock{next: client.Transport, recorder: recorder, by: 2 * time.Minute}
	fetcher.client = client

	err := fetcher.prepare("linux", "amd64", testVersion, t.TempDir())

	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("prepare error = %v, want the spent budget", err)
	}
	if n := maven.count(testJarPath + ".sha256"); n != 0 {
		t.Fatalf(".sha256 requests = %d, want none once the budget is spent", n)
	}
}

// advanceClock moves the fake clock forward by the given duration on every request.
type advanceClock struct {
	next     http.RoundTripper
	recorder *sleepRecorder
	by       time.Duration
}

func (a advanceClock) RoundTrip(request *http.Request) (*http.Response, error) {
	a.recorder.elapsed += a.by

	return a.next.RoundTrip(request)
}

// TestPrepareTargetIsEmbeddedPostgresCacheHit pins Prepare's file name to the cache lookup of the
// embedded-postgres version in go.mod on the platform running the test: with a file at the path
// Prepare writes, Start must not touch the repository. A mismatch would let Start fall back to
// its own single-attempt download that skips the checksum when the .sha256 is not a 200.
func TestPrepareTargetIsEmbeddedPostgresCacheHit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	cache := t.TempDir()
	operatingSystem, architecture := archivePlatform()
	target := archivePath(cache, operatingSystem, architecture, embeddedpostgres.V18)
	if err := os.WriteFile(target, []byte("not an xz archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	root := t.TempDir()

	database := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(port).
		CachePath(cache).
		RuntimePath(filepath.Join(root, "runtime")).
		DataPath(filepath.Join(root, "data")).
		BinaryRepositoryURL(server.URL).
		Logger(io.Discard))
	err = database.Start()
	if err == nil {
		_ = database.Stop()
		t.Fatal("Start succeeded with a fake archive; want an extraction error")
	}

	if n := requests.Load(); n != 0 {
		t.Fatalf("Start made %d repository requests (%v); %s is not its cache hit", n, err, target)
	}
	if !strings.Contains(err.Error(), "extract") {
		t.Fatalf("Start error = %v, want it to extract the cached %s", err, target)
	}
}

func TestPrepareSkipsNetworkWhenArchiveIsCached(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, testTxzName), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := testFetcher(server, &sleepRecorder{}).prepare("linux", "amd64", testVersion, cache); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if n := maven.count(testJarPath) + maven.count(testJarPath+".sha256"); n != 0 {
		t.Fatalf("requests = %d, want none on a cache hit", n)
	}
}

func TestPrepareRejectsJarWithoutArchive(t *testing.T) {
	maven, server := newFakeMaven(t, []byte("x"))
	maven.jar = zipWithEntries(t, map[string][]byte{"META-INF/MANIFEST.MF": []byte("x")})
	sum := sha256.Sum256(maven.jar)
	maven.sha = hex.EncodeToString(sum[:])

	err := testFetcher(server, &sleepRecorder{}).prepare("linux", "amd64", testVersion, t.TempDir())

	if err == nil || !strings.Contains(err.Error(), ".txz") {
		t.Fatalf("prepare error = %v, want a missing .txz failure", err)
	}
}
