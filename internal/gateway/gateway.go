package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/embed"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/llmcost"
	"github.com/eimsound/lazy-observe/internal/management"
	observemcp "github.com/eimsound/lazy-observe/internal/mcp"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/query"
	"github.com/eimsound/lazy-observe/internal/scrape"
	"github.com/eimsound/lazy-observe/internal/secret"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	"github.com/eimsound/lazy-observe/internal/state"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/eimsound/lazy-observe/internal/webui"
	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"go.opentelemetry.io/otel/metric"
)

const projectLocal = "lazy-observe.project"
const userLocal = "lazy-observe.user"

// maxDecodedRequestBody bounds every request body after Content-Encoding is
// removed. It equals the compressed BodyLimit so compression never raises the
// amount of memory one request may claim.
const maxDecodedRequestBody = 52 << 20

const (
	maxRemoteWriteDecodedBody     = storagepkg.DefaultRemoteWriteMaxDecodedBytes
	defaultRemoteWriteConcurrency = 16
	defaultRemoteWriteRateLimit   = 600
	defaultRemoteWriteRateWindow  = time.Minute
)

const (
	defaultReadTimeout     = 60 * time.Second
	defaultIdleTimeout     = 120 * time.Second
	defaultShutdownTimeout = 15 * time.Second
	defaultProbeTimeout    = 5 * time.Second
)

const (
	defaultProxyMaxIdleConnections        = 256
	defaultProxyMaxIdleConnectionsPerHost = 128
	defaultProxyResponseHeaderTimeout     = 60 * time.Second
	proxyDrainBodyLimitBytes              = 64 << 10
)

type TokenStore interface {
	AuthenticateProjectAPIToken(context.Context, string) (meta.ProjectInfo, error)
	Revision() uint64
}

// UploadTokenStore resolves source map upload tokens (ADR 0018). It is separate
// from TokenStore so ingest authentication can never resolve an upload token.
type UploadTokenStore interface {
	AuthenticateSourceMapUploadToken(context.Context, string) (meta.ProjectInfo, error)
	Revision() uint64
}

type HealthChecker interface {
	Health(context.Context) map[string]error
}

type ManagementStore interface {
	ProjectBySlug(context.Context, string) (meta.ProjectInfo, error)
	GetProject(context.Context, int) (*meta.Project, error)
	RecordAudit(context.Context, meta.AuditRecord) error
	ListAudit(context.Context, meta.AuditListOptions) ([]meta.AuditRecord, error)
}

type IssueStore interface {
	ListIssues(context.Context, int, meta.IssueListOptions) ([]meta.IssueView, error)
	CountIssues(context.Context, int, meta.IssueListOptions) (int, error)
	GetIssue(context.Context, int, int) (meta.IssueView, error)
	IssueIDForErrorEvent(context.Context, int, string) (int, bool, error)
	SetIssueStatus(context.Context, int, int, string, time.Time) (meta.IssueView, error)
	SetIssuesStatus(context.Context, int, []int, string, time.Time) (int, error)
	SaveIssueAnalysis(context.Context, int, int, map[string]any, time.Time) (meta.IssueView, error)
}

// ErrorIntake is the durable error Inbox as the gateway uses it: it accepts
// browser error batches and reports capacity for health and readiness.
type ErrorIntake interface {
	errortrack.Accepter
	errortrack.InboxMonitor
}

// NotificationStore lists and requeues a project's notification history.
type NotificationStore interface {
	ListNotificationOutbox(context.Context, meta.NotificationOutboxListOptions) ([]meta.NotificationOutboxView, error)
	RequeueNotification(context.Context, int64, int) (bool, error)
}

type SourceMapStore interface {
	UpsertSourceMapArtifact(context.Context, int, string, string, int64, string, time.Time) error
	UpsertSourceMapArtifacts(context.Context, int, string, []meta.SourceMapArtifactInput, int, int64, time.Time) error
	SourceMapArtifacts(context.Context, int, string) ([]*meta.SourceMapArtifact, error)
	ListSourceMapReleases(context.Context, int, int) ([]meta.SourceMapRelease, error)
	DeleteSourceMapArtifacts(context.Context, int, string) error
}

type Config struct {
	// InstanceID identifies one running gateway process to authenticated
	// operational probes. Empty generates a fresh 128-bit value in New.
	InstanceID              string
	Tokens                  TokenStore
	UploadTokens            UploadTokenStore
	AuthInvalidator         state.AuthInvalidator
	Store                   ManagementStore
	Account                 account.Behavior
	Limits                  state.Limiter
	Management              *management.Manager
	Health                  HealthChecker
	Readiness               HealthChecker
	Client                  *http.Client
	Metrics                 string
	Logs                    string
	Traces                  string
	Storage                 storagepkg.SignalBackend
	Now                     func() time.Time
	Alerts                  *alert.Service
	Issues                  IssueStore
	ErrorTracker            ErrorIntake
	Notifications           NotificationStore
	Replays                 ReplayStore
	Blobs                   blob.Store
	SourceMaps              SourceMapStore
	Profiles                ProfileStore
	Scrapes                 *scrape.Service
	ScrapeUnavailableReason string
	MCPBaseURL              string
	AIEnabled               bool
	AIProvider              string
	AIModel                 string
	AIClient                ai.Client
	// AISettingsStore switches the gateway into dynamic AI management: the
	// admin API reads/writes the persisted row and hot-swaps the client.
	// When nil, the static AIEnabled/AIProvider/AIModel/AIClient fields above
	// define the runtime state (used by focused tests and embedders).
	AISettingsStore AISettingsStore
	// AISecretBox seals/opens the stored BYO API key (dynamic mode only).
	AISecretBox *secret.Box
	// AIBuildClient constructs the provider client; defaults to ai.New.
	AIBuildClient func(ai.Config) (ai.Client, error)
	// AITimeout bounds provider calls; defaults to 60s when unset.
	AITimeout time.Duration
	// AISettingsRefreshInterval controls HA polling of persisted AI settings;
	// defaults to 5s when unset.
	AISettingsRefreshInterval time.Duration
	// IssueAIAutoDailyBudget sets the default per-project daily automatic
	// Issue AI budget; defaults to 20 when unset.
	IssueAIAutoDailyBudget int
	// OutboundPolicy restricts outbound destinations for BYO AI requests.
	OutboundPolicy      outbound.Policy
	LLMPricing          *llmcost.Table
	WebUI               fs.FS
	DisableWebUI        bool
	Instrumentation     fiber.Handler
	SelfMonitoringToken string
	Logger              *slog.Logger
	// MeterProvider receives the gateway's own operational metrics. Nil uses a
	// no-op provider.
	MeterProvider metric.MeterProvider
	// IngestLimiter enforces unified per-project token-bucket rate limiting
	// across all HTTP project-token ingest routes. Nil disables limiting.
	IngestLimiter *ingestlimit.Limiter
	// SelfMetrics collects low-cardinality Prometheus self-observability metrics
	// served by GET /metrics when MetricsEndpoint.Enabled is true.
	SelfMetrics *selfmetrics.Registry
	// MetricsEndpoint configures GET /metrics exposition and authentication.
	MetricsEndpoint MetricsEndpointConfig
	// RequireCSRF enables the double-submit CSRF token check for
	// session-authenticated mutating requests. Production wiring enables it.
	RequireCSRF bool
	// ReadTimeout bounds reading one request (headers and body); IdleTimeout
	// bounds keep-alive waits; ProxyTimeout bounds unary upstream proxy calls.
	// Zero selects the defaults. There is deliberately no write timeout: log
	// tail is a long-lived streaming response.
	ReadTimeout  time.Duration
	IdleTimeout  time.Duration
	ProxyTimeout time.Duration
	// LiveTailMaxGlobal, LiveTailMaxPerUser, LiveTailMaxPerProject,
	// LiveTailMaxDuration, and LiveTailHeartbeat bound /api/v1/logs/tail
	// streaming connections. Zero selects defaults.
	LiveTailMaxGlobal     int
	LiveTailMaxPerUser    int
	LiveTailMaxPerProject int
	LiveTailMaxDuration   time.Duration
	LiveTailHeartbeat     time.Duration
	// SecureSessionCookie marks the session and CSRF cookies with the Secure
	// attribute. Enable when TLS terminates at the gateway or a reverse proxy.
	SecureSessionCookie bool
	// TrustedProxies lists explicit reverse proxy IPs or CIDRs whose
	// X-Forwarded-For headers are trusted for client IP resolution.
	TrustedProxies []string
	// InitialAdminPasswordFile names the bootstrap-generated first-admin
	// password file. The file is removed once the admin replaces that password.
	// Empty disables removal: cmd supplies it unless security.admin_password_file
	// names that same file (by path or file identity), which is the
	// deployer-owned case.
	InitialAdminPasswordFile string
	// OTLPGzipPassthrough overrides automatic loopback detection for OTLP/HTTP
	// gzip passthrough. Nil enables passthrough only for non-loopback Victoria
	// upstreams; Prometheus metrics backends always decode to plaintext.
	OTLPGzipPassthrough *bool
}

type gatewaySecurity struct {
	auth                     *Authenticator
	uploadAuth               *Authenticator
	authInvalidator          state.AuthInvalidator
	invalidatorWG            sync.WaitGroup
	invalidatorReady         chan struct{}
	account                  account.Behavior
	limits                   state.Limiter
	requireCSRF              bool
	secureSessionCookie      bool
	initialAdminPasswordFile string
	selfMonitoringToken      string
}

type gatewayLifecycle struct {
	health         HealthChecker
	readiness      HealthChecker
	ready          bool
	readyMu        sync.RWMutex
	drainOnce      sync.Once
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

type domainStores struct {
	store                   ManagementStore
	management              *management.Manager
	alerts                  *alert.Service
	issues                  IssueStore
	errorTracker            ErrorIntake
	notifications           NotificationStore
	replays                 ReplayStore
	blobs                   blob.Store
	sourceMaps              SourceMapStore
	sourceMapUploadPinTTL   time.Duration
	profiles                ProfileStore
	scrapes                 *scrape.Service
	scrapeUnavailableReason string
}

type gatewayAI struct {
	ai          *aiManager
	aiAssistant *assistantService
	issueAI     *issueAIService
	llmPricing  *llmcost.Table
}

type Gateway struct {
	app        *fiber.App
	instanceID string
	now        func() time.Time
	logger     *slog.Logger

	gatewaySecurity
	gatewayLifecycle
	domainStores
	gatewayAI

	proxy                *storageProxy
	client               *http.Client
	storage              storagepkg.SignalBackend
	gzipPassthrough      gzipPassthroughPolicy
	metrics              *storagepkg.MetricsClient
	querier              *query.StorageQuerier
	mcpBaseURL           string
	webUI                fs.FS
	instrumentation      fiber.Handler
	meterProvider        metric.MeterProvider
	ingestLimiter        *ingestlimit.Limiter
	selfMetrics          *selfmetrics.Registry
	metricsEndpoint      metricsEndpointState
	remoteWriteSem       chan struct{}
	remoteWriteRateLimit int

	registeredRoutes []registeredRoute
	registeredIndex  map[string]routePolicy
}

func newFiberApp(
	readTimeout, idleTimeout time.Duration,
	trustedProxies []string,
	logger *slog.Logger,
) *fiber.App {
	cfg := fiber.Config{
		// Immutable makes Ctx.Get, Params, Query, Cookies, FormValue, Path,
		// Method, Bind().Body fields and friends return owned copies instead of
		// borrowing fasthttp's request buffers, which are reused by the next
		// request. Borrowed strings once leaked into the project token cache:
		// a cached key silently became a later request's token, so another
		// project's token, or one never issued, authenticated as the cached
		// project. Handlers still copy values they keep beyond a request; this
		// setting is the default-safe backstop for the ones that slip through.
		Immutable: true,
		// Fiber v3 gives every GET route a HEAD twin that runs the whole GET
		// chain, so HEAD on a query route would proxy a HEAD to storage and run
		// the read. v2 had no such routes: HEAD fell through to serveWebUI,
		// which answers 404 for API and probe paths. Keep that contract.
		DisableHeadAutoRegister: true,
		BodyLimit:               maxDecodedRequestBody,
		ReadTimeout:             readTimeout,
		IdleTimeout:             idleTimeout,
		ErrorHandler:            apiErrorHandler(logger),
	}
	if len(trustedProxies) > 0 {
		cfg.TrustProxy = true
		cfg.ProxyHeader = fiber.HeaderXForwardedFor
		cfg.TrustProxyConfig = fiber.TrustProxyConfig{
			Proxies: append([]string(nil), trustedProxies...),
		}
	}
	return fiber.New(cfg)
}

func New(cfg Config) *Gateway {
	if cfg.Account == nil {
		panic("gateway: account behavior is required")
	}
	if cfg.InstanceID == "" {
		value := make([]byte, 16)
		if _, err := rand.Read(value); err != nil {
			panic("gateway: generate instance id: " + err.Error())
		}
		cfg.InstanceID = "loi_" + base64.RawURLEncoding.EncodeToString(value)
	}
	if cfg.Client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// The platform gateway is an HTTP proxy. net/http's default of two idle
		// connections per host forces connection churn under normal OTLP batch
		// concurrency and prevents replicas from scaling independently.
		transport.MaxIdleConns = defaultProxyMaxIdleConnections
		transport.MaxIdleConnsPerHost = defaultProxyMaxIdleConnectionsPerHost
		transport.ResponseHeaderTimeout = defaultProxyResponseHeaderTimeout
		cfg.Client = &http.Client{
			Transport:     transport,
			CheckRedirect: noFollowRedirects,
		}
	} else {
		proxyClient := *cfg.Client
		if proxyClient.CheckRedirect == nil {
			proxyClient.CheckRedirect = noFollowRedirects
		}
		cfg.Client = &proxyClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SelfMetrics == nil {
		cfg.SelfMetrics = selfmetrics.New(selfmetrics.Config{Now: cfg.Now})
	}
	if cfg.Limits == nil {
		cfg.Limits = state.NewMemoryLimiterWithClock(cfg.Now)
	}
	if cfg.Metrics == "" {
		cfg.Metrics = "http://" + embed.MetricsAddr
	}
	if cfg.Logs == "" {
		cfg.Logs = "http://" + embed.LogsAddr
	}
	if cfg.Traces == "" {
		cfg.Traces = "http://" + embed.TracesAddr
	}
	if cfg.Storage == nil {
		// Metrics/Logs/Traces are kept as a compatibility constructor surface for
		// focused gateway tests and embedders. Production startup passes the
		// validated backend explicitly, so every handler still consumes the same
		// storage interface.
		backend, err := storagepkg.New(
			storagepkg.MetricsVM,
			cfg.Metrics,
			cfg.Metrics,
			cfg.Logs,
			cfg.Traces,
			cfg.Client,
		)
		if err != nil {
			panic("gateway: invalid storage endpoints: " + err.Error())
		}
		cfg.Storage = backend
	}
	if cfg.LLMPricing == nil {
		cfg.LLMPricing = llmcost.Default()
	}
	if cfg.DisableWebUI {
		cfg.WebUI = nil
	} else if cfg.WebUI == nil {
		cfg.WebUI, _ = webui.FS()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.ProxyTimeout <= 0 {
		cfg.ProxyTimeout = defaultUnaryProxyTimeout
	}
	if cfg.LiveTailMaxDuration <= 0 {
		cfg.LiveTailMaxDuration = defaultLiveTailMaxDuration
	}
	if cfg.LiveTailHeartbeat <= 0 {
		cfg.LiveTailHeartbeat = defaultLiveTailHeartbeat
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	auth := NewAuthenticator(cfg.Tokens)
	auth.now = cfg.Now
	uploadAuth := newUploadAuthenticator(cfg.UploadTokens)
	uploadAuth.now = cfg.Now

	proxy := newStorageProxy(storageProxyConfig{
		storage:      cfg.Storage,
		client:       cfg.Client,
		tailClient:   newLiveTailClient(cfg.Client),
		proxyTimeout: cfg.ProxyTimeout,
		liveTail: newLiveTailLimiter(
			cfg.LiveTailMaxGlobal,
			cfg.LiveTailMaxPerUser,
			cfg.LiveTailMaxPerProject,
		),
		liveTailMaxDuration: cfg.LiveTailMaxDuration,
		liveTailHeartbeat:   cfg.LiveTailHeartbeat,
		shutdownCtx:         shutdownCtx,
		logger:              cfg.Logger,
		meterProvider:       cfg.MeterProvider,
	})

	g := &Gateway{
		app:        newFiberApp(cfg.ReadTimeout, cfg.IdleTimeout, cfg.TrustedProxies, cfg.Logger),
		instanceID: cfg.InstanceID,
		now:        cfg.Now,
		logger:     cfg.Logger,
		gatewaySecurity: gatewaySecurity{
			auth:                     auth,
			uploadAuth:               uploadAuth,
			authInvalidator:          cfg.AuthInvalidator,
			account:                  cfg.Account,
			limits:                   cfg.Limits,
			requireCSRF:              cfg.RequireCSRF,
			secureSessionCookie:      cfg.SecureSessionCookie,
			initialAdminPasswordFile: cfg.InitialAdminPasswordFile,
			selfMonitoringToken:      cfg.SelfMonitoringToken,
		},
		gatewayLifecycle: gatewayLifecycle{
			health:         cfg.Health,
			readiness:      cfg.Readiness,
			shutdownCtx:    shutdownCtx,
			shutdownCancel: shutdownCancel,
		},
		domainStores: domainStores{
			store:                   cfg.Store,
			management:              cfg.Management,
			alerts:                  cfg.Alerts,
			issues:                  cfg.Issues,
			errorTracker:            cfg.ErrorTracker,
			notifications:           cfg.Notifications,
			replays:                 cfg.Replays,
			blobs:                   cfg.Blobs,
			sourceMaps:              cfg.SourceMaps,
			profiles:                cfg.Profiles,
			scrapes:                 cfg.Scrapes,
			scrapeUnavailableReason: cfg.ScrapeUnavailableReason,
		},
		gatewayAI: gatewayAI{
			llmPricing: cfg.LLMPricing,
		},
		proxy:           proxy,
		client:          cfg.Client,
		storage:         cfg.Storage,
		gzipPassthrough: newGzipPassthroughPolicy(cfg.Storage, cfg.OTLPGzipPassthrough),
		metrics: storagepkg.NewMetricsClient(storagepkg.MetricsClientConfig{
			Backend:        cfg.Storage,
			HTTPClient:     cfg.Client,
			DefaultTimeout: metricsInstantQueryTimeout,
			MaxBytes:       metricsInstantQueryMaxBytes,
			BodyDrainLimit: metricsBodyDrainLimit,
		}),
		mcpBaseURL:           cfg.MCPBaseURL,
		webUI:                cfg.WebUI,
		instrumentation:      cfg.Instrumentation,
		meterProvider:        cfg.MeterProvider,
		ingestLimiter:        cfg.IngestLimiter,
		selfMetrics:          cfg.SelfMetrics,
		metricsEndpoint:      newMetricsEndpointState(cfg.MetricsEndpoint),
		remoteWriteSem:       make(chan struct{}, defaultRemoteWriteConcurrency),
		remoteWriteRateLimit: defaultRemoteWriteRateLimit,
	}
	g.querier = query.NewStorageQuerier(query.StorageQuerierConfig{
		Backend:        g.storage,
		Client:         g.client,
		IsolateMetrics: g.applyMetricsIsolation,
	})
	if cfg.AISettingsStore != nil {
		// Dynamic mode: the admin UI manages the persisted AI configuration.
		g.ai = newAIManager(
			cfg.AISettingsStore,
			cfg.AISecretBox,
			cfg.AIBuildClient,
			cfg.AITimeout,
			cfg.AISettingsRefreshInterval,
			cfg.OutboundPolicy,
			cfg.Now,
			cfg.Logger,
			cfg.MeterProvider,
		)
		g.ai.loadFromStore(context.Background())
		g.ai.startSyncLoop()
	} else {
		// Static mode: constructor-provided AI state (tests, embedders).
		g.ai = newAIManager(
			nil,
			nil,
			cfg.AIBuildClient,
			cfg.AITimeout,
			cfg.AISettingsRefreshInterval,
			cfg.OutboundPolicy,
			cfg.Now,
			cfg.Logger,
			cfg.MeterProvider,
		)
		g.ai.current = aiSnapshot{
			enabled:   cfg.AIEnabled,
			provider:  cfg.AIProvider,
			model:     cfg.AIModel,
			maxTokens: aiDefaultMaxTokens,
			hasKey:    cfg.AIClient != nil,
			client:    g.ai.wrapClient(cfg.AIClient, cfg.AIProvider, cfg.AIModel),
		}
	}
	g.aiAssistant = newAssistantService(g)
	g.issueAI = newIssueAIService(g, cfg.IssueAIAutoDailyBudget)
	g.startAuthInvalidation()
	g.routes()
	return g
}

func (g *Gateway) startAuthInvalidation() {
	if g.authInvalidator == nil {
		return
	}
	ready := make(chan struct{})
	g.invalidatorReady = ready
	var readyOnce sync.Once
	markReady := func() { readyOnce.Do(func() { close(ready) }) }
	g.invalidatorWG.Add(1)
	go func() {
		defer g.invalidatorWG.Done()
		defer markReady()
		err := g.authInvalidator.Subscribe(g.shutdownCtx, markReady, g.invalidateAuthCaches)
		if err != nil && !errors.Is(err, context.Canceled) && g.shutdownCtx.Err() == nil {
			g.logger.Warn("auth invalidation subscriber exited", "error", err)
		}
	}()
}

func (g *Gateway) invalidateAuthCaches() {
	if g.auth != nil {
		g.auth.Invalidate()
	}
	if g.uploadAuth != nil {
		g.uploadAuth.Invalidate()
	}
}

func (g *Gateway) notifyAuthInvalidation(ctx context.Context) {
	g.invalidateAuthCaches()
	if g.authInvalidator == nil {
		return
	}
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := g.authInvalidator.Publish(pubCtx); err != nil {
		g.logger.Warn("publish auth invalidation failed", "error", err)
	}
}

func (g *Gateway) waitAuthInvalidationReady(ctx context.Context) error {
	if g.invalidatorReady == nil {
		return nil
	}
	select {
	case <-g.invalidatorReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gateway) App() *fiber.App { return g.app }

// IngestAuthenticator returns the gateway's shared project-token authenticator
// so auxiliary ingest listeners (such as OTLP/gRPC) share the same cache and
// cross-replica invalidation stream.
func (g *Gateway) IngestAuthenticator() *Authenticator { return g.auth }

// AttachErrorTracker completes composition before the listener starts. The
// Gateway owns only the HTTP acceptance seam; the composition root owns the
// durable module and its metadata, VLogs, and symbolication adapters.
func (g *Gateway) AttachErrorTracker(tracker ErrorIntake) error {
	if tracker == nil {
		return errors.New("gateway: error tracker is required")
	}
	if g.errorTracker != nil {
		return errors.New("gateway: error tracker is already configured")
	}
	g.errorTracker = tracker
	return nil
}

// listenConfig keeps Fiber's startup banner off stdout: Fiber v3 moved
// DisableStartupMessage from fiber.Config to fiber.ListenConfig, so every
// Listen/Listener call has to pass it.
var listenConfig = fiber.ListenConfig{DisableStartupMessage: true}

func (g *Gateway) Listen(addr string) error { return g.app.Listen(addr, listenConfig) }

// Serve accepts connections on ln until Shutdown or Drain. The listener is
// served as supplied, so TLS stays whatever the caller wrapped it with.
func (g *Gateway) Serve(ln net.Listener) error { return g.app.Listener(ln, listenConfig) }

// MarkReady opens the readiness gate after startup initialization and metadata
// migrations have completed. New gateways deliberately start unready.
func (g *Gateway) MarkReady() {
	g.readyMu.Lock()
	g.ready = true
	g.readyMu.Unlock()
}

// BeginDrain immediately removes the replica from service discovery and cancels
// long-lived streaming upstreams (such as Live Tail) so shutdown does not stall.
// It is idempotent so signal and error paths can safely converge on it.
func (g *Gateway) BeginDrain() {
	g.drainOnce.Do(func() {
		g.readyMu.Lock()
		g.ready = false
		g.readyMu.Unlock()
		if g.shutdownCancel != nil {
			g.shutdownCancel()
		}
	})
}

// Drain stops accepting requests, waits for in-flight handlers, and only then
// flushes the Issue batch. The supplied context bounds the whole operation.
func (g *Gateway) Drain(ctx context.Context) error {
	g.BeginDrain()
	shutdownErr := g.app.ShutdownWithContext(ctx)
	g.invalidatorWG.Wait()
	var errorTrackErr error
	if g.errorTracker != nil {
		errorTrackErr = g.errorTracker.Close(ctx)
	}
	var aiErr error
	if g.issueAI != nil {
		aiErr = g.issueAI.Close(ctx)
	}
	var aiSyncErr error
	if g.ai != nil {
		aiSyncErr = g.ai.Close(ctx)
	}
	return errors.Join(shutdownErr, errorTrackErr, aiErr, aiSyncErr)
}

func (g *Gateway) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	return g.Drain(ctx)
}

// routes registers handlers in the exact order Fiber must match them:
// cross-cutting middleware and probes first, the SPA fallback last.
func (g *Gateway) routes() {
	g.registerMiddlewareAndProbes()
	g.registerPlatformRoutes()
	g.registerAuthRoutes()
	g.registerTelemetryRoutes()
	g.registerSessionAPI()
	g.app.Use(g.serveWebUI)
}

// registerMiddlewareAndProbes installs HTTP self-metrics observation, panic
// recovery, bounded request decoding, the kubelet probes, and optional
// self-instrumentation.
func (g *Gateway) registerMiddlewareAndProbes() {
	// Observe HTTP requests with lock-free in-memory counters before recover so
	// recovered panics are also counted as 5xx responses.
	g.app.Use(g.httpMetricsMiddleware)
	// A handler panic answers 500 for that request instead of killing the
	// process (and every in-flight request with it).
	g.app.Use(fiberrecover.New(fiberrecover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c fiber.Ctx, value any) {
			// g.logger may export asynchronously; never hand it borrowed
			// request memory.
			g.logger.Error(
				"gateway handler panic",
				"method", strings.Clone(c.Method()), "path", strings.Clone(c.Path()), "panic", fmt.Sprint(value),
				"stack", string(debug.Stack()),
			)
		},
	}))
	// Bound content-coding expansion before any route can read the body.
	g.app.Use(boundedRequestDecodingWithPolicy(g.gzipPassthrough))
	// Register kubelet probes before optional instrumentation. In particular,
	// /livez must not depend on telemetry middleware or any external service.
	g.route(fiber.MethodGet, "/livez", routePolicy{auth: authPublic}, g.livez)
	g.route(fiber.MethodGet, "/readyz", routePolicy{auth: authPublic}, g.readyz)
	if g.instrumentation != nil {
		g.app.Use(func(c fiber.Ctx) error {
			// OTLP exports are never observed, and the private token is a second
			// marker for any future internal endpoint. Either condition prevents
			// self-generated telemetry from recursively producing more telemetry.
			if strings.HasPrefix(c.Path(), "/v1/") ||
				(g.selfMonitoringToken != "" && bearerCredential(c) == g.selfMonitoringToken) {
				return c.Next()
			}
			return g.instrumentation(c)
		})
	}
}

// registerPlatformRoutes serves health, optional Prometheus self-metrics,
// system diagnostics, and MCP.
func (g *Gateway) registerPlatformRoutes() {
	g.route(fiber.MethodGet, "/healthz", routePolicy{auth: authPublic}, g.healthz)
	if g.metricsEndpoint.enabled {
		g.route(fiber.MethodGet, "/metrics", g.metricsEndpoint.routePolicy(), g.prometheusMetrics)
	}
	g.route(fiber.MethodGet, "/api/system/info", routePolicy{auth: authUser, projectScope: true}, g.systemInfo)
	g.route(
		fiber.MethodGet,
		"/api/system/error-inbox",
		routePolicy{auth: authUser, role: roleAdmin},
		g.errorInboxDiagnostics,
	)
	baseURL := g.mcpBaseURL
	if baseURL == "" {
		baseURL = "http://127.0.0.1:3100"
	}
	mcpHandler := fasthttpadaptor.NewFastHTTPHandler(observemcp.NewHTTPHandler(baseURL, nil))
	g.routeAll(
		"/mcp",
		routePolicy{auth: authUserTokenOnly, projectScope: true},
		func(c fiber.Ctx) error {
			mcpHandler(c.RequestCtx())
			return nil
		},
	)
}

// registerAuthRoutes serves login and the current user's own account.
func (g *Gateway) registerAuthRoutes() {
	g.route(fiber.MethodPost, "/api/auth/login", routePolicy{auth: authPublic}, g.login)
	g.route(fiber.MethodGet, "/api/auth/me", routePolicy{auth: authUser, allowWhenMustChange: true}, g.me)
	// Login is the only cookie-less mutation here; every other /api/auth
	// mutation rides on the session cookie and needs the CSRF double-submit.
	g.route(
		fiber.MethodPost,
		"/api/auth/logout",
		routePolicy{auth: authSessionOnly, csrf: true, allowWhenMustChange: true},
		g.logout,
	)
	g.route(fiber.MethodGet, "/api/auth/tokens", routePolicy{auth: authSessionOnly}, g.listCurrentUserTokens)
	g.route(
		fiber.MethodPost,
		"/api/auth/tokens",
		routePolicy{auth: authSessionOnly, csrf: true},
		g.createCurrentUserToken,
	)
	g.route(
		fiber.MethodDelete,
		"/api/auth/tokens/:tokenID",
		routePolicy{auth: authSessionOnly, csrf: true},
		g.revokeCurrentUserToken,
	)
	g.route(
		fiber.MethodPost,
		"/api/auth/password",
		routePolicy{auth: authSessionOnly, csrf: true, allowWhenMustChange: true},
		g.changePassword,
	)
	g.route(fiber.MethodPut, "/api/auth/me", routePolicy{auth: authSessionOnly, csrf: true}, g.updateMe)
	g.route(fiber.MethodGet, "/api/auth/sessions", routePolicy{auth: authSessionOnly}, g.listMySessions)
	g.route(
		fiber.MethodDelete,
		"/api/auth/sessions/:id",
		routePolicy{auth: authSessionOnly, csrf: true},
		g.deleteMySession,
	)
	g.route(fiber.MethodGet, "/api/audit", routePolicy{auth: authUser, role: roleAdmin}, g.listAudit)
}

// registerTelemetryRoutes serves ingest (project tokens) and the project-scoped
// read API (sessions or user tokens).
func (g *Gateway) registerTelemetryRoutes() {
	projectQuery := routePolicy{auth: authUser, projectScope: true}

	g.route(fiber.MethodPost, "/v1/metrics", projectIngestPolicy(selfmetrics.SignalMetrics), g.metricsIngest)
	g.route(fiber.MethodPost, "/api/v1/write", projectIngestPolicy(selfmetrics.SignalMetrics), g.metricsRemoteWrite)
	g.route(
		fiber.MethodPost,
		"/v1/logs",
		projectIngestPolicy(selfmetrics.SignalLogs),
		g.proxy.tenantHandler(g.storage.LogsIngestBase(), "/insert/opentelemetry/v1/logs"),
	)
	g.route(
		fiber.MethodPost,
		"/v1/traces",
		browserIngestPolicy(selfmetrics.SignalTraces),
		g.proxy.tenantHandler(g.storage.TracesIngestBase(), "/insert/opentelemetry/v1/traces"),
	)
	g.route(fiber.MethodPost, "/api/events/errors", browserIngestPolicy(selfmetrics.SignalErrors), g.errorsIngest)
	g.route(fiber.MethodPost, "/api/events/vitals", browserIngestPolicy(selfmetrics.SignalVitals), g.vitalsIngest)
	g.route(
		fiber.MethodPost,
		"/api/events/diagnostics",
		browserIngestPolicy(selfmetrics.SignalDiagnostics),
		g.sdkDiagnosticsIngest,
	)
	g.route(
		fiber.MethodPost,
		"/api/releases/:release/sourcemaps",
		routePolicy{auth: authUploadToken},
		g.uploadSourceMaps,
	)
	g.route(fiber.MethodGet, "/api/releases", projectQuery, g.listSourceMapReleases)
	g.route(
		fiber.MethodPost,
		"/api/replay/segments",
		browserIngestPolicy(selfmetrics.SignalReplay),
		g.publicIngestAdmission(replayIngestPolicy),
		g.replayIngest,
	)
	g.route(
		fiber.MethodPost,
		"/api/profiles",
		projectIngestPolicy(selfmetrics.SignalProfiles),
		g.publicIngestAdmission(profileIngestPolicy),
		g.profileIngest,
	)
	g.route(fiber.MethodGet, "/api/profiles/services", projectQuery, g.listProfileServices)
	g.route(fiber.MethodGet, "/api/profiles/flamegraph", projectQuery, g.profileFlamegraph)
	g.route(fiber.MethodGet, "/api/profiles/:id/raw", projectQuery, g.rawProfile)
	g.route(fiber.MethodGet, "/api/profiles", projectQuery, g.listProfiles)
	g.route(fiber.MethodGet, "/api/replay/sessions", projectQuery, g.listReplaySessions)
	g.route(fiber.MethodGet, "/api/replay/sessions/:session_id", projectQuery, g.getReplaySession)
	g.route(fiber.MethodGet, "/api/replay/sessions/:session_id/segments/:seq", projectQuery, g.getReplaySegment)
	for _, metricPath := range []string{
		"/api/v1/query", "/api/v1/query_range", "/api/v1/series", "/api/v1/labels",
		"/api/v1/label/:name/values",
	} {
		g.routeAll(metricPath, projectQuery, g.metricsQuery)
	}
	g.route(fiber.MethodGet, "/api/v1/prettify-query", projectQuery, g.proxy.metricsPrettify)
	for _, route := range logsProxyRoutes {
		handler := g.proxy.logsHandler(route)
		g.route(fiber.MethodGet, route.apiPath, projectQuery, handler)
		g.route(fiber.MethodPost, route.apiPath, projectQuery, handler)
	}
	g.route(
		fiber.MethodGet,
		"/api/traces/services",
		projectQuery,
		g.proxy.tenantHandler(g.storage.TracesQueryBase(), "/select/jaeger/api/services"),
	)
	g.route(fiber.MethodGet, "/api/traces/services/:svc/operations", projectQuery, g.traceOperations)
	g.route(fiber.MethodGet, "/api/traces/search", projectQuery, g.traceSearch)
	g.route(fiber.MethodGet, "/api/traces/:traceID", projectQuery, g.traceByID)
	g.route(fiber.MethodGet, "/api/llm/summary", projectQuery, g.llmSummary)
	g.route(fiber.MethodGet, "/api/llm/calls", projectQuery, g.llmCalls)
	g.route(fiber.MethodGet, "/api/llm/timeseries", projectQuery, g.llmTimeseries)
	g.route(fiber.MethodGet, "/api/v1/performance/endpoints", projectQuery, g.performanceEndpoints)
	g.route(fiber.MethodGet, "/api/v1/performance/vitals", projectQuery, g.performanceVitals)
	// Token-authenticated, read-only query aliases used by MCP. They deliberately
	// reuse the same handlers as the UI and therefore retain project isolation.
	g.route(fiber.MethodGet, "/api/v1/issues", projectQuery, g.listIssues)
	g.route(fiber.MethodGet, "/api/v1/issues/:id", projectQuery, g.getIssue)
	g.route(fiber.MethodGet, "/api/v1/issues/:id/events/:event_id", projectQuery, g.getIssueEvent)
	g.route(fiber.MethodGet, "/api/v1/alerts", projectQuery, g.listAlerts)
	g.route(fiber.MethodGet, "/api/v1/hosts", projectQuery, g.hosts)
}

// registerSessionAPI serves the web UI's management API under /api with
// explicit per-route authentication, CSRF, role, and project-scope policies.
func (g *Gateway) registerSessionAPI() {
	g.route(fiber.MethodGet, "/api/ai/status", routePolicy{auth: authUser}, g.aiStatus)
	g.route(fiber.MethodPost, "/api/ai/test", routePolicy{auth: authUser, role: roleAdmin, csrf: true}, g.aiTest)
	g.route(fiber.MethodGet, "/api/ai/settings", routePolicy{auth: authUser, role: roleAdmin}, g.aiSettingsGet)
	g.route(
		fiber.MethodPut,
		"/api/ai/settings",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.aiSettingsPut,
	)
	g.route(
		fiber.MethodPost,
		"/api/ai/assistant/query",
		routePolicy{auth: authUser, csrf: true, projectScope: true},
		g.aiAssistantQuery,
	)
	g.route(
		fiber.MethodPost,
		"/api/ai/assistant/explain",
		routePolicy{auth: authUser, csrf: true},
		g.aiAssistantExplain,
	)

	g.route(fiber.MethodGet, "/api/projects", routePolicy{auth: authUser}, g.listProjects)
	g.route(fiber.MethodGet, "/api/projects/:id", routePolicy{auth: authUser}, g.getProject)
	g.route(fiber.MethodGet, "/api/projects/:id/alert-config", routePolicy{auth: authUser}, g.getProjectAlertConfig)
	g.route(
		fiber.MethodPut,
		"/api/projects/:id/alert-config",
		routePolicy{auth: authUser, role: roleMember, csrf: true},
		g.putProjectAlertConfig,
	)
	g.route(fiber.MethodGet, "/api/projects/:id/ai-config", routePolicy{auth: authUser}, g.getProjectAIConfig)
	g.route(
		fiber.MethodPut,
		"/api/projects/:id/ai-config",
		routePolicy{auth: authUser, role: roleMember, csrf: true},
		g.putProjectAIConfig,
	)
	g.route(
		fiber.MethodPost,
		"/api/projects",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.createProject,
	)
	g.route(
		fiber.MethodPatch,
		"/api/projects/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.updateProject,
	)
	g.route(
		fiber.MethodPut,
		"/api/projects/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.updateProject,
	)
	g.route(
		fiber.MethodDelete,
		"/api/projects/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.deleteProject,
	)
	g.route(fiber.MethodGet, "/api/projects/:id/tokens", routePolicy{auth: authUser}, g.listProjectAPITokens)
	g.route(
		fiber.MethodPost,
		"/api/projects/:id/tokens",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.createProjectAPIToken,
	)
	g.route(
		fiber.MethodDelete,
		"/api/projects/:id/tokens/:tokenID",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.revokeProjectAPIToken,
	)
	g.route(
		fiber.MethodGet,
		"/api/projects/:id/source-map-upload-tokens",
		routePolicy{auth: authUser, role: roleAdmin},
		g.listSourceMapUploadTokens,
	)
	g.route(
		fiber.MethodPost,
		"/api/projects/:id/source-map-upload-tokens",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.createSourceMapUploadToken,
	)
	g.route(
		fiber.MethodDelete,
		"/api/projects/:id/source-map-upload-tokens/:tokenID",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.revokeSourceMapUploadToken,
	)

	g.route(fiber.MethodGet, "/api/users", routePolicy{auth: authUser, role: roleAdmin}, g.listUsers)
	g.route(fiber.MethodGet, "/api/users/:id", routePolicy{auth: authUser, role: roleAdmin}, g.getUser)
	g.route(fiber.MethodPost, "/api/users", routePolicy{auth: authUser, role: roleAdmin, csrf: true}, g.createUser)
	g.route(
		fiber.MethodPatch,
		"/api/users/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.updateUser,
	)
	g.route(fiber.MethodPut, "/api/users/:id", routePolicy{auth: authUser, role: roleAdmin, csrf: true}, g.updateUser)
	g.route(
		fiber.MethodDelete,
		"/api/users/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.deleteUser,
	)

	g.route(
		fiber.MethodGet,
		"/api/alert-rules",
		routePolicy{auth: authUser, role: roleMember, projectScope: true},
		g.listAlertRules,
	)
	g.route(
		fiber.MethodPost,
		"/api/alert-rules",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.createAlertRule,
	)
	g.route(
		fiber.MethodPatch,
		"/api/alert-rules/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateAlertRule,
	)
	g.route(
		fiber.MethodPut,
		"/api/alert-rules/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateAlertRule,
	)
	g.route(
		fiber.MethodDelete,
		"/api/alert-rules/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.deleteAlertRule,
	)
	g.route(
		fiber.MethodGet,
		"/api/notifications",
		routePolicy{auth: authUser, role: roleMember, projectScope: true},
		g.listNotifications,
	)
	g.route(
		fiber.MethodPost,
		"/api/notifications/:id/retry",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.requeueNotification,
	)
	g.route(fiber.MethodGet, "/api/channels", routePolicy{auth: authUser, role: roleMember}, g.listChannels)
	g.route(
		fiber.MethodPost,
		"/api/channels",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.createChannel,
	)
	g.route(
		fiber.MethodPatch,
		"/api/channels/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.updateChannel,
	)
	g.route(
		fiber.MethodPut,
		"/api/channels/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.updateChannel,
	)
	g.route(
		fiber.MethodDelete,
		"/api/channels/:id",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.deleteChannel,
	)
	g.route(
		fiber.MethodPost,
		"/api/channels/:id/test",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true},
		g.testChannel,
	)
	g.route(
		fiber.MethodGet,
		"/api/silences",
		routePolicy{auth: authUser, role: roleMember, projectScope: true},
		g.listSilences,
	)
	g.route(
		fiber.MethodPost,
		"/api/silences",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.createSilence,
	)
	g.route(
		fiber.MethodPatch,
		"/api/silences/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateSilence,
	)
	g.route(
		fiber.MethodPut,
		"/api/silences/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateSilence,
	)
	g.route(
		fiber.MethodDelete,
		"/api/silences/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.deleteSilence,
	)
	g.route(
		fiber.MethodGet,
		"/api/inhibition-rules",
		routePolicy{auth: authUser, role: roleMember, projectScope: true},
		g.listInhibitionRules,
	)
	g.route(
		fiber.MethodPost,
		"/api/inhibition-rules",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.createInhibitionRule,
	)
	g.route(
		fiber.MethodPatch,
		"/api/inhibition-rules/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateInhibitionRule,
	)
	g.route(
		fiber.MethodPut,
		"/api/inhibition-rules/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateInhibitionRule,
	)
	g.route(
		fiber.MethodDelete,
		"/api/inhibition-rules/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.deleteInhibitionRule,
	)
	g.route(fiber.MethodGet, "/api/alerts", routePolicy{auth: authUser, projectScope: true}, g.listAlerts)

	g.route(
		fiber.MethodGet,
		"/api/scrape-targets",
		routePolicy{auth: authUser, projectScope: true},
		g.listScrapeTargets,
	)
	g.route(
		fiber.MethodPost,
		"/api/scrape-targets",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.createScrapeTarget,
	)
	g.route(
		fiber.MethodGet,
		"/api/scrape-targets/:id",
		routePolicy{auth: authUser, projectScope: true},
		g.getScrapeTarget,
	)
	g.route(
		fiber.MethodPatch,
		"/api/scrape-targets/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateScrapeTarget,
	)
	g.route(
		fiber.MethodPut,
		"/api/scrape-targets/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.updateScrapeTarget,
	)
	g.route(
		fiber.MethodDelete,
		"/api/scrape-targets/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.deleteScrapeTarget,
	)
	g.route(
		fiber.MethodGet,
		"/api/scrape-targets/:id/health",
		routePolicy{auth: authUser, projectScope: true},
		g.scrapeTargetHealth,
	)
	g.route(fiber.MethodGet, "/api/hosts", routePolicy{auth: authUser, projectScope: true}, g.hosts)

	g.route(fiber.MethodGet, "/api/issues", routePolicy{auth: authUser, projectScope: true}, g.listIssues)
	g.route(fiber.MethodGet, "/api/issues/:id", routePolicy{auth: authUser, projectScope: true}, g.getIssue)
	g.route(
		fiber.MethodGet,
		"/api/issues/:id/ai-analysis",
		routePolicy{auth: authUser, projectScope: true},
		g.getIssueAIAnalysis,
	)
	g.route(
		fiber.MethodPost,
		"/api/issues/:id/ai-analysis",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.postIssueAIAnalysis,
	)
	g.route(
		fiber.MethodPatch,
		"/api/issues/:id",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.patchIssue,
	)
	g.route(
		fiber.MethodPost,
		"/api/issues/batch",
		routePolicy{auth: authUser, role: roleMember, csrf: true, projectScope: true},
		g.batchUpdateIssues,
	)
	g.route(
		fiber.MethodGet,
		"/api/issues/:id/events/:event_id",
		routePolicy{auth: authUser, projectScope: true},
		g.getIssueEvent,
	)
	g.route(
		fiber.MethodDelete,
		"/api/releases/:release/sourcemaps",
		routePolicy{auth: authUser, role: roleAdmin, csrf: true, projectScope: true},
		g.deleteSourceMaps,
	)
}

func (g *Gateway) serveWebUI(c fiber.Ctx) error {
	if g.webUI == nil || (c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead) {
		return c.SendStatus(fiber.StatusNotFound)
	}
	requestPath := c.Path()
	for _, prefix := range []string{"/api", "/v1", "/mcp", "/healthz", "/livez", "/readyz", "/select", "/metrics"} {
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return c.SendStatus(fiber.StatusNotFound)
		}
	}
	name := strings.TrimPrefix(path.Clean("/"+requestPath), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	data, err := fs.ReadFile(g.webUI, name)
	if err != nil {
		if path.Ext(name) != "" {
			return c.SendStatus(fiber.StatusNotFound)
		}
		name = "index.html"
		data, err = fs.ReadFile(g.webUI, name)
		if err != nil {
			return c.SendStatus(fiber.StatusNotFound)
		}
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	c.Set(fiber.HeaderContentType, contentType)
	if c.Method() == fiber.MethodHead {
		return c.SendStatus(fiber.StatusOK)
	}
	return c.Send(data)
}

func probeComponentStatus(err error) string {
	if err == nil {
		return "ok"
	}
	if err.Error() == "component restarting" {
		return "component restarting"
	}
	return "unavailable"
}

func (g *Gateway) healthz(c fiber.Ctx) error {
	probeCtx, cancel := context.WithTimeout(c.Context(), defaultProbeTimeout)
	defer cancel()
	status := fiber.StatusOK
	components := map[string]string{"gateway": "ok"}
	if g.health != nil {
		for name, err := range g.health.Health(probeCtx) {
			if err != nil {
				status = fiber.StatusServiceUnavailable
				g.logger.Warn("health probe component unhealthy", "component", name, "error", err)
				components[name] = probeComponentStatus(err)
			} else {
				components[name] = "ok"
			}
		}
	}
	if g.errorTracker != nil {
		summary, err := g.errorTracker.Summary(probeCtx)
		if err != nil {
			status = fiber.StatusServiceUnavailable
			components["error_inbox"] = "diagnostics unavailable"
		} else {
			components["error_inbox"] = fmt.Sprintf(
				"active_items=%d active_bytes=%d",
				summary.ActiveItems,
				summary.ActiveBytes,
			)
		}
	}
	return c.Status(status).
		JSON(fiber.Map{"status": map[bool]string{true: "ok", false: "degraded"}[status == fiber.StatusOK], "components": components})
}

func (g *Gateway) livez(c fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"status": "ok"})
}

func (g *Gateway) readyz(c fiber.Ctx) error {
	g.readyMu.RLock()
	ready := g.ready
	g.readyMu.RUnlock()
	components := map[string]string{"initialization": "ok"}
	if !ready {
		components["initialization"] = "not ready or draining"
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"status": "not_ready", "components": components})
	}
	probeCtx, cancel := context.WithTimeout(c.Context(), defaultProbeTimeout)
	defer cancel()
	checker := g.readiness
	if checker == nil {
		checker = g.health
	}
	if checker != nil {
		for name, err := range checker.Health(probeCtx) {
			if err != nil {
				g.logger.Warn("readiness probe component not ready", "component", name, "error", err)
				components[name] = probeComponentStatus(err)
				ready = false
			} else {
				components[name] = "ok"
			}
		}
	}
	if g.errorTracker != nil {
		if err := g.errorTracker.Readiness(probeCtx); err != nil {
			g.logger.Warn("readiness probe error inbox not ready", "error", err)
			components["error_inbox"] = "unavailable"
			ready = false
		} else {
			components["error_inbox"] = "ok"
		}
	}
	status := fiber.StatusOK
	if !ready {
		status = fiber.StatusServiceUnavailable
	}
	return c.Status(status).
		JSON(fiber.Map{"status": map[bool]string{true: "ready", false: "not_ready"}[ready], "components": components})
}

func (g *Gateway) metricsIngest(c fiber.Ctx) error {
	return g.proxy.metricsWrite(c, "/opentelemetry/v1/metrics", true)
}

func (g *Gateway) metricsRemoteWrite(c fiber.Ctx) error {
	if !g.storage.MetricsIngestEnabled() {
		return c.Status(fiber.StatusNotImplemented).
			JSON(fiber.Map{"error": "metrics ingest is disabled: storage.metrics.ingest_url is not configured"})
	}
	if strings.Contains(
		strings.ToLower(c.Get(fiber.HeaderContentType)),
		"io.prometheus.write.v2.request",
	) ||
		strings.HasPrefix(strings.TrimSpace(c.Get("X-Prometheus-Remote-Write-Version")), "2.") {
		return fiber.NewError(
			fiber.StatusUnsupportedMediaType,
			"Prometheus remote-write 2.0 is not supported; use prometheus.WriteRequest",
		)
	}
	if g.remoteWriteSem != nil {
		select {
		case g.remoteWriteSem <- struct{}{}:
			defer func() { <-g.remoteWriteSem }()
		default:
			c.Set(fiber.HeaderRetryAfter, "1")
			return c.Status(fiber.StatusTooManyRequests).
				JSON(fiber.Map{"error": "too many concurrent remote-write requests"})
		}
	}
	p := currentProject(c)
	if g.limits != nil && g.remoteWriteRateLimit > 0 {
		allowed, limitErr := g.limits.Allow(
			c.Context(),
			"remote-write:project:"+strconv.Itoa(p.ID),
			g.remoteWriteRateLimit,
			defaultRemoteWriteRateWindow,
		)
		if limitErr != nil {
			g.logger.Warn("remote-write rate limit check failed", "project", p.Slug, "error", limitErr)
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "rate limiter temporarily unavailable"})
		}
		if !allowed {
			c.Set(fiber.HeaderRetryAfter, "1")
			return c.Status(fiber.StatusTooManyRequests).
				JSON(fiber.Map{"error": "remote-write rate limit exceeded"})
		}
	}
	raw := requestBody(c)
	if len(raw) > maxRemoteWriteDecodedBody {
		return fiber.NewError(
			fiber.StatusRequestEntityTooLarge,
			"Prometheus remote-write request exceeds size limit",
		)
	}
	body, err := storagepkg.InjectRemoteWriteProject(raw, p.Slug, maxRemoteWriteDecodedBody)
	if errors.Is(err, storagepkg.ErrRemoteWriteTooLarge) {
		return fiber.NewError(
			fiber.StatusRequestEntityTooLarge,
			"Prometheus remote-write request exceeds decoded size or cardinality limit",
		)
	}
	if err != nil {
		return fiber.NewError(
			fiber.StatusBadRequest,
			"invalid Prometheus remote-write request: "+err.Error(),
		)
	}
	c.Request().Header.SetContentType("application/x-protobuf")
	c.Request().Header.Set("Content-Encoding", "snappy")
	c.Request().Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	return g.proxy.metricsWriteWithBody(c, "/prometheus/api/v1/write", false, body)
}

func accountUserInfo(user account.User) meta.UserInfo {
	return meta.UserInfo{
		ID: user.ID, Username: user.Username, Role: user.Role, DisplayName: user.DisplayName, Email: user.Email,
		CreatedAt: user.CreatedAt, LastLoginAt: user.LastLoginAt, MustChangePassword: user.MustChangePassword,
	}
}

func accountUser(user meta.UserInfo) account.User {
	return account.User{
		ID: user.ID, Username: user.Username, Role: user.Role, DisplayName: user.DisplayName, Email: user.Email,
		CreatedAt: user.CreatedAt, LastLoginAt: user.LastLoginAt, MustChangePassword: user.MustChangePassword,
	}
}

// errResponseWritten marks auth failures whose HTTP response has already been
// written. Callers stop the handler chain without returning the error to
// Fiber's error handler (which would rewrite the status to 500).
var errResponseWritten = errors.New("authentication response already written")

func unauthorized(c fiber.Ctx) error {
	_ = c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	return errResponseWritten
}

func currentProject(c fiber.Ctx) meta.ProjectInfo {
	p, _ := c.Locals(projectLocal).(meta.ProjectInfo)
	return p
}
