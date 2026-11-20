package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

type staticHealth map[string]error

func (h staticHealth) Health(context.Context) map[string]error { return h }

func TestProbeLifecycle(t *testing.T) {
	g := newTestGateway(Config{Readiness: staticHealth{"metadata": nil}})
	assertStatus := func(path string, want int) {
		t.Helper()
		resp, err := g.App().Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s status=%d want=%d", path, resp.StatusCode, want)
		}
	}
	assertStatus("/livez", http.StatusOK)
	assertStatus("/readyz", http.StatusServiceUnavailable)
	g.MarkReady()
	assertStatus("/readyz", http.StatusOK)
	g.BeginDrain()
	assertStatus("/readyz", http.StatusServiceUnavailable)
	assertStatus("/livez", http.StatusOK)
}

func TestReadinessReportsDependencyFailure(t *testing.T) {
	g := newTestGateway(Config{Readiness: staticHealth{"redis": errors.New("unreachable")}})
	g.MarkReady()
	resp, err := g.App().Test(httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestManagedComponentRestartProbeContract(t *testing.T) {
	restarting := staticHealth{
		"logs":    errors.New("component restarting"),
		"metrics": nil,
		"traces":  nil,
	}
	g := newTestGateway(Config{Health: restarting, Readiness: restarting})
	g.MarkReady()
	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/livez", want: http.StatusOK},
		{path: "/healthz", want: http.StatusServiceUnavailable},
		{path: "/readyz", want: http.StatusServiceUnavailable},
	} {
		resp, err := g.App().Test(httptest.NewRequest(http.MethodGet, test.path, nil))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode != test.want {
			t.Fatalf("%s status=%d want=%d body=%s", test.path, resp.StatusCode, test.want, body)
		}
		if test.path != "/livez" &&
			(!bytes.Contains(body, []byte(`"logs":"component restarting"`)) ||
				bytes.Contains(body, []byte("127.0.0.1"))) {
			t.Fatalf("%s unsafe or incomplete body=%s", test.path, body)
		}
	}
}

func TestDrainWaitsForInflightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	g := newTestGateway(Config{Instrumentation: func(c fiber.Ctx) error {
		if c.Path() == "/healthz" {
			close(started)
			<-release
		}
		return c.Next()
	}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- g.Serve(listener) }()

	responseDone := make(chan error, 1)
	go func() {
		resp, requestErr := http.Get("http://" + listener.Addr().String() + "/healthz")
		if requestErr == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				requestErr = errors.New("in-flight request did not complete successfully")
			}
		}
		responseDone <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach handler")
	}

	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		drainDone <- g.Drain(ctx)
	}()
	select {
	case err := <-drainDone:
		t.Fatalf("drain returned before in-flight request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("listener did not stop")
	}
}
