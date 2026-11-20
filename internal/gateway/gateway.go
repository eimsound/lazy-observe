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
	"sync/atomic"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/embed"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/llmcost"
	"github.com/eimsound/lazy-observe/internal/management"
	observemcp "github.com/eimsound/lazy-observe/internal/mcp"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/scrape"
	"github.com/eimsound/lazy-observe/internal/secret"
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
const tokenCacheTTL = 30 * time.Second

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
}

type Gateway struct {
	app                      *fiber.App
	instanceID               string
	auth                     *Authenticator
	uploadAuth               *Authenticator
	store                    ManagementStore
	account                  account.Behavior
	limits                   state.Limiter
	management               *management.Manager
	health                   HealthChecker
	readiness                HealthChecker
	ready                    bool
	readyMu                  sync.RWMutex
	drainOnce                sync.Once
	shutdownCtx              context.Context
	shutdownCancel           context.CancelFunc
	client                   *http.Client
	tailClient               *http.Client
	proxyTimeout             time.Duration
	liveTail                 *liveTailLimiter
	liveTailMaxDuration      time.Duration
	liveTailHeartbeat        time.Duration
	proxyStreamErrors        atomic.Int64
	proxyStreamErrCounter    metric.Int64Counter
	now                      func() time.Time
	alerts                   *alert.Service
	issues                   IssueStore
	errorTracker             ErrorIntake
	notifications            NotificationStore
	replays                  ReplayStore
	blobs                    blob.Store
	sourceMaps               SourceMapStore
	profiles                 ProfileStore
	scrapes                  *scrape.Service
	storage                  storagepkg.SignalBackend
	metrics                  *storagepkg.MetricsClient
	scrapeUnavailableReason  string
	mcpBaseURL               string
	ai                       *aiManager
	aiAssistant              *assistantService
	issueAI                  *issueAIService
	llmPricing               *llmcost.Table
	webUI                    fs.FS
	instrumentation          fiber.Handler
	selfMonitoringToken      string
	logger                   *slog.Logger
	meterProvider            metric.MeterProvider
	replayLocks              [64]sync.Mutex
	requireCSRF              bool
	secureSessionCookie      bool
	initialAdminPasswordFile string
	remoteWriteSem           chan struct{}
	remoteWriteRateLimit     int
}

func newFiberApp(readTimeout, idleTimeout time.Duration, trustedProxies []string) *fiber.App {
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
	g := &Gateway{
		app:        newFiberApp(cfg.ReadTimeout, cfg.IdleTimeout, cfg.TrustedProxies),
		instanceID: cfg.InstanceID,
		auth: NewAuthenticator(
			cfg.Tokens,
		),
		uploadAuth:     newUploadAuthenticator(cfg.UploadTokens),
		store:          cfg.Store,
		account:        cfg.Account,
		limits:         cfg.Limits,
		management:     cfg.Management,
		health:         cfg.Health,
		readiness:      cfg.Readiness,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		client:         cfg.Client,
		tailClient:     newLiveTailClient(cfg.Client),
		proxyTimeout:   cfg.ProxyTimeout,
		liveTail: newLiveTailLimiter(
			cfg.LiveTailMaxGlobal,
			cfg.LiveTailMaxPerUser,
			cfg.LiveTailMaxPerProject,
		),
		liveTailMaxDuration: cfg.LiveTailMaxDuration,
		liveTailHeartbeat:   cfg.LiveTailHeartbeat,
		now:                 cfg.Now,
		alerts:              cfg.Alerts,
		issues:              cfg.Issues,
		replays:             cfg.Replays,
		blobs:               cfg.Blobs,
		sourceMaps:          cfg.SourceMaps,
		profiles:            cfg.Profiles,
		scrapes:             cfg.Scrapes,
		storage:             cfg.Storage,
		metrics: storagepkg.NewMetricsClient(storagepkg.MetricsClientConfig{
			Backend:        cfg.Storage,
			HTTPClient:     cfg.Client,
			DefaultTimeout: metricsInstantQueryTimeout,
			MaxBytes:       metricsInstantQueryMaxBytes,
			BodyDrainLimit: metricsBodyDrainLimit,
		}),
		scrapeUnavailableReason:  cfg.ScrapeUnavailableReason,
		mcpBaseURL:               cfg.MCPBaseURL,
		llmPricing:               cfg.LLMPricing,
		webUI:                    cfg.WebUI,
		instrumentation:          cfg.Instrumentation,
		selfMonitoringToken:      cfg.SelfMonitoringToken,
		logger:                   cfg.Logger,
		meterProvider:            cfg.MeterProvider,
		requireCSRF:              cfg.RequireCSRF,
		secureSessionCookie:      cfg.SecureSessionCookie,
		initialAdminPasswordFile: cfg.InitialAdminPasswordFile,
		remoteWriteSem:           make(chan struct{}, defaultRemoteWriteConcurrency),
		remoteWriteRateLimit:     defaultRemoteWriteRateLimit,
	}
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
		)
		g.ai.current = aiSnapshot{
			enabled:   cfg.AIEnabled,
			provider:  cfg.AIProvider,
			model:     cfg.AIModel,
			maxTokens: aiDefaultMaxTokens,
			hasKey:    cfg.AIClient != nil,
			client:    cfg.AIClient,
		}
	}
	g.aiAssistant = newAssistantService(g)
	g.issueAI = newIssueAIService(g, cfg.IssueAIAutoDailyBudget)
	g.errorTracker = cfg.ErrorTracker
	g.notifications = cfg.Notifications
	if g.meterProvider != nil {
		meter := g.meterProvider.Meter("github.com/eimsound/lazy-observe/internal/gateway")
		g.proxyStreamErrCounter, _ = meter.Int64Counter("gateway.proxy.stream_errors")
	}
	g.routes()
	return g
}

func (g *Gateway) App() *fiber.App { return g.app }

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

// registerMiddlewareAndProbes installs panic recovery, bounded request
// decoding, the kubelet probes, and optional self-instrumentation.
func (g *Gateway) registerMiddlewareAndProbes() {
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
	g.app.Use(boundedRequestDecoding)
	// Register kubelet probes before optional instrumentation. In particular,
	// /livez must not depend on telemetry middleware or any external service.
	g.app.Get("/livez", g.livez)
	g.app.Get("/readyz", g.readyz)
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

// registerPlatformRoutes serves health, system diagnostics, and MCP.
func (g *Gateway) registerPlatformRoutes() {
	g.app.Get("/healthz", g.healthz)
	g.app.Get("/api/system/info", g.queryRequired, g.systemInfo)
	g.app.Get("/api/system/error-inbox", g.userRequired, g.adminRequired, g.errorInboxDiagnostics)
	baseURL := g.mcpBaseURL
	if baseURL == "" {
		baseURL = "http://127.0.0.1:3100"
	}
	mcpHandler := fasthttpadaptor.NewFastHTTPHandler(observemcp.NewHTTPHandler(baseURL, nil))
	g.app.All("/mcp", g.mcpRequired, func(c fiber.Ctx) error {
		mcpHandler(c.RequestCtx())
		return nil
	})
}

// registerAuthRoutes serves login and the current user's own account.
func (g *Gateway) registerAuthRoutes() {
	g.app.Post("/api/auth/login", g.login)
	g.app.Get("/api/auth/me", g.userRequired, g.me)
	// Login is the only cookie-less mutation here; every other /api/auth
	// mutation rides on the session cookie and needs the CSRF double-submit.
	g.app.Post("/api/auth/logout", g.sessionRequired, g.csrfRequired, g.logout)
	g.app.Get("/api/auth/tokens", g.sessionRequired, g.listCurrentUserTokens)
	g.app.Post("/api/auth/tokens", g.sessionRequired, g.csrfRequired, g.createCurrentUserToken)
	g.app.Delete("/api/auth/tokens/:tokenID", g.sessionRequired, g.csrfRequired, g.revokeCurrentUserToken)
	g.app.Post("/api/auth/password", g.sessionRequired, g.csrfRequired, g.changePassword)
	g.app.Put("/api/auth/me", g.sessionRequired, g.csrfRequired, g.updateMe)
	g.app.Get("/api/auth/sessions", g.sessionRequired, g.listMySessions)
	g.app.Delete("/api/auth/sessions/:id", g.sessionRequired, g.csrfRequired, g.deleteMySession)
	g.app.Get("/api/audit", g.userRequired, g.adminRequired, g.listAudit)
}

// registerTelemetryRoutes serves ingest (project tokens) and the project-scoped
// read API (sessions or user tokens via queryRequired).
func (g *Gateway) registerTelemetryRoutes() {
	g.app.Post("/v1/metrics", g.auth.Middleware, g.metricsIngest)
	g.app.Post("/api/v1/write", g.auth.Middleware, g.metricsRemoteWrite)
	g.app.Post(
		"/v1/logs",
		g.auth.Middleware,
		g.tenantProxy(g.storage.LogsIngestBase(), "/insert/opentelemetry/v1/logs"),
	)
	g.browserIngestRoute(
		"/v1/traces",
		g.tenantProxy(g.storage.TracesIngestBase(), "/insert/opentelemetry/v1/traces"),
	)
	g.browserIngestRoute("/api/events/errors", g.errorsIngest)
	g.browserIngestRoute("/api/events/vitals", g.vitalsIngest)
	g.browserIngestRoute("/api/events/diagnostics", g.sdkDiagnosticsIngest)
	g.app.Post("/api/releases/:release/sourcemaps", g.uploadAuth.UploadMiddleware, g.uploadSourceMaps)
	g.app.Get("/api/releases", g.queryRequired, g.listSourceMapReleases)
	g.browserIngestRoute("/api/replay/segments", g.replayIngest)
	g.app.Post("/api/profiles", g.auth.Middleware, g.profileIngest)
	g.app.Get("/api/profiles/services", g.queryRequired, g.listProfileServices)
	g.app.Get("/api/profiles/flamegraph", g.queryRequired, g.profileFlamegraph)
	g.app.Get("/api/profiles/:id/raw", g.queryRequired, g.rawProfile)
	g.app.Get("/api/profiles", g.queryRequired, g.listProfiles)
	g.app.Get("/api/replay/sessions", g.queryRequired, g.listReplaySessions)
	g.app.Get("/api/replay/sessions/:session_id", g.queryRequired, g.getReplaySession)
	g.app.Get("/api/replay/sessions/:session_id/segments/:seq", g.queryRequired, g.getReplaySegment)
	for _, path := range []string{
		"/api/v1/query", "/api/v1/query_range", "/api/v1/series", "/api/v1/labels",
		"/api/v1/label/:name/values",
	} {
		g.app.All(path, g.queryRequired, g.metricsQuery)
	}
	g.app.Get("/api/v1/prettify-query", g.queryRequired, g.metricsPrettify)
	for _, route := range logsProxyRoutes {
		handler := g.logsProxyHandler(route)
		g.app.Get(route.apiPath, g.queryRequired, handler)
		g.app.Post(route.apiPath, g.queryRequired, handler)
	}
	g.app.Get(
		"/api/traces/services",
		g.queryRequired,
		g.tenantProxy(g.storage.TracesQueryBase(), "/select/jaeger/api/services"),
	)
	g.app.Get("/api/traces/services/:svc/operations", g.queryRequired, g.traceOperations)
	g.app.Get("/api/traces/search", g.queryRequired, g.traceSearch)
	g.app.Get("/api/traces/:traceID", g.queryRequired, g.traceByID)
	g.app.Get("/api/llm/summary", g.queryRequired, g.llmSummary)
	g.app.Get("/api/llm/calls", g.queryRequired, g.llmCalls)
	g.app.Get("/api/llm/timeseries", g.queryRequired, g.llmTimeseries)
	g.app.Get("/api/v1/performance/endpoints", g.queryRequired, g.performanceEndpoints)
	// Token-authenticated, read-only query aliases used by MCP. They deliberately
	// reuse the same handlers as the UI and therefore retain project isolation.
	g.app.Get("/api/v1/issues", g.queryRequired, g.listIssues)
	g.app.Get("/api/v1/issues/:id", g.queryRequired, g.getIssue)
	g.app.Get("/api/v1/issues/:id/events/:event_id", g.queryRequired, g.getIssueEvent)
	g.app.Get("/api/v1/alerts", g.queryRequired, g.listAlerts)
	g.app.Get("/api/v1/hosts", g.queryRequired, g.hosts)
}

// registerSessionAPI serves the web UI's management API under /api, behind a
// user credential and the CSRF check.
func (g *Gateway) registerSessionAPI() {
	api := g.app.Group("/api", g.userRequired, g.csrfRequired)
	api.Get("/ai/status", g.aiStatus)
	api.Post("/ai/test", g.adminRequired, g.aiTest)
	api.Get("/ai/settings", g.adminRequired, g.aiSettingsGet)
	api.Put("/ai/settings", g.adminRequired, g.aiSettingsPut)
	api.Post("/ai/assistant/query", g.loadSessionProject, g.aiAssistantQuery)
	api.Post("/ai/assistant/explain", g.aiAssistantExplain)
	api.Get("/projects", g.listProjects)
	api.Get("/projects/:id", g.getProject)
	api.Get("/projects/:id/alert-config", g.getProjectAlertConfig)
	api.Put("/projects/:id/alert-config", g.memberRequired, g.putProjectAlertConfig)
	api.Get("/projects/:id/ai-config", g.getProjectAIConfig)
	api.Put("/projects/:id/ai-config", g.memberRequired, g.putProjectAIConfig)
	api.Post("/projects", g.adminRequired, g.createProject)
	api.Patch("/projects/:id", g.adminRequired, g.updateProject)
	api.Put("/projects/:id", g.adminRequired, g.updateProject)
	api.Delete("/projects/:id", g.adminRequired, g.deleteProject)
	api.Get("/projects/:id/tokens", g.listProjectAPITokens)
	api.Post("/projects/:id/tokens", g.adminRequired, g.createProjectAPIToken)
	api.Delete("/projects/:id/tokens/:tokenID", g.adminRequired, g.revokeProjectAPIToken)
	api.Get("/projects/:id/source-map-upload-tokens", g.adminRequired, g.listSourceMapUploadTokens)
	api.Post("/projects/:id/source-map-upload-tokens", g.adminRequired, g.createSourceMapUploadToken)
	api.Delete(
		"/projects/:id/source-map-upload-tokens/:tokenID",
		g.adminRequired,
		g.revokeSourceMapUploadToken,
	)
	api.Get("/users", g.adminRequired, g.listUsers)
	api.Get("/users/:id", g.adminRequired, g.getUser)
	api.Post("/users", g.adminRequired, g.createUser)
	api.Patch("/users/:id", g.adminRequired, g.updateUser)
	api.Put("/users/:id", g.adminRequired, g.updateUser)
	api.Delete("/users/:id", g.adminRequired, g.deleteUser)
	api.Get("/alert-rules", g.memberRequired, g.loadSessionProject, g.listAlertRules)
	api.Post("/alert-rules", g.memberRequired, g.loadSessionProject, g.createAlertRule)
	api.Patch("/alert-rules/:id", g.memberRequired, g.loadSessionProject, g.updateAlertRule)
	api.Put("/alert-rules/:id", g.memberRequired, g.loadSessionProject, g.updateAlertRule)
	api.Delete("/alert-rules/:id", g.memberRequired, g.loadSessionProject, g.deleteAlertRule)
	api.Get("/notifications", g.loadSessionProject, g.memberRequired, g.listNotifications)
	api.Post("/notifications/:id/retry", g.loadSessionProject, g.memberRequired, g.requeueNotification)
	api.Get("/channels", g.memberRequired, g.listChannels)
	api.Post("/channels", g.adminRequired, g.createChannel)
	api.Patch("/channels/:id", g.adminRequired, g.updateChannel)
	api.Put("/channels/:id", g.adminRequired, g.updateChannel)
	api.Delete("/channels/:id", g.adminRequired, g.deleteChannel)
	api.Post("/channels/:id/test", g.adminRequired, g.testChannel)
	api.Get("/silences", g.memberRequired, g.loadSessionProject, g.listSilences)
	api.Post("/silences", g.memberRequired, g.loadSessionProject, g.createSilence)
	api.Patch("/silences/:id", g.memberRequired, g.loadSessionProject, g.updateSilence)
	api.Put("/silences/:id", g.memberRequired, g.loadSessionProject, g.updateSilence)
	api.Delete("/silences/:id", g.memberRequired, g.loadSessionProject, g.deleteSilence)
	api.Get("/inhibition-rules", g.memberRequired, g.loadSessionProject, g.listInhibitionRules)
	api.Post("/inhibition-rules", g.memberRequired, g.loadSessionProject, g.createInhibitionRule)
	api.Patch("/inhibition-rules/:id", g.memberRequired, g.loadSessionProject, g.updateInhibitionRule)
	api.Put("/inhibition-rules/:id", g.memberRequired, g.loadSessionProject, g.updateInhibitionRule)
	api.Delete("/inhibition-rules/:id", g.memberRequired, g.loadSessionProject, g.deleteInhibitionRule)
	api.Get("/alerts", g.loadSessionProject, g.listAlerts)
	api.Get("/scrape-targets", g.loadSessionProject, g.listScrapeTargets)
	api.Post("/scrape-targets", g.loadSessionProject, g.memberRequired, g.createScrapeTarget)
	api.Get("/scrape-targets/:id", g.loadSessionProject, g.getScrapeTarget)
	api.Patch("/scrape-targets/:id", g.loadSessionProject, g.memberRequired, g.updateScrapeTarget)
	api.Put("/scrape-targets/:id", g.loadSessionProject, g.memberRequired, g.updateScrapeTarget)
	api.Delete("/scrape-targets/:id", g.loadSessionProject, g.memberRequired, g.deleteScrapeTarget)
	api.Get("/scrape-targets/:id/health", g.loadSessionProject, g.scrapeTargetHealth)
	api.Get("/hosts", g.loadSessionProject, g.hosts)
	api.Get("/issues", g.loadSessionProject, g.listIssues)
	api.Get("/issues/:id", g.loadSessionProject, g.getIssue)
	api.Get("/issues/:id/ai-analysis", g.loadSessionProject, g.getIssueAIAnalysis)
	api.Post(
		"/issues/:id/ai-analysis",
		g.loadSessionProject,
		g.memberRequired,
		g.postIssueAIAnalysis,
	)
	api.Patch("/issues/:id", g.loadSessionProject, g.memberRequired, g.patchIssue)
	api.Post("/issues/batch", g.loadSessionProject, g.memberRequired, g.batchUpdateIssues)
	api.Get("/issues/:id/events/:event_id", g.loadSessionProject, g.getIssueEvent)
	api.Delete(
		"/releases/:release/sourcemaps",
		g.loadSessionProject,
		g.adminRequired,
		g.deleteSourceMaps,
	)
}

func (g *Gateway) browserIngestRoute(path string, handler fiber.Handler) {
	g.app.Options(path, browserIngestCORS)
	g.app.Post(path, browserIngestCORS, g.auth.Middleware, handler)
}

func (g *Gateway) serveWebUI(c fiber.Ctx) error {
	if g.webUI == nil || (c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead) {
		return c.SendStatus(fiber.StatusNotFound)
	}
	requestPath := c.Path()
	for _, prefix := range []string{"/api", "/v1", "/mcp", "/healthz", "/livez", "/readyz", "/select"} {
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

// mcpRequired authenticates MCP with a user API token and the project selected
// by X-LO-Project. MCP only exposes reads, so project (ingest) tokens are
// refused like on every other query route.
func (g *Gateway) mcpRequired(c fiber.Ctx) error {
	token := bearerCredential(c)
	if token == "" {
		return unauthorizedResponse(c)
	}
	if !strings.HasPrefix(token, "lou_") {
		return nonQueryToken(c, token)
	}
	return g.queryRequired(c)
}

// ingestOnlyToken rejects a project API token on a read route. Project tokens
// are embedded in browser pages and SDK configuration, so they are ingest-only.
func ingestOnlyToken(c fiber.Ctx) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  "ingest_only_token",
		"error": "project API tokens can only ingest telemetry; use a session or a user API token to query",
	})
}

// uploadOnlyToken rejects a source map upload token anywhere but the upload route
// (ADR 0018). It is confidential but narrowly scoped: it can neither ingest nor read.
func uploadOnlyToken(c fiber.Ctx) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  "upload_only_token",
		"error": "source map upload tokens can only upload source maps to their project",
	})
}

// sourceMapUploadTokenRequired rejects a credential that is not an upload token on
// the upload route: the project API token is public (ADR 0010) and must not be
// able to overwrite a release's source maps (ADR 0018).
func sourceMapUploadTokenRequired(c fiber.Ctx) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  "source_map_upload_token_required",
		"error": "source map upload requires a source map upload token (los_...); create one in the project settings",
	})
}

// nonQueryToken answers a bearer credential that cannot query: an upload token or a
// project API token.
func nonQueryToken(c fiber.Ctx, token string) error {
	if strings.HasPrefix(token, meta.SourceMapUploadTokenPrefix) {
		return uploadOnlyToken(c)
	}
	return ingestOnlyToken(c)
}

func unauthorizedResponse(c fiber.Ctx) error {
	_ = unauthorized(c)
	return nil
}

func (g *Gateway) loadSessionProject(c fiber.Ctx) error {
	return g.resolveRequestProject(c)
}

func requestedProjectSlug(c fiber.Ctx) string {
	slug := strings.TrimSpace(c.Get("X-LO-Project"))
	if slug == "" {
		slug = "default"
	}
	return slug
}

func (g *Gateway) resolveRequestProject(c fiber.Ctx) error {
	if g.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "project store unavailable"})
	}
	slug := requestedProjectSlug(c)
	p, err := g.store.ProjectBySlug(c.Context(), slug)
	if err != nil {
		if meta.IsNotFound(err) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"code":  "project_not_found",
				"error": "project not found",
			})
		}
		g.logger.Warn("project lookup failed", "project_slug", slug, "error", err)
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "project lookup temporarily unavailable"})
	}
	if p.ID <= 0 {
		return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
	}
	c.Locals(projectLocal, p)
	return c.Next()
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
	return g.metricsWrite(c, "/opentelemetry/v1/metrics", true)
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
	return g.metricsWriteWithBody(c, "/prometheus/api/v1/write", false, body)
}

func (g *Gateway) queryRequired(c fiber.Ctx) error {
	if token := bearerCredential(c); strings.HasPrefix(token, "lou_") {
		u, err := g.loadUserAPIToken(c, token)
		if err != nil {
			if errors.Is(err, errResponseWritten) {
				return nil
			}
			return err
		}
		c.Locals(userLocal, u)
		return g.loadQueryProject(c)
	}
	if token := bearerCredential(c); token != "" {
		return nonQueryToken(c, token)
	}
	if err := g.loadSession(c); err != nil {
		if errors.Is(err, errResponseWritten) {
			return nil
		}
		if account.Code(err) == account.CodeUnavailable {
			_ = c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "authentication temporarily unavailable"})
			return nil
		}
		_ = unauthorized(c)
		return nil
	}
	return g.loadQueryProject(c)
}

func (g *Gateway) loadQueryProject(c fiber.Ctx) error {
	return g.resolveRequestProject(c)
}

func bearerCredential(c fiber.Ctx) string {
	authorization := strings.TrimSpace(c.Get("Authorization"))
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return ""
	}
	return token
}

func (g *Gateway) sessionRequired(c fiber.Ctx) error {
	if strings.TrimSpace(c.Cookies("lo_session")) == "" {
		_ = unauthorized(c)
		return nil
	}
	if err := g.loadSession(c); err != nil {
		if errors.Is(err, errResponseWritten) {
			return nil
		}
		if account.Code(err) == account.CodeUnavailable {
			_ = c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "authentication temporarily unavailable"})
			return nil
		}
		_ = unauthorized(c)
		return nil
	}
	return c.Next()
}

func (g *Gateway) userRequired(c fiber.Ctx) error {
	if token := bearerCredential(c); token != "" {
		if !strings.HasPrefix(token, "lou_") {
			// ADR 0010: project API tokens are ingest-only; refuse them on the
			// session API exactly like on the telemetry query routes.
			return nonQueryToken(c, token)
		}
		u, err := g.loadUserAPIToken(c, token)
		if err != nil {
			if errors.Is(err, errResponseWritten) {
				return nil
			}
			return err
		}
		c.Locals(userLocal, u)
		return c.Next()
	}
	return g.sessionRequired(c)
}

func (g *Gateway) loadUserAPIToken(c fiber.Ctx, token string) (meta.UserInfo, error) {
	u, err := g.account.AuthenticateUserToken(c.Context(), token)
	if err != nil {
		if account.Code(err) == account.CodeInvalidCredentials {
			return meta.UserInfo{}, unauthorized(c)
		}
		_ = c.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "authentication temporarily unavailable"})
		return meta.UserInfo{}, errResponseWritten
	}
	if passwordChangeBlocks(c, u) {
		return meta.UserInfo{}, errResponseWritten
	}
	return accountUserInfo(u), nil
}

func (g *Gateway) loadSession(c fiber.Ctx) error {
	token := strings.TrimSpace(c.Cookies("lo_session"))
	if token == "" {
		return &account.Error{Code: account.CodeInvalidCredentials}
	}
	u, err := g.account.AuthenticateSession(c.Context(), token)
	if err != nil {
		return err
	}
	if passwordChangeBlocks(c, u) {
		return errResponseWritten
	}
	c.Locals(userLocal, accountUserInfo(u))
	return nil
}

// passwordChangeRoutes are the only routes a user flagged with
// must_change_password may reach, whether through a session or a user API
// token: read the current user, change the own password, and log out.
var passwordChangeRoutes = map[string]struct{}{
	fiber.MethodGet + " /api/auth/me":        {},
	fiber.MethodPost + " /api/auth/password": {},
	fiber.MethodPost + " /api/auth/logout":   {},
}

// passwordChangeBlocks writes 403 password_change_required and reports true
// when a flagged user calls anything outside passwordChangeRoutes. The flag is
// read from metadata on every request (ADR 0006), so a password change takes
// effect immediately for every session adapter and replica.
func passwordChangeBlocks(c fiber.Ctx, user account.User) bool {
	if !user.MustChangePassword {
		return false
	}
	if _, allowed := passwordChangeRoutes[c.Method()+" "+c.Route().Path]; allowed {
		return false
	}
	_ = c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"code":  string(account.CodePasswordChangeRequired),
		"error": "change the initial password before using the API",
	})
	return true
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

func (g *Gateway) adminRequired(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	if u.Role != "admin" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
	}
	return c.Next()
}

func (g *Gateway) memberRequired(c fiber.Ctx) error {
	u, _ := c.Locals(userLocal).(meta.UserInfo)
	if u.Role != "admin" && u.Role != "member" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
	}
	return c.Next()
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

type cachedProject struct {
	project   meta.ProjectInfo
	expiresAt time.Time
}

var errAuthenticationRevisionChanged = errors.New("authentication store revision changed")

const defaultAuthLookupTimeout = 5 * time.Second

type authenticationCall struct {
	done     chan struct{}
	revision uint64
	project  meta.ProjectInfo
	err      error
	panicked bool
	panicVal any
}

// Authenticator resolves a bearer credential to a project through one lookup. Two
// instances exist: ingest (project API tokens) and source map upload (ADR 0018);
// each only ever consults its own token table.
type Authenticator struct {
	revisionFn    func() uint64
	lookup        func(context.Context, string) (meta.ProjectInfo, error)
	invalid       error
	configured    bool
	lookupTimeout time.Duration
	mu            sync.RWMutex
	cache         map[string]cachedProject
	inflight      map[string]*authenticationCall
	revision      uint64
}

func NewAuthenticator(store TokenStore) *Authenticator {
	a := newAuthenticator()
	if store != nil {
		a.revisionFn = store.Revision
		a.lookup = store.AuthenticateProjectAPIToken
		a.invalid = meta.ErrInvalidProjectAPIToken
		a.configured = true
	}
	return a
}

func newUploadAuthenticator(store UploadTokenStore) *Authenticator {
	a := newAuthenticator()
	if store != nil {
		a.revisionFn = store.Revision
		a.lookup = store.AuthenticateSourceMapUploadToken
		a.invalid = meta.ErrInvalidSourceMapUploadToken
		a.configured = true
	}
	return a
}

func newAuthenticator() *Authenticator {
	return &Authenticator{
		lookupTimeout: defaultAuthLookupTimeout,
		cache:         make(map[string]cachedProject),
		inflight:      make(map[string]*authenticationCall),
	}
}

// Middleware authenticates ingest routes with a project API token. A source map
// upload token is refused explicitly: it is a different credential kind.
func (a *Authenticator) Middleware(c fiber.Ctx) error {
	token := bearerCredential(c)
	if strings.HasPrefix(token, meta.SourceMapUploadTokenPrefix) {
		return uploadOnlyToken(c)
	}
	return a.authenticateRequest(c, token)
}

// UploadMiddleware authenticates the source map upload route. Only a source map
// upload token is accepted; project API tokens and user tokens get 403 with
// source_map_upload_token_required, anything else 401.
func (a *Authenticator) UploadMiddleware(c fiber.Ctx) error {
	token := bearerCredential(c)
	if token != "" && !strings.HasPrefix(token, meta.SourceMapUploadTokenPrefix) &&
		(strings.HasPrefix(token, "lo_") || strings.HasPrefix(token, "lou_")) {
		return sourceMapUploadTokenRequired(c)
	}
	return a.authenticateRequest(c, token)
}

func (a *Authenticator) authenticateRequest(c fiber.Ctx, token string) error {
	if token == "" || !a.configured {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	p, err := a.authenticate(c.Context(), token)
	if err != nil {
		if !errors.Is(err, a.invalid) && !meta.IsNotFound(err) {
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "authentication temporarily unavailable"})
		}
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	c.Locals(projectLocal, p)
	return c.Next()
}

func (a *Authenticator) authenticate(ctx context.Context, token string) (meta.ProjectInfo, error) {
	for {
		a.mu.Lock()
		revision := a.revisionFn()
		if revision != a.revision {
			clear(a.cache)
			a.revision = revision
		}
		if cached, ok := a.cache[token]; ok && time.Now().Before(cached.expiresAt) {
			a.mu.Unlock()
			return cached.project, nil
		}
		delete(a.cache, token)
		if call, ok := a.inflight[token]; ok {
			done := call.done
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return meta.ProjectInfo{}, ctx.Err()
			case <-done:
			}
			if errors.Is(call.err, errAuthenticationRevisionChanged) {
				continue
			}
			return call.project, call.err
		}

		// The caller's token may borrow Fiber's reusable request buffer, which
		// the next request on the same context overwrites. A borrowed map key
		// would then silently become another request's token: a different
		// project's token, or one that was never issued, would match this
		// entry and authenticate as this project. Store only an owned copy.
		// Lookups above may use the borrowed value; only stored keys must own
		// their bytes.
		key := strings.Clone(token)
		call := &authenticationCall{done: make(chan struct{}), revision: revision}
		a.inflight[key] = call
		a.mu.Unlock()

		// Decouple upstream lookup from the leader's cancellation so a single
		// client disconnect cannot fail concurrent waiters sharing this call,
		// while bounding the lookup with an independent timeout.
		timeout := a.lookupTimeout
		if timeout <= 0 {
			timeout = defaultAuthLookupTimeout
		}
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		go a.runLookup(lookupCtx, cancel, key, call)

		select {
		case <-ctx.Done():
			return meta.ProjectInfo{}, ctx.Err()
		case <-call.done:
		}
		if call.panicked {
			panic(call.panicVal)
		}
		if errors.Is(call.err, errAuthenticationRevisionChanged) {
			continue
		}
		return call.project, call.err
	}
}

func (a *Authenticator) runLookup(
	ctx context.Context,
	cancel context.CancelFunc,
	key string,
	call *authenticationCall,
) {
	defer cancel()
	completed := false
	defer func() {
		r := recover()
		if !completed {
			a.mu.Lock()
			if r != nil {
				call.panicked = true
				call.panicVal = r
				call.err = fmt.Errorf("authentication lookup panic: %v", r)
			} else {
				call.err = errors.New("authentication lookup aborted")
			}
			if a.inflight[key] == call {
				delete(a.inflight, key)
			}
			close(call.done)
			a.mu.Unlock()
		}
	}()

	project, authErr := a.lookup(ctx, key)
	a.mu.Lock()
	latestRevision := a.revisionFn()
	if latestRevision != a.revision {
		clear(a.cache)
		a.revision = latestRevision
	}
	if latestRevision != call.revision {
		call.err = errAuthenticationRevisionChanged
	} else {
		call.project, call.err = project, authErr
		if authErr == nil {
			a.cache[key] = cachedProject{
				project:   project,
				expiresAt: time.Now().Add(tokenCacheTTL),
			}
		}
	}
	if a.inflight[key] == call {
		delete(a.inflight, key)
	}
	completed = true
	close(call.done)
	a.mu.Unlock()
}
