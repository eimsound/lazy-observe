package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
)

type fatalSupervisor struct {
	fatal     chan error
	shutdowns atomic.Int64
}

type inertGatewayLifecycle struct{}

func (inertGatewayLifecycle) BeginDrain()                 {}
func (inertGatewayLifecycle) Drain(context.Context) error { return nil }
func (inertGatewayLifecycle) Shutdown() error             { return nil }

func (s *fatalSupervisor) Done() <-chan error { return s.fatal }

func (s *fatalSupervisor) Shutdown() error {
	s.shutdowns.Add(1)
	return nil
}

func TestWaitForServerExitReturnsOnlySupervisorFatalErrors(t *testing.T) {
	supervisor := &fatalSupervisor{fatal: make(chan error, 1)}
	supervisor.fatal <- errors.New("restart loop invariant failed")
	running := &runningServerGateway{
		gateway: inertGatewayLifecycle{},
		errors:  make(chan error),
	}
	monitoring := &selfMonitoring{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := waitForServerExit(context.Background(), appconfig.Defaults(), monitoring, supervisor, running)

	if err == nil || !strings.Contains(err.Error(), "managed component supervisor failed") ||
		!strings.Contains(err.Error(), "restart loop invariant failed") {
		t.Fatalf("wait error=%v", err)
	}
	if supervisor.shutdowns.Load() != 1 {
		t.Fatalf("supervisor shutdowns=%d want=1", supervisor.shutdowns.Load())
	}
}

func TestCombinedHealthPreservesSupervisorRestartState(t *testing.T) {
	restarting := errors.New("component restarting")
	combined := combinedHealth{
		healthFunc(func(context.Context) map[string]error {
			return map[string]error{"logs": restarting, "metrics": nil}
		}),
		healthFunc(func(context.Context) map[string]error {
			return map[string]error{
				"logs":    errors.New("Get http://127.0.0.1:19428/health: connection refused"),
				"metrics": errors.New("metrics backend unavailable"),
			}
		}),
	}

	health := combined.Health(context.Background())

	if !errors.Is(health["logs"], restarting) || health["metrics"] == nil ||
		health["metrics"].Error() != "metrics backend unavailable" {
		t.Fatalf("combined health=%v", health)
	}
}

func TestEffectiveSecureSessionCookieEnablesWithNativeTLS(t *testing.T) {
	cfg := appconfig.Defaults()
	if effectiveSecureSessionCookie(cfg) {
		t.Fatal("plain HTTP without SessionSecureCookie should keep SecureSessionCookie false")
	}
	cfg.Security.SessionSecureCookie = true
	if !effectiveSecureSessionCookie(cfg) {
		t.Fatal("explicit SessionSecureCookie should be true")
	}
	cfg.Security.SessionSecureCookie = false
	cfg.Security.TLSCert = "/tmp/cert.pem"
	cfg.Security.TLSKey = "/tmp/key.pem"
	if !effectiveSecureSessionCookie(cfg) {
		t.Fatal("native TLS should automatically enable SecureSessionCookie")
	}
}

func TestCollectPlatformEndpointsIncludesOffsetLoopbackAndExternalStorage(t *testing.T) {
	cfg := appconfig.Defaults()
	cfg.Listen = "0.0.0.0:4100"
	cfg.Process.ComponentPortOffset = 100
	cfg.Storage.Metrics.QueryURL = "http://vmselect.internal:8481"
	cfg.Storage.Metrics.IngestURL = "https://vminsert.internal:443"
	cfg.Storage.Logs.URL = "http://vlogs.internal:9428"
	cfg.Storage.Traces.URL = "http://vtraces.internal:10428"

	loopback := &internalLoopback{baseURL: "http://127.0.0.1:49876"}
	ports, hostPorts := collectPlatformEndpoints(cfg, loopback, nil)

	portSet := make(map[int]bool, len(ports))
	for _, p := range ports {
		portSet[p] = true
	}
	for _, want := range []int{3100, 4100, 18428, 18528, 19528, 20528, 21528, 22528, 49876, 8481, 9428, 10428} {
		if !portSet[want] {
			t.Errorf("platform ports missing %d: got=%v", want, ports)
		}
	}
	if portSet[443] {
		t.Errorf("standard HTTPS port 443 must not be globally blocked across all hosts: ports=%v", ports)
	}
	hpSet := make(map[string]bool, len(hostPorts))
	for _, hp := range hostPorts {
		hpSet[hp] = true
	}
	if !hpSet["vminsert.internal:443"] || !hpSet["127.0.0.1:49876"] {
		t.Errorf("blockedHostPorts=%v missing expected endpoints", hostPorts)
	}
}
