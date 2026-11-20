package embeddedpg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

const (
	// fetchAttempts is how many times each Maven Central request is tried before the
	// download is reported as failed.
	fetchAttempts = 5
	// fetchBackoff is the delay before the second attempt; it doubles for every further one.
	fetchBackoff = 2 * time.Second
	// fetchMaxDelay caps every wait, including a server-supplied Retry-After.
	fetchMaxDelay = 30 * time.Second
	// fetchTimeout bounds one request, including reading the ~15 MB jar.
	fetchTimeout = 2 * time.Minute
	// fetchBudget bounds the retries of one Prepare: no wait ends and no request starts past it, so Prepare
	// returns within fetchBudget plus one fetchTimeout (7 minutes), inside go test's default
	// 10-minute -timeout and the 30-minute meta-pg job, and the caller still reports the
	// last HTTP status instead of a test-timeout panic. It covers the worst case of every
	// wait capped at fetchMaxDelay for both the jar and the .sha256 (8 x 30s = 4 minutes).
	fetchBudget = 5 * time.Minute
	// fetchMaxBytes bounds what one response may put in memory.
	fetchMaxBytes = 256 << 20
)

// Prepare makes sure the Postgres archive for version is in cacheDir, laid out the way
// embedded-postgres looks it up, so Start finds a cache hit and never downloads on its own.
//
// embedded-postgres downloads with a single http.Get: one 429 from Maven Central (shared CI
// egress IPs get rate limited) or one stray 404 fails the whole run, and it skips the
// checksum comparison whenever the .sha256 request is not a 200. Prepare retries 403, 404,
// 408, 429, 5xx and network errors with exponential backoff, treats other statuses as
// permanent, requires the published .sha256 to be fetched and to match, and returns an
// error when it cannot get a verified archive. It never skips the download silently.
func Prepare(version embeddedpostgres.PostgresVersion, cacheDir string) error {
	operatingSystem, architecture := archivePlatform()

	return newFetcher().prepare(operatingSystem, architecture, version, cacheDir)
}

type fetcher struct {
	repository string
	client     *http.Client
	attempts   int
	backoff    time.Duration
	maxDelay   time.Duration
	budget     time.Duration
	now        func() time.Time
	sleep      func(time.Duration)
	logf       func(format string, args ...any)
}

func newFetcher() fetcher {
	return fetcher{
		repository: binaryRepository,
		client:     &http.Client{Timeout: fetchTimeout},
		attempts:   fetchAttempts,
		backoff:    fetchBackoff,
		maxDelay:   fetchMaxDelay,
		budget:     fetchBudget,
		now:        time.Now,
		sleep:      time.Sleep,
		logf: func(format string, args ...any) {
			_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
}

func (f fetcher) prepare(
	operatingSystem, architecture string,
	version embeddedpostgres.PostgresVersion,
	cacheDir string,
) error {
	target := archivePath(cacheDir, operatingSystem, architecture, version)
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		return nil
	}

	url := jarURL(f.repository, operatingSystem, architecture, version)
	deadline := f.now().Add(f.budget)

	jar, err := f.get(url, deadline)
	if err != nil {
		return err
	}

	published, err := f.get(url+".sha256", deadline)
	if err != nil {
		return err
	}

	if err := verifyChecksum(jar, published, url); err != nil {
		return err
	}

	archive, err := extractArchive(jar, url)
	if err != nil {
		return err
	}

	return writeAtomically(cacheDir, target, archive)
}

// get fetches url, retrying retryable failures until attempts or the deadline run out, and
// returns the body of a 200 response.
func (f fetcher) get(url string, deadline time.Time) ([]byte, error) {
	// A request only starts before the deadline, so one Prepare ends within budget plus one
	// request timeout even when the jar succeeds late and the .sha256 would be next.
	if f.now().After(deadline) {
		return nil, fmt.Errorf("download %s not started, retry budget of %s exhausted", url, f.budget)
	}

	var lastErr error

	for attempt := 1; attempt <= f.attempts; attempt++ {
		body, status, retryAfter, err := f.getOnce(url)
		if err == nil {
			return body, nil
		}

		lastErr = err
		if status != 0 && !retryableStatus(status) {
			return nil, fmt.Errorf("download %s: %w", url, err)
		}

		if attempt == f.attempts {
			break
		}

		delay := f.delay(attempt, retryAfter)
		if f.now().Add(delay).After(deadline) {
			return nil, fmt.Errorf(
				"download %s gave up after %d attempts, retry budget of %s exhausted: %w",
				url,
				attempt,
				f.budget,
				lastErr,
			)
		}
		f.logf("embedded postgres download: %v; retry %d/%d in %s", err, attempt, f.attempts-1, delay)
		f.sleep(delay)
	}

	return nil, fmt.Errorf("download %s failed after %d attempts: %w", url, f.attempts, lastErr)
}

// getOnce returns the status as 0 for transport errors so get can tell them from HTTP answers.
func (f fetcher) getOnce(url string) ([]byte, int, time.Duration, error) {
	response, err := f.client.Get(url)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After")),
			fmt.Errorf("HTTP %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, fetchMaxBytes+1))
	if err != nil {
		return nil, 0, 0, err
	}
	if len(body) > fetchMaxBytes {
		return nil, response.StatusCode, 0, fmt.Errorf("response larger than %d bytes", fetchMaxBytes)
	}

	return body, response.StatusCode, 0, nil
}

// delay is the exponential backoff for attempt (1-based), raised to a server-supplied
// Retry-After and capped at maxDelay.
func (f fetcher) delay(attempt int, retryAfter time.Duration) time.Duration {
	delay := f.backoff << (attempt - 1)
	if retryAfter > delay {
		delay = retryAfter
	}

	return min(delay, f.maxDelay)
}

// retryableStatus covers rate limiting (403 and 429 from a shared egress IP), request
// timeouts, server errors, and 404: Maven Central answered 404 once for a jar that exists
// (PR #69), and a version that is really gone still fails after the last attempt.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}

	return status >= http.StatusInternalServerError
}

// parseRetryAfter understands the delay-seconds form; an HTTP-date or garbage means no hint.
func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return 0
	}

	return time.Duration(seconds) * time.Second
}

func verifyChecksum(jar, published []byte, jarURL string) error {
	fields := strings.Fields(string(published))
	if len(fields) == 0 {
		return fmt.Errorf("%s.sha256 is empty", jarURL)
	}

	sum := sha256.Sum256(jar)
	if !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
		return fmt.Errorf(
			"sha256 of %s is %x, published checksum is %s",
			jarURL,
			sum,
			fields[0],
		)
	}

	return nil
}

func extractArchive(jar []byte, jarURL string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(jar), int64(len(jar)))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", jarURL, err)
	}

	for _, file := range reader.File {
		if file.FileInfo().IsDir() || !strings.HasSuffix(file.Name, ".txz") {
			continue
		}

		entry, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s in %s: %w", file.Name, jarURL, err)
		}
		defer func() { _ = entry.Close() }()

		archive, err := io.ReadAll(io.LimitReader(entry, fetchMaxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read %s in %s: %w", file.Name, jarURL, err)
		}
		if len(archive) > fetchMaxBytes {
			return nil, fmt.Errorf("%s in %s is larger than %d bytes", file.Name, jarURL, fetchMaxBytes)
		}

		return archive, nil
	}

	return nil, fmt.Errorf("no .txz archive inside %s", jarURL)
}

// writeAtomically publishes content at target through a temporary file in the same directory,
// so a concurrent test binary or an interrupted run never sees a partial archive.
func writeAtomically(directory, target string, content []byte) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", directory, err)
	}

	temp, err := os.CreateTemp(directory, "temp_")
	if err != nil {
		return fmt.Errorf("create temporary archive in %s: %w", directory, err)
	}
	defer func() { _ = os.Remove(temp.Name()) }()

	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()

		return fmt.Errorf("write %s: %w", temp.Name(), err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", temp.Name(), err)
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return fmt.Errorf("publish %s: %w", target, err)
	}

	return nil
}
