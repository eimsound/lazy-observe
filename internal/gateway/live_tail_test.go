package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestLiveTailFlushesHeadersImmediatelyAndCancelsUpstreamOnClientDisconnect(t *testing.T) {
	upstreamReached := make(chan http.Header, 1)
	upstreamCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReached <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/stream+json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Keep the stream completely idle until canceled.
		<-r.Context().Done()
		upstreamCanceled <- struct{}{}
	}))
	defer upstream.Close()

	gw := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = gw.Drain(ctx)
		<-serveDone
	})

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body := url.Values{"query": {"*"}}.Encode()
	rawReq := "POST /api/v1/logs/tail HTTP/1.1\r\n" +
		"Host: " + ln.Addr().String() + "\r\n" +
		"Authorization: Bearer lou_valid\r\n" +
		"Content-Type: application/x-www-form-urlencoded\r\n" +
		"Accept-Encoding: gzip, br\r\n" +
		"Content-Length: " + strconvItoa(len(body)) + "\r\n\r\n" +
		body
	if _, err := io.WriteString(conn, rawReq); err != nil {
		conn.Close()
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		conn.Close()
		t.Fatalf("did not receive immediate 200 OK headers for idle live tail: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		t.Fatalf("status=%d want=200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		conn.Close()
		t.Fatalf("X-Accel-Buffering=%q want=no", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		conn.Close()
		t.Fatalf("Cache-Control=%q want=no-cache", got)
	}

	select {
	case hdr := <-upstreamReached:
		if enc := hdr.Get("Accept-Encoding"); enc != "" {
			conn.Close()
			t.Fatalf("upstream Accept-Encoding=%q want empty", enc)
		}
	case <-time.After(2 * time.Second):
		conn.Close()
		t.Fatal("upstream was not reached")
	}

	// Disconnect the client while upstream is still idle.
	_ = conn.Close()

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream live tail context was not canceled after client disconnect")
	}
}

func TestLiveTailCancelsUpstreamOnGatewayDrain(t *testing.T) {
	upstreamCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/stream+json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		upstreamCanceled <- struct{}{}
	}))
	defer upstream.Close()

	gw := newTestGateway(Config{
		Tokens: &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:   upstream.URL,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ln) }()

	client := &http.Client{}
	req, err := http.NewRequest(
		http.MethodPost,
		"http://"+ln.Addr().String()+"/api/v1/logs/tail",
		strings.NewReader("query=*"),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer lou_valid")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open live tail: %v", err)
	}
	defer resp.Body.Close()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	if err := gw.Drain(drainCtx); err != nil {
		t.Fatalf("Drain blocked on idle live tail: %v", err)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream live tail context was not canceled on Drain")
	}
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after Drain")
	}
}

func TestLiveTailEnforcesConcurrencyLimitsAndReleasesSlots(t *testing.T) {
	upstreamCanceled := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/stream+json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		upstreamCanceled <- struct{}{}
	}))
	defer upstream.Close()

	gw := newTestGateway(Config{
		Tokens:                &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:                  upstream.URL,
		LiveTailMaxGlobal:     4,
		LiveTailMaxPerUser:    1,
		LiveTailMaxPerProject: 4,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = gw.Drain(ctx)
		<-serveDone
	})

	openTail := func() *http.Response {
		req, reqErr := http.NewRequest(
			http.MethodPost,
			"http://"+ln.Addr().String()+"/api/v1/logs/tail",
			strings.NewReader("query=*"),
		)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		req.Header.Set("Authorization", "Bearer lou_valid")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, doErr := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		return resp
	}

	first := openTail()
	if first.StatusCode != http.StatusOK {
		first.Body.Close()
		t.Fatalf("first tail status=%d want=200", first.StatusCode)
	}

	// Second concurrent stream for the same user must be rejected with 429.
	second := openTail()
	_ = second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		first.Body.Close()
		t.Fatalf("second tail status=%d want=429", second.StatusCode)
	}

	// Closing the first stream must cancel upstream and release the slot.
	_ = first.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("first upstream was not canceled after closing response body")
	}

	deadline := time.Now().Add(2 * time.Second)
	for gw.proxy.liveTail.activeCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if active := gw.proxy.liveTail.activeCount(); active != 0 {
		t.Fatalf("active live tail slots=%d want=0", active)
	}

	third := openTail()
	if third.StatusCode != http.StatusOK {
		third.Body.Close()
		t.Fatalf("reopened tail after release status=%d want=200", third.StatusCode)
	}
	_ = third.Body.Close()
}

func TestLiveTailEnforcesMaxDurationAndHeartbeat(t *testing.T) {
	upstreamCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/stream+json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		upstreamCanceled <- struct{}{}
	}))
	defer upstream.Close()

	gw := newTestGateway(Config{
		Tokens:              &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Logs:                upstream.URL,
		LiveTailMaxDuration: 250 * time.Millisecond,
		LiveTailHeartbeat:   50 * time.Millisecond,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- gw.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = gw.Drain(ctx)
		<-serveDone
	})

	req, err := http.NewRequest(
		http.MethodPost,
		"http://"+ln.Addr().String()+"/api/v1/logs/tail",
		strings.NewReader("query=*"),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer lou_valid")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read live tail body: %v", err)
	}
	if !strings.Contains(string(body), "\n") {
		t.Fatalf("expected heartbeat newline in idle live tail body, got %q", body)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream live tail was not canceled at max duration")
	}
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
