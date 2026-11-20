package embeddedpg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// fetchAttempts is how many times each request is tried before its last answer is returned.
	fetchAttempts = 5
	// fetchBackoff is the delay before the second attempt; it doubles for every further one.
	fetchBackoff = 2 * time.Second
	// fetchMaxDelay caps every wait, including a server-supplied Retry-After.
	fetchMaxDelay = 30 * time.Second
	// drainLimit bounds how much of a discarded error response is read so the connection can be reused.
	drainLimit = 64 << 10
)

// retryTransport retries transient failures of idempotent GET requests. When the attempts run
// out it returns the last response (or error) unchanged, so the caller reports the real HTTP
// status. The wait ends early, with the context error, when the request context is done; the
// http.Client timeout therefore bounds the whole retry sequence.
type retryTransport struct {
	next     http.RoundTripper
	attempts int
	backoff  time.Duration
	maxDelay time.Duration
	sleep    func(ctx context.Context, delay time.Duration) error
	logf     func(format string, args ...any)
}

func newRetryTransport() retryTransport {
	return retryTransport{
		next:     http.DefaultTransport,
		attempts: fetchAttempts,
		backoff:  fetchBackoff,
		maxDelay: fetchMaxDelay,
		sleep:    sleepContext,
		logf: func(format string, args ...any) {
			_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
}

func (t retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		response, err := t.next.RoundTrip(request)
		retryAfter := time.Duration(0)
		reason := ""

		switch {
		case err != nil:
			if request.Context().Err() != nil {
				return nil, err
			}
			reason = err.Error()
		case retryableStatus(response.StatusCode):
			retryAfter = parseRetryAfter(response.Header.Get("Retry-After"))
			reason = "HTTP " + response.Status
		default:
			return response, nil
		}

		if attempt >= t.attempts {
			return response, err
		}
		if response != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, drainLimit))
			_ = response.Body.Close()
		}

		delay := t.delay(attempt, retryAfter)
		t.logf(
			"embedded postgres download %s: %s; retry %d/%d in %s",
			request.URL.Redacted(), reason, attempt, t.attempts-1, delay,
		)
		if sleepErr := t.sleep(request.Context(), delay); sleepErr != nil {
			return nil, fmt.Errorf(
				"%s after %d attempts, last answer %s: %w",
				request.URL.Redacted(), attempt, reason, sleepErr,
			)
		}
	}
}

// delay is the exponential backoff for attempt (1-based), raised to a server-supplied
// Retry-After and capped at maxDelay.
func (t retryTransport) delay(attempt int, retryAfter time.Duration) time.Duration {
	delay := t.backoff << (attempt - 1)
	if retryAfter > delay {
		delay = retryAfter
	}

	return min(delay, t.maxDelay)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryableStatus covers rate limiting (403 and 429 from a shared egress IP), request
// timeouts, server errors, and 404: Maven Central answered 404 once for an archive that exists
// (PR #69). The pinned distribution is expected to exist, so a 404 that persists still fails
// after the last attempt.
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
