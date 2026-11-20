package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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

	err := waitForServerExit(context.Background(), nil, appconfig.Defaults(), monitoring, supervisor, running)

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

func TestLoadLLMPricingUsesConfiguredOverrideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	override := `[{"model_pattern":"custom-enterprise-model","input_per_1k":2.5,"output_per_1k":5.0,"currency":"USD"}]`
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appconfig.Defaults()
	cfg.AI.PricingFile = path
	table, err := loadLLMPricing(cfg)
	if err != nil {
		t.Fatalf("loadLLMPricing error=%v", err)
	}
	cost, unpriced := table.Cost("custom-enterprise-model", 1000, 1000)
	if unpriced || cost != 7.5 {
		t.Fatalf("cost=%v unpriced=%v want 7.5,false", cost, unpriced)
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

type recordingGatewayLifecycle struct {
	beginDrains atomic.Int64
	drains      atomic.Int64
	drainHasDL  atomic.Bool
}

func (g *recordingGatewayLifecycle) BeginDrain() { g.beginDrains.Add(1) }

func (g *recordingGatewayLifecycle) Drain(ctx context.Context) error {
	_, ok := ctx.Deadline()
	g.drainHasDL.Store(ok)
	g.drains.Add(1)
	return nil
}

func (g *recordingGatewayLifecycle) Shutdown() error { return nil }

func TestPreStopSignalsDrainBeforeDelayAndTermAfterDelay(t *testing.T) {
	var events []string
	var slept time.Duration
	err := runPreStopWith(
		42,
		1,
		7*time.Second,
		func(pid int, sig syscall.Signal) error {
			if pid != 1 {
				t.Fatalf("unexpected target pid=%d", pid)
			}
			switch sig {
			case syscall.SIGUSR1:
				events = append(events, "SIGUSR1")
			case syscall.SIGTERM:
				events = append(events, "SIGTERM")
			default:
				t.Fatalf("unexpected signal=%v", sig)
			}
			return nil
		},
		func(d time.Duration) {
			slept = d
			events = append(events, "SLEEP")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"SIGUSR1", "SLEEP", "SIGTERM"}) || slept != 7*time.Second {
		t.Fatalf("pre-stop sequence=%v slept=%s want [SIGUSR1 SLEEP SIGTERM] 7s", events, slept)
	}
}

func TestWaitForServerExitHandlesPreStopSignalBeforeTerm(t *testing.T) {
	gw := &recordingGatewayLifecycle{}
	supervisor := &fatalSupervisor{fatal: make(chan error, 1)}
	running := &runningServerGateway{gateway: gw, errors: make(chan error)}
	monitoring := &selfMonitoring{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	drainSig := make(chan os.Signal, 1)

	done := make(chan error, 1)
	go func() {
		done <- waitForServerExit(ctx, drainSig, appconfig.Defaults(), monitoring, supervisor, running)
	}()

	// SIGUSR1 marks gateway unready without stopping the listener or exiting.
	drainSig <- syscall.SIGUSR1
	deadline := time.After(time.Second)
	for gw.beginDrains.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("BeginDrain was not called on SIGUSR1")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if gw.drains.Load() != 0 || supervisor.shutdowns.Load() != 0 {
		t.Fatalf(
			"server stopped prematurely on SIGUSR1: drains=%d shutdowns=%d",
			gw.drains.Load(),
			supervisor.shutdowns.Load(),
		)
	}

	// SIGTERM (ctx cancel) then drains gateway with deadline and stops supervisor.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("waitForServerExit err=%v", err)
	}
	if gw.drains.Load() != 1 || !gw.drainHasDL.Load() || supervisor.shutdowns.Load() != 1 {
		t.Fatalf(
			"unexpected shutdown counts: drains=%d hasDeadline=%v supervisorShutdowns=%d",
			gw.drains.Load(),
			gw.drainHasDL.Load(),
			supervisor.shutdowns.Load(),
		)
	}
}

func TestWaitForServerExitGatewayErrorDrainsWithDeadlineAndStopsSupervisor(t *testing.T) {
	gw := &recordingGatewayLifecycle{}
	supervisor := &fatalSupervisor{fatal: make(chan error, 1)}
	gwErrs := make(chan error, 1)
	gwErrs <- errors.New("listener closed")
	running := &runningServerGateway{gateway: gw, errors: gwErrs}
	monitoring := &selfMonitoring{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := waitForServerExit(context.Background(), nil, appconfig.Defaults(), monitoring, supervisor, running)
	if err == nil || !strings.Contains(err.Error(), "listener closed") {
		t.Fatalf("err=%v", err)
	}
	if gw.drains.Load() != 1 || !gw.drainHasDL.Load() || supervisor.shutdowns.Load() != 1 {
		t.Fatalf(
			"drains=%d hasDeadline=%v shutdowns=%d",
			gw.drains.Load(),
			gw.drainHasDL.Load(),
			supervisor.shutdowns.Load(),
		)
	}
}
