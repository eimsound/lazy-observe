package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/metric"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/ai"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/gateway"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/otlpgrpc"
	"github.com/eimsound/lazy-observe/internal/symbolicate"
)

type runningServerGateway struct {
	gateway serverGatewayLifecycle
	errors  <-chan error
}

type serverGatewayLifecycle interface {
	BeginDrain()
	Drain(context.Context) error
	Shutdown() error
}

type managedComponentSupervisor interface {
	Done() <-chan error
	Shutdown() error
}

type managedPublicListener struct {
	listener net.Listener
}

func openPublicListener(cfg appconfig.Config, logger *slog.Logger) (*managedPublicListener, error) {
	listener, err := gatewayListener(cfg, logger)
	if err != nil {
		return nil, err
	}
	return &managedPublicListener{listener: listener}, nil
}

func (m *managedPublicListener) take() net.Listener {
	if m == nil {
		return nil
	}
	listener := m.listener
	m.listener = nil
	return listener
}

func (m *managedPublicListener) Close() error {
	if m == nil || m.listener == nil {
		return nil
	}
	listener := m.listener
	m.listener = nil
	return listener.Close()
}

type managedOTLPGRPCListener struct {
	listener  net.Listener
	tlsConfig *tls.Config
}

func openOTLPGRPCListener(cfg appconfig.Config, logger *slog.Logger) (*managedOTLPGRPCListener, error) {
	if !cfg.OTLPGRPC.Enabled {
		return nil, nil
	}
	listener, err := net.Listen("tcp", strings.TrimSpace(cfg.OTLPGRPC.Listen))
	if err != nil {
		return nil, fmt.Errorf("listen otlp grpc: %w", err)
	}
	tlsConfig, err := gatewayTLSConfig(cfg)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("configure otlp grpc TLS: %w", err)
	}
	if tlsConfig != nil && logger != nil {
		logger.Info("OTLP/gRPC TLS termination enabled", "cert", cfg.Security.TLSCert)
	}
	return &managedOTLPGRPCListener{listener: listener, tlsConfig: tlsConfig}, nil
}

func (m *managedOTLPGRPCListener) take() (net.Listener, *tls.Config) {
	if m == nil {
		return nil, nil
	}
	listener := m.listener
	tlsConfig := m.tlsConfig
	m.listener = nil
	m.tlsConfig = nil
	return listener, tlsConfig
}

func (m *managedOTLPGRPCListener) Port() int {
	if m == nil || m.listener == nil {
		return 0
	}
	if tcpAddr, ok := m.listener.Addr().(*net.TCPAddr); ok && tcpAddr.Port > 0 {
		return tcpAddr.Port
	}
	return explicitListenPort(m.listener.Addr().String())
}

func (m *managedOTLPGRPCListener) Close() error {
	if m == nil || m.listener == nil {
		return nil
	}
	listener := m.listener
	m.listener = nil
	return listener.Close()
}

type compositeGatewayLifecycle struct {
	httpGateway serverGatewayLifecycle
	otlpGRPC    *otlpgrpc.Server
	drainLimit  time.Duration
}

func (c *compositeGatewayLifecycle) BeginDrain() {
	if c.httpGateway != nil {
		c.httpGateway.BeginDrain()
	}
}

func (c *compositeGatewayLifecycle) Drain(ctx context.Context) error {
	var (
		wg      sync.WaitGroup
		grpcErr error
		httpErr error
	)
	if c.otlpGRPC != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grpcErr = c.otlpGRPC.Shutdown(ctx)
		}()
	}
	if c.httpGateway != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			httpErr = c.httpGateway.Drain(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(grpcErr, httpErr)
}

func (c *compositeGatewayLifecycle) Shutdown() error {
	timeout := c.drainLimit
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Drain(ctx)
}

func startGateway(
	cfg appconfig.Config,
	logger *slog.Logger,
	essentials serverEssentials,
	shared *serverSharedState,
	metadata *serverMetadata,
	signals *serverSignalStorage,
	monitoring *selfMonitoring,
	services *serverServices,
	loopback *internalLoopback,
	publicListener *managedPublicListener,
	otlpGRPCListener *managedOTLPGRPCListener,
	cleanups *cleanupStack,
) (*runningServerGateway, error) {
	readiness := serverReadiness(cfg, shared, metadata, signals)
	var sessionAdapter account.Sessions
	if !cfg.Redis.Enabled {
		sessionAdapter = account.NewMetadataSessions(metadata.store)
	} else {
		sessionAdapter = account.NewStateSessions(shared.sessions)
	}
	accounts := account.New(metadata.store, sessionAdapter, account.Options{
		Limits: shared.limits, LoginGate: shared.loginGate,
	})
	var meterProvider metric.MeterProvider
	if monitoring.client != nil {
		meterProvider = monitoring.client.MeterProvider()
	}
	gw := gateway.New(
		gateway.Config{
			Tokens:                   metadata.store,
			UploadTokens:             metadata.store,
			Store:                    metadata.store,
			Account:                  accounts,
			Management:               management.New(metadata.store).WithSessions(sessionAdapter),
			Health:                   combinedHealth{signals.supervisor, signals.backend},
			Readiness:                readiness,
			Storage:                  signals.backend,
			Alerts:                   services.alerts,
			Replays:                  metadata.store,
			Issues:                   metadata.store,
			SourceMaps:               metadata.store,
			Profiles:                 metadata.store,
			Notifications:            metadata.store,
			Blobs:                    metadata.blobs,
			Scrapes:                  services.scrapes,
			ScrapeUnavailableReason:  services.scrapeUnavailableReason,
			MCPBaseURL:               loopback.baseURL,
			AISettingsStore:          metadata.store,
			AISecretBox:              essentials.secretBox,
			AIBuildClient:            ai.New,
			AITimeout:                60 * time.Second,
			OutboundPolicy:           essentials.outboundPolicy,
			LLMPricing:               essentials.llmPricing,
			DisableWebUI:             !cfg.ServeWebUI,
			Instrumentation:          monitoring.instrumentation,
			SelfMonitoringToken:      metadata.selfToken,
			Limits:                   shared.limits,
			AuthInvalidator:          shared.authInvalidator,
			RequireCSRF:              cfg.Security.CSRF,
			SecureSessionCookie:      effectiveSecureSessionCookie(cfg),
			TrustedProxies:           cfg.Security.TrustedProxies,
			InitialAdminPasswordFile: gatewayInitialAdminPasswordFile(cfg),
			Logger:                   logger,
			MeterProvider:            meterProvider,
		},
	)
	inboxOptions := errortrack.InboxOptions{
		Logger: logger,
		Capacity: errortrack.InboxCapacity{
			MaxItems: cfg.ErrorInbox.MaxItems, MaxBytes: cfg.ErrorInbox.MaxBytes,
			ReadinessItems: cfg.ErrorInbox.ReadinessItems, ReadinessBytes: cfg.ErrorInbox.ReadinessBytes,
		},
		Symbolicator: symbolicate.New(metadata.blobs, 128), OnChange: gw.HandleIssueChange,
		OnEvent: gw.HandleErrorEvent,
	}
	if monitoring.client != nil {
		inboxOptions.MeterProvider = monitoring.client.MeterProvider()
	}
	errorTracker, err := errortrack.NewInbox(
		metadata.store,
		errortrack.NewVictoriaLogsWriter(signals.backend, http.DefaultClient),
		inboxOptions,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize durable error tracking: %w", err)
	}
	if err := gw.AttachErrorTracker(errorTracker); err != nil {
		_ = errorTracker.Close(context.Background())
		return nil, err
	}

	listener := publicListener.take()
	if listener == nil {
		listener, err = gatewayListener(cfg, logger)
		if err != nil {
			_ = errorTracker.Close(context.Background())
			return nil, err
		}
	}
	var grpcServer *otlpgrpc.Server
	var grpcAddr string
	var grpcTLS bool
	grpcListener, grpcTLSConfig := otlpGRPCListener.take()
	if grpcListener != nil {
		grpcServer, err = otlpgrpc.NewServer(otlpgrpc.Config{
			Auth:           gw.IngestAuthenticator(),
			Storage:        signals.backend,
			TLSConfig:      grpcTLSConfig,
			MaxRecvMsgSize: cfg.OTLPGRPC.MaxRecvBytes,
			DrainTimeout:   cfg.Process.DrainTimeout.Value(),
			Logger:         logger,
		})
		if err != nil {
			_ = grpcListener.Close()
			_ = listener.Close()
			_ = errorTracker.Close(context.Background())
			return nil, fmt.Errorf("initialize OTLP/gRPC server: %w", err)
		}
		grpcAddr = grpcListener.Addr().String()
		grpcTLS = grpcTLSConfig != nil
		if cleanups != nil {
			drainTimeout := cfg.Process.DrainTimeout.Value()
			cleanups.Push("otlp-grpc-server", func() error {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
				defer cancel()
				return grpcServer.Shutdown(shutdownCtx)
			})
		}
	}

	serverErr := make(chan error, 3)
	go func() { serverErr <- gw.Serve(listener) }()
	if internal := loopback.take(); internal != nil {
		go func() { serverErr <- gw.Serve(internal) }()
	}
	if grpcServer != nil {
		go func() {
			if serveErr := grpcServer.Serve(grpcListener); serveErr != nil {
				serverErr <- fmt.Errorf("otlp grpc server: %w", serveErr)
			}
		}()
	}
	gw.MarkReady()
	logGatewayReady(cfg, logger, grpcAddr, grpcTLS)
	lifecycle := serverGatewayLifecycle(gw)
	if grpcServer != nil {
		lifecycle = &compositeGatewayLifecycle{
			httpGateway: gw,
			otlpGRPC:    grpcServer,
			drainLimit:  cfg.Process.DrainTimeout.Value(),
		}
	}
	return &runningServerGateway{gateway: lifecycle, errors: serverErr}, nil
}

func effectiveSecureSessionCookie(cfg appconfig.Config) bool {
	return cfg.Security.SessionSecureCookie || (cfg.Security.TLSCert != "" && cfg.Security.TLSKey != "")
}

func serverReadiness(
	cfg appconfig.Config,
	shared *serverSharedState,
	metadata *serverMetadata,
	signals *serverSignalStorage,
) combinedHealth {
	readiness := combinedHealth{signals.supervisor, signals.backend, metadata.store}
	if shared.redisClient != nil {
		readiness = append(readiness, healthFunc(func(ctx context.Context) map[string]error {
			return map[string]error{"redis": shared.redisClient.Ping(ctx).Err()}
		}))
	}
	if cfg.Blob.Backend == "s3" {
		readiness = append(readiness, healthFunc(func(ctx context.Context) map[string]error {
			_, err := metadata.blobs.List(ctx, "readiness/")
			return map[string]error{"s3": err}
		}))
	}
	return readiness
}

func gatewayTLSConfig(cfg appconfig.Config) (*tls.Config, error) {
	if cfg.Security.TLSCert == "" && cfg.Security.TLSKey == "" {
		return nil, nil
	}
	if cfg.Security.TLSCert == "" || cfg.Security.TLSKey == "" {
		return nil, errors.New("tls-cert and tls-key must be set together")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.Security.TLSCert, cfg.Security.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

func gatewayListener(cfg appconfig.Config, logger *slog.Logger) (net.Listener, error) {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen gateway: %w", err)
	}
	tlsConfig, err := gatewayTLSConfig(cfg)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if tlsConfig == nil {
		return listener, nil
	}
	logger.Info("TLS termination enabled", "cert", cfg.Security.TLSCert)
	return tls.NewListener(listener, tlsConfig), nil
}

func logGatewayReady(cfg appconfig.Config, logger *slog.Logger, otlpGRPCAddr string, otlpGRPCTLS bool) {
	logger.Info(
		"gateway listening",
		"address",
		cfg.Listen,
		"topology",
		cfg.EffectiveTopology(),
		"standalone",
		cfg.IsStandalone(),
		"metrics_kind",
		cfg.Storage.Metrics.Kind,
	)
	if otlpGRPCAddr != "" {
		logger.Info("otlp grpc listening", "address", otlpGRPCAddr, "tls", otlpGRPCTLS)
	}
	if cfg.ServeWebUI {
		logger.Info("web UI ready", "url", "http://"+loopbackAddress(cfg.Listen))
	} else {
		logger.Info("web UI disabled")
	}
}

func waitForServerExit(
	ctx context.Context,
	drainSig <-chan os.Signal,
	cfg appconfig.Config,
	monitoring *selfMonitoring,
	supervisor managedComponentSupervisor,
	runningGateway *runningServerGateway,
) error {
	alreadyDraining := false
	for {
		select {
		case <-drainSig:
			if !alreadyDraining {
				monitoring.logger.Info("pre-stop drain requested; marking readiness unavailable")
				runningGateway.gateway.BeginDrain()
				alreadyDraining = true
			}
		case <-ctx.Done():
			return drainServer(cfg, monitoring, supervisor, runningGateway.gateway, alreadyDraining)
		case fatalErr := <-supervisor.Done():
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.Process.DrainTimeout.Value())
			monitoring.Shutdown()
			_ = runningGateway.gateway.Drain(drainCtx)
			cancelDrain()
			shutdownErr := supervisor.Shutdown()
			if fatalErr == nil {
				fatalErr = errors.New("managed component supervisor stopped without an error")
			}
			return errors.Join(fmt.Errorf("managed component supervisor failed: %w", fatalErr), shutdownErr)
		case err := <-runningGateway.errors:
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.Process.DrainTimeout.Value())
			monitoring.Shutdown()
			_ = runningGateway.gateway.Drain(drainCtx)
			cancelDrain()
			shutdownErr := supervisor.Shutdown()
			if err == nil {
				err = errors.New("gateway stopped unexpectedly")
			}
			return errors.Join(err, shutdownErr)
		}
	}
}

func drainServer(
	cfg appconfig.Config,
	monitoring *selfMonitoring,
	supervisor managedComponentSupervisor,
	gw serverGatewayLifecycle,
	alreadyDraining bool,
) error {
	monitoring.logger.Info("shutdown requested")
	gw.BeginDrain()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.Process.DrainTimeout.Value())
	if !alreadyDraining {
		// Keep the listener available briefly when SIGTERM arrived directly
		// (without a preceding pre-stop SIGUSR1 window) so readiness polling can
		// observe 503 before connection acceptance stops.
		time.Sleep(250 * time.Millisecond)
	}
	// Flush while the in-process OTLP endpoint and storage are still alive.
	monitoring.Shutdown()
	if err := gw.Drain(drainCtx); err != nil {
		monitoring.logger.Error("gateway shutdown failed", "error", err)
	}
	cancelDrain()
	return supervisor.Shutdown()
}

type healthChecker interface {
	Health(context.Context) map[string]error
}

type healthFunc func(context.Context) map[string]error

func (f healthFunc) Health(ctx context.Context) map[string]error { return f(ctx) }

type combinedHealth []healthChecker

func (checkers combinedHealth) Health(ctx context.Context) map[string]error {
	result := make(map[string]error)
	for _, checker := range checkers {
		for name, err := range checker.Health(ctx) {
			existing, exists := result[name]
			if exists && existing != nil {
				continue
			}
			if !exists || err != nil {
				result[name] = err
			}
		}
	}
	return result
}

// internalLoopback is where the gateway's own in-process clients (MCP tool
// calls and the self-monitoring SDK) reach the gateway again. Without native
// TLS that is the public listener on loopback. With native TLS those clients
// cannot speak plaintext to the TLS listener, and the certificate is usually
// not valid for 127.0.0.1, so a second plaintext listener bound to 127.0.0.1
// serves the same app; it is never reachable from outside the host.
type internalLoopback struct {
	baseURL  string
	listener net.Listener
}

func openInternalLoopback(cfg appconfig.Config) (*internalLoopback, error) {
	if cfg.Security.TLSCert == "" && cfg.Security.TLSKey == "" {
		return &internalLoopback{baseURL: "http://" + loopbackAddress(cfg.Listen)}, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen internal loopback: %w", err)
	}
	return &internalLoopback{baseURL: "http://" + listener.Addr().String(), listener: listener}, nil
}

// take hands the listener to the gateway server, which then owns closing it.
func (l *internalLoopback) take() net.Listener {
	listener := l.listener
	l.listener = nil
	return listener
}

// Close releases a listener that was never handed to the gateway.
func (l *internalLoopback) Close() {
	if l.listener != nil {
		_ = l.listener.Close()
	}
}

func loopbackAddress(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:3100"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func listenPort(listen string) int {
	if port := explicitListenPort(listen); port > 0 {
		return port
	}
	return 3100
}

func explicitListenPort(listen string) int {
	_, raw, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return 0
	}
	port, err := net.LookupPort("tcp", raw)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

func embeddedURL(address string, offset int) string {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return "http://" + address
	}
	port, err := net.LookupPort("tcp", rawPort)
	if err != nil {
		return "http://" + address
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprintf("%d", port+offset))
}
