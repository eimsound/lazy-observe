package lazyobserve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIngestClientIsolatesFromDefaultClientAndClassifiesStatuses(t *testing.T) {
	origDefault := http.DefaultClient
	http.DefaultClient = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("http.DefaultClient must not be used by ingestClient")
		}),
	}
	t.Cleanup(func() { http.DefaultClient = origDefault })

	var gotAuth, gotContentType, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)

	client := newIngestClient(server.URL, "lo_secret", nil)
	payload := []byte(`{"events":[]}`)
	err := client.post(context.Background(), ingestRequest{
		Path:          "/api/events/errors",
		ContentType:   "application/json",
		ContentLength: int64(len(payload)),
		Body:          func() io.ReadCloser { return io.NopCloser(bytes.NewReader(payload)) },
		Timeout:       2 * time.Second,
		Op:            "send error events",
	})
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}
	if gotAuth != "Bearer lo_secret" || gotContentType != "application/json" || gotBody != string(payload) {
		t.Fatalf("auth=%q contentType=%q body=%q", gotAuth, gotContentType, gotBody)
	}

	cases := []struct {
		status        int
		retryAfter    string
		wantPermanent bool
		wantMinDelay  time.Duration
	}{
		{http.StatusBadRequest, "", true, 0},
		{http.StatusUnauthorized, "", true, 0},
		{http.StatusForbidden, "", true, 0},
		{http.StatusNotFound, "", true, 0},
		{http.StatusConflict, "", true, 0},
		{http.StatusRequestEntityTooLarge, "", true, 0},
		{http.StatusUnprocessableEntity, "", true, 0},
		{http.StatusRequestTimeout, "", false, 0},
		{http.StatusTooManyRequests, "2", false, 2 * time.Second},
		{http.StatusInternalServerError, "", false, 0},
		{http.StatusBadGateway, "", false, 0},
		{http.StatusServiceUnavailable, "3", false, 3 * time.Second},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "status body")
			}))
			t.Cleanup(srv.Close)

			c := newIngestClient(srv.URL, "token", nil)
			err := c.post(context.Background(), ingestRequest{
				Path:          "/api/events/errors",
				ContentType:   "application/json",
				ContentLength: 2,
				Body:          func() io.ReadCloser { return io.NopCloser(strings.NewReader("{}")) },
				Timeout:       2 * time.Second,
				Op:            "test op",
			})
			var dErr *deliveryError
			if !errors.As(err, &dErr) {
				t.Fatalf("err=%v want *deliveryError", err)
			}
			if dErr.statusCode != tc.status || dErr.permanent != tc.wantPermanent {
				t.Fatalf(
					"status=%d permanent=%v want %d/%v",
					dErr.statusCode,
					dErr.permanent,
					tc.status,
					tc.wantPermanent,
				)
			}
			if tc.wantMinDelay > 0 && dErr.retryAfter != tc.wantMinDelay {
				t.Fatalf("retryAfter=%s want %s", dErr.retryAfter, tc.wantMinDelay)
			}
		})
	}
}

func TestIngestClientBoundsErrorBodyAndDrainsConnections(t *testing.T) {
	t.Parallel()
	hugeBody := strings.Repeat("x", 256<<10)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/huge-error" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, hugeBody)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("o", 128<<10))
	}))
	t.Cleanup(server.Close)

	client := newIngestClient(server.URL, "token", nil)
	err := client.post(context.Background(), ingestRequest{
		Path:          "/huge-error",
		ContentLength: 0,
		Timeout:       2 * time.Second,
		Op:            "huge",
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if len(err.Error()) > ingestMaxErrorBodyBytes+64 {
		t.Fatalf("error message not bounded: len=%d", len(err.Error()))
	}
	if err := client.post(context.Background(), ingestRequest{
		Path:          "/huge-ok",
		ContentLength: 0,
		Timeout:       2 * time.Second,
		Op:            "ok",
	}); err != nil {
		t.Fatalf("2xx with large body failed: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d want 1", requests.Load())
	}
}

func TestIngestClientStripsAuthorizationOnCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var crossOriginAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossOriginAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	client := newIngestClient(source.URL, "lo_must_not_leak", nil)
	if err := client.post(context.Background(), ingestRequest{
		Path:          "/api/events/errors",
		ContentType:   "application/json",
		ContentLength: 2,
		Body:          func() io.ReadCloser { return io.NopCloser(strings.NewReader("{}")) },
		Timeout:       2 * time.Second,
		Op:            "redirect test",
	}); err != nil {
		t.Fatalf("redirect request failed: %v", err)
	}
	if crossOriginAuth != "" {
		t.Fatalf("Authorization header leaked across origins: %q", crossOriginAuth)
	}
}
