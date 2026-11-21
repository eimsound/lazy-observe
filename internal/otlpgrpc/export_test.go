package otlpgrpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
)

func TestClassifyTransportErrorPrefersShutdownUnavailable(t *testing.T) {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	shutdownCancel()

	// Even when the underlying HTTP request reports context.Canceled (and even
	// if parentCtx was also canceled during forced stop), shutdown takes priority
	// so OTel clients retry against another replica instead of treating it as a
	// client-initiated cancel.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	code, msg := classifyTransportError(parentCtx, context.Background(), shutdownCtx, context.Canceled)
	if code != codes.Unavailable || msg != "server is shutting down" {
		t.Fatalf("shutdown cancel got (%s, %q), want (Unavailable, %q)", code, msg, "server is shutting down")
	}

	// Without server shutdown, client cancellation stays Canceled.
	parentCancel()
	code, msg = classifyTransportError(parentCtx, context.Background(), context.Background(), context.Canceled)
	if code != codes.Canceled || msg != "request canceled" {
		t.Fatalf("client cancel got (%s, %q), want (Canceled, %q)", code, msg, "request canceled")
	}
}

func TestParseRetryAfterBoundsAndFallbacks(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{raw: "", want: time.Second},
		{raw: "   ", want: time.Second},
		{raw: "0", want: time.Second},
		{raw: "-5", want: time.Second},
		{raw: "not-a-number", want: time.Second},
		{raw: "5", want: 5 * time.Second},
		{raw: " 12 ", want: 12 * time.Second},
		{raw: "999999999999999", want: maxRetryAfterDelay},
		{raw: "9223372036854775807", want: maxRetryAfterDelay},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.raw); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.raw, got, tc.want)
		}
	}
}
