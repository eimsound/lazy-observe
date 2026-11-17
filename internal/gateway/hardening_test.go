package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
)

type panickingTokens struct{}

func (panickingTokens) AuthenticateProjectAPIToken(context.Context, string) (meta.ProjectInfo, error) {
	panic("injected handler panic")
}

func (panickingTokens) Revision() uint64 { return 0 }

// A handler panic must become a 500 for that request instead of terminating
// the whole gateway process.
func TestGatewayRecoversHandlerPanic(t *testing.T) {
	app := newTestGateway(Config{Tokens: panickingTokens{}}).App()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want=500", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/livez", nil))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway unusable after panic: status=%v err=%v", resp.StatusCode, err)
	}
}

// A client that never finishes its request headers (slowloris) must not hold
// a connection open forever.
func TestGatewayReadTimeoutClosesSlowClient(t *testing.T) {
	gw := newTestGateway(Config{
		Tokens:      &fakeTokens{project: meta.ProjectInfo{ID: 7, Slug: "alpha"}},
		ReadTimeout: 200 * time.Millisecond,
		IdleTimeout: 200 * time.Millisecond,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gw.App().Listener(ln) }()
	t.Cleanup(func() { _ = gw.App().Shutdown() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /livez HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(conn)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("gateway kept an incomplete request open past its read timeout")
	}
}

func TestGatewayDefaultTimeoutsLeaveStreamingWritesUnbounded(t *testing.T) {
	cfg := newTestGateway(Config{Tokens: &fakeTokens{}}).App().Config()
	if cfg.ReadTimeout != defaultReadTimeout || cfg.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("read/idle timeout=%s/%s", cfg.ReadTimeout, cfg.IdleTimeout)
	}
	// Log tail is a long-lived streaming response; fasthttp's WriteTimeout would
	// cut it off, so it must stay disabled.
	if cfg.WriteTimeout != 0 {
		t.Fatalf("write timeout=%s want 0", cfg.WriteTimeout)
	}
}
