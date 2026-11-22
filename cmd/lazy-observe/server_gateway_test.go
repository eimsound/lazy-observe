package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/otlpgrpc"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logsdata "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
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
	override := `[
		{"model_pattern":"custom-enterprise-model","input_per_1k":2.5,"output_per_1k":5.0,"effective_from":"2026-07-01","currency":"USD"},
		{"model_pattern":"internal-vllm-llama","provider":"vllm","self_hosted":true,"input_per_1k":0,"output_per_1k":0}
	]`
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
	if price, ok := table.LookupWithProvider("internal-vllm-llama", "vllm"); !ok || !price.SelfHosted {
		t.Fatalf("self-hosted lookup=%#v ok=%v", price, ok)
	}
}

func TestLoadServerEssentialsValidatesPricingFileAtStartup(t *testing.T) {
	t.Run("missing pricing file", func(t *testing.T) {
		cfg := appconfig.Defaults()
		cfg.DataDir = t.TempDir()
		cfg.AI.PricingFile = filepath.Join(t.TempDir(), "does-not-exist.json")
		_, err := loadServerEssentials(cfg)
		if err == nil || !strings.Contains(err.Error(), "configure LLM pricing") {
			t.Fatalf("loadServerEssentials err=%v want configure LLM pricing", err)
		}
	})

	invalidPayloads := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "invalid JSON syntax",
			content: `[{"model_pattern":"gpt-4o"`,
			wantErr: "configure LLM pricing",
		},
		{
			name:    "unknown field typo",
			content: `[{"model_pattern":"gpt-4o","input_per_1000":1,"output_per_1k":2,"currency":"USD"}]`,
			wantErr: "unknown field",
		},
		{
			name:    "invalid effective_from date",
			content: `[{"model_pattern":"gpt-4o","input_per_1k":1,"output_per_1k":2,"effective_from":"not-a-date","currency":"USD"}]`,
			wantErr: "effective_from",
		},
		{
			name:    "negative rate",
			content: `[{"model_pattern":"gpt-4o","input_per_1k":-1,"output_per_1k":2,"currency":"USD"}]`,
			wantErr: "input_per_1k",
		},
		{
			name:    "unsupported currency",
			content: `[{"model_pattern":"gpt-4o","input_per_1k":1,"output_per_1k":2,"currency":"CNY"}]`,
			wantErr: "currency must be USD",
		},
	}
	for _, tc := range invalidPayloads {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "pricing.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := appconfig.Defaults()
			cfg.DataDir = dir
			cfg.AI.PricingFile = path
			_, err := loadServerEssentials(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("loadServerEssentials(%s) err=%v want substring %q", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestCollectPlatformEndpointsIncludesOffsetLoopbackAndExternalStorage(t *testing.T) {
	cfg := appconfig.Defaults()
	cfg.Listen = "0.0.0.0:4100"
	cfg.Process.ComponentPortOffset = 100
	cfg.Storage.Metrics.QueryURL = "http://vmselect.internal:8481"
	cfg.Storage.Metrics.IngestURL = "https://vminsert.internal:443"
	cfg.Storage.Logs.URL = "http://vlogs.internal:9428"
	cfg.Storage.Logs.QueryURL = "http://vlselect.internal:9471"
	cfg.Storage.Logs.IngestURL = "http://vlinsert.internal:9481"
	cfg.Storage.Traces.URL = "http://vtraces.internal:10428"
	cfg.Storage.Traces.QueryURL = "http://vtselect.internal:10471"
	cfg.Storage.Traces.IngestURL = "http://vtinsert.internal:10481"

	loopback := &internalLoopback{baseURL: "http://127.0.0.1:49876"}
	ports, hostPorts := collectPlatformEndpoints(cfg, loopback, nil)

	portSet := make(map[int]bool, len(ports))
	for _, p := range ports {
		portSet[p] = true
	}
	for _, want := range []int{
		3100, 4100, 18428, 18528, 19528, 20528, 21528, 22528, 49876, 8481,
		9428, 9471, 9481, 10428, 10471, 10481,
	} {
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

func TestCollectPlatformEndpointsIncludesOTLPGRPCPortWhenEnabled(t *testing.T) {
	cfg := appconfig.Defaults()
	cfg.OTLPGRPC.Enabled = true
	cfg.OTLPGRPC.Listen = "0.0.0.0:4317"

	ports, _ := collectPlatformEndpoints(cfg, nil, nil)
	if !slices.Contains(ports, 4317) {
		t.Fatalf("platform ports=%v missing enabled OTLP/gRPC port 4317", ports)
	}

	policy, err := serverOutboundPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := policy.BlockedPorts[4317]; !ok {
		t.Fatalf("outbound policy BlockedPorts=%v missing 4317", policy.BlockedPorts)
	}
}

type stubGRPCAuth struct{}

func (stubGRPCAuth) AuthenticateProjectToken(context.Context, string) (meta.ProjectInfo, error) {
	return meta.ProjectInfo{ID: 1, Slug: "default"}, nil
}

type signalingGatewayLifecycle struct {
	recordingGatewayLifecycle
	drained chan struct{}
}

func (g *signalingGatewayLifecycle) Drain(ctx context.Context) error {
	err := g.recordingGatewayLifecycle.Drain(ctx)
	close(g.drained)
	return err
}

func TestCompositeGatewayLifecycleDrainsOTLPGRPCAndHTTPConcurrently(t *testing.T) {
	upstreamStarted := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamStarted <- struct{}{}
		select {
		case <-releaseUpstream:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	grpcSrv, err := otlpgrpc.NewServer(otlpgrpc.Config{
		Auth:       stubGRPCAuth{},
		Storage:    backend,
		HTTPClient: upstream.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- grpcSrv.Serve(ln) }()

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Start an in-flight OTLP/gRPC RPC that blocks on upstream so GracefulStop stalls.
	rpcDone := make(chan error, 1)
	go func() {
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer lo_test"))
		_, callErr := logspb.NewLogsServiceClient(conn).Export(ctx, &logspb.ExportLogsServiceRequest{
			ResourceLogs: []*logsdata.ResourceLogs{{
				ScopeLogs: []*logsdata.ScopeLogs{{
					LogRecords: []*logsdata.LogRecord{{
						Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "in-flight"}},
					}},
				}},
			}},
		})
		rpcDone <- callErr
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight gRPC export did not reach upstream")
	}

	httpGW := &signalingGatewayLifecycle{drained: make(chan struct{})}
	composite := &compositeGatewayLifecycle{
		httpGateway: httpGW,
		otlpGRPC:    grpcSrv,
		drainLimit:  2 * time.Second,
	}
	composite.BeginDrain()
	if httpGW.beginDrains.Load() != 1 {
		t.Fatalf("beginDrains=%d want=1", httpGW.beginDrains.Load())
	}

	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		drainDone <- composite.Drain(ctx)
	}()

	// HTTP Drain must start and complete concurrently without waiting for the slow gRPC RPC.
	select {
	case <-httpGW.drained:
	case <-time.After(500 * time.Millisecond):
		close(releaseUpstream)
		t.Fatal("httpGateway.Drain was blocked by slow OTLP/gRPC Shutdown")
	}

	close(releaseUpstream)
	if err := <-rpcDone; err != nil {
		t.Fatalf("in-flight gRPC RPC failed during concurrent drain: %v", err)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("composite Drain err=%v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("grpcSrv.Serve err=%v", err)
	}
	if httpGW.drains.Load() != 1 {
		t.Fatalf("http drains=%d want=1", httpGW.drains.Load())
	}
}

func TestOpenOTLPGRPCListenerTrimsWhitespace(t *testing.T) {
	cfg := appconfig.Defaults()
	cfg.OTLPGRPC.Enabled = true
	cfg.OTLPGRPC.Listen = "  127.0.0.1:0  "
	managed, err := openOTLPGRPCListener(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("openOTLPGRPCListener with surrounding spaces failed: %v", err)
	}
	defer managed.Close()
	if managed.Port() <= 0 {
		t.Fatalf("managed.Port()=%d want > 0", managed.Port())
	}
}

func TestErrorInboxWorkerCountRespectsClusteredMode(t *testing.T) {
	if got := errorInboxWorkerCount(false); got != 1 {
		t.Fatalf("non-clustered errorInboxWorkerCount=%d want 1", got)
	}
	if got := errorInboxWorkerCount(true); got != errortrack.DefaultClusteredWorkers || got < 2 || got > 4 {
		t.Fatalf("clustered errorInboxWorkerCount=%d want %d (in 2..4)", got, errortrack.DefaultClusteredWorkers)
	}
}
