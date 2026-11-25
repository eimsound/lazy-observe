// Package config loads the server configuration with flag > environment > file > default precedence.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d Duration) Value() time.Duration { return time.Duration(d) }
func (d Duration) String() string       { return time.Duration(d).String() }
func (d Duration) Type() string         { return "duration" }
func (d *Duration) Set(value string) error {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid day duration %q: %w", value, err)
		}
		*d = Duration(time.Duration(days) * 24 * time.Hour)
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
func (d *Duration) UnmarshalYAML(node *yaml.Node) error { return d.Set(node.Value) }

const (
	TopologyAllInOne   = "all-in-one"
	TopologyStandalone = "standalone"
	TopologyHA         = "ha"

	ScrapeModeAuto     = "auto"
	ScrapeModeDisabled = "disabled"
)

type Config struct {
	Listen          string          `yaml:"listen"`
	OTLPGRPC        OTLPGRPC        `yaml:"otlp_grpc"`
	IngestRateLimit IngestRateLimit `yaml:"ingest_rate_limit"`
	MetricsEndpoint MetricsEndpoint `yaml:"metrics_endpoint"`
	DataDir         string          `yaml:"data_dir"`
	ServeWebUI      bool            `yaml:"serve_web_ui"`
	ExternalURL     string          `yaml:"external_url"`
	Topology        string          `yaml:"topology"`
	Standalone      bool            `yaml:"standalone"`
	StorageBinDir   string          `yaml:"storage_bin_dir"`
	Storage         Storage         `yaml:"storage"`
	Meta            Meta            `yaml:"meta"`
	Redis           Redis           `yaml:"redis"`
	Retention       Retention       `yaml:"retention"`
	Replay          Replay          `yaml:"replay"`
	Blob            Blob            `yaml:"blob"`
	Alert           Alert           `yaml:"alert"`
	Scrape          Scrape          `yaml:"scrape"`
	AI              AI              `yaml:"ai"`
	ErrorInbox      ErrorInbox      `yaml:"error_inbox"`
	Process         Process         `yaml:"process"`
	Security        Security        `yaml:"security"`
	SelfMonitoring  SelfMonitoring  `yaml:"self_monitoring"`
}

// OTLPGRPC configures the native OTLP/gRPC ingest listener (Traces, Metrics,
// and Logs on default port :4317). Disabled by default so existing deployments
// and multi-instance test fixtures do not collide on :4317 until enabled.
type OTLPGRPC struct {
	Enabled      bool   `yaml:"enabled"`
	Listen       string `yaml:"listen"`
	MaxRecvBytes int    `yaml:"max_recv_bytes"`
}

// IngestRateLimit configures unified per-project token-bucket rate limiting
// across HTTP and OTLP/gRPC telemetry ingest endpoints.
type IngestRateLimit struct {
	Enabled            bool    `yaml:"enabled"`
	RequestsPerSecond  float64 `yaml:"requests_per_second"`
	Burst              int     `yaml:"burst"`
	BytesPerSecond     int64   `yaml:"bytes_per_second"`
	MaxTrackedProjects int     `yaml:"max_tracked_projects"`
}

// MetricsEndpoint configures the Prometheus-format self-observability /metrics
// endpoint on the HTTP gateway.
type MetricsEndpoint struct {
	// Enabled exposes GET /metrics in Prometheus text exposition format 0.0.4.
	Enabled bool `yaml:"enabled"`
	// BearerToken optionally authenticates scrapers using a dedicated
	// Authorization: Bearer <token> credential (checked in constant time).
	// When empty, GET /metrics requires an admin session or user API token
	// unless AllowUnauthenticated is explicitly true.
	BearerToken string `yaml:"bearer_token"`
	// AllowUnauthenticated allows scraping GET /metrics without credentials.
	// Cannot be combined with BearerToken.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated"`
}

// Security groups browser-facing hardening controls.
type Security struct {
	// CSRF enables the double-submit CSRF token check for session-authenticated
	// mutating requests. Enabled by default.
	CSRF bool `yaml:"csrf"`
	// SessionSecureCookie marks session and CSRF cookies Secure. Automatically
	// enabled when native TLS (tls_cert and tls_key) is configured; set this
	// explicitly when TLS terminates at a reverse proxy in front of the gateway.
	SessionSecureCookie bool `yaml:"session_secure_cookie"`
	// TrustedProxies lists explicit reverse proxy IP addresses or CIDRs whose
	// X-Forwarded-For headers are trusted to identify client IPs for rate
	// limiting. Empty disables proxy header trust (direct peer IP only).
	TrustedProxies []string `yaml:"trusted_proxies"`
	// AuditRetention bounds how long management audit events are kept.
	AuditRetention Duration `yaml:"audit_retention"`
	// TLSCert and TLSKey enable gateway-side TLS termination. Both must be
	// set together; empty values keep plain HTTP.
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// SecretKeyFile is the platform encryption key file used to seal sensitive
	// values in the metadata database (BYO LLM API keys, future secrets). On
	// first startup a fresh 256-bit key is generated and persisted with mode
	// 0600. Empty uses <data_dir>/secret.key. HA replicas must share one file;
	// an existing file (for example a read-only Kubernetes Secret mount with
	// `openssl rand -base64 32` output) is only read, never rewritten.
	SecretKeyFile string `yaml:"secret_key_file"`
	// AdminPasswordFile supplies the first admin password when the admin
	// account does not exist yet; surrounding whitespace is ignored and at
	// least 8 characters are required. Empty generates a random password and
	// writes it to <data_dir>/initial-admin-password (mode 0600) on the replica
	// that creates the admin. HA deployments should set this.
	AdminPasswordFile string `yaml:"admin_password_file"`
	// OutboundAllowlist authorizes explicit private/loopback CIDRs, hosts, or
	// host:port endpoints (such as local Ollama 127.0.0.1:11434 or internal
	// webhooks) for outbound notification and BYO AI requests.
	OutboundAllowlist []string `yaml:"outbound_allowlist"`
}

// SelfMonitoring controls the platform's use of its own Go SDK.
type SelfMonitoring struct {
	Enabled   bool `yaml:"enabled"`
	Profiling bool `yaml:"profiling"`
}

type Meta struct {
	DBURL                string   `yaml:"db_url"`
	MigrationLockTimeout Duration `yaml:"migration_lock_timeout"`
}

type Redis struct {
	Enabled   bool   `yaml:"enabled"`
	Addr      string `yaml:"addr"`
	Password  string `yaml:"password"`
	DB        int    `yaml:"db"`
	KeyPrefix string `yaml:"key_prefix"`
}

type Storage struct {
	Metrics Metrics  `yaml:"metrics"`
	Logs    Endpoint `yaml:"logs"`
	Traces  Endpoint `yaml:"traces"`
}
type Metrics struct {
	Kind            string `yaml:"kind"`
	Cluster         bool   `yaml:"cluster"`
	TenantAccountID string `yaml:"tenant_account_id"`
	QueryURL        string `yaml:"query_url"`
	IngestURL       string `yaml:"ingest_url"`
}
type Endpoint struct {
	URL       string `yaml:"url"`
	QueryURL  string `yaml:"query_url"`
	IngestURL string `yaml:"ingest_url"`
}

func (e Endpoint) EffectiveQueryURL() string {
	if u := strings.TrimSpace(e.QueryURL); u != "" {
		return u
	}
	return strings.TrimSpace(e.URL)
}

func (e Endpoint) EffectiveIngestURL() string {
	if u := strings.TrimSpace(e.IngestURL); u != "" {
		return u
	}
	return strings.TrimSpace(e.URL)
}

type Retention struct {
	Metrics      Duration `yaml:"metrics"`
	Logs         Duration `yaml:"logs"`
	Traces       Duration `yaml:"traces"`
	Replay       Duration `yaml:"replay"`
	SourceMaps   Duration `yaml:"sourcemaps"`
	AlertEvents  Duration `yaml:"alert_events"`
	Profiles     Duration `yaml:"profiles"`
	IssuesClosed bool     `yaml:"issues_closed"`
}
type Replay struct {
	CleanupInterval Duration `yaml:"cleanup_interval"`
}
type Blob struct {
	Backend string `yaml:"backend"`
	S3      S3     `yaml:"s3"`
}
type S3 struct {
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
}
type Alert struct {
	StormThreshold int      `yaml:"storm_threshold"`
	GroupWait      Duration `yaml:"group_wait"`
	RepeatInterval Duration `yaml:"repeat_interval"`
}

// Scrape controls embedded vmagent target scraping behavior.
type Scrape struct {
	// Mode is "auto" (run embedded vmagent, using PostgreSQL advisory-lock
	// leader election in clustered/HA deployments) or "disabled" (never extract
	// or run vmagent; external scrapers own target collection).
	Mode string `yaml:"mode"`
}

// AI groups static LLM observability configuration. BYO AI provider credentials
// remain managed exclusively through the admin UI.
type AI struct {
	PricingFile string `yaml:"pricing_file"`
}

type ErrorInbox struct {
	MaxItems       int64 `yaml:"max_items"`
	MaxBytes       int64 `yaml:"max_bytes"`
	ReadinessItems int64 `yaml:"readiness_items"`
	ReadinessBytes int64 `yaml:"readiness_bytes"`
}

// Process groups runtime-tuning knobs for the embedded storage supervisor.
type Process struct {
	StartupTimeout      Duration `yaml:"startup_timeout"`
	ShutdownTimeout     Duration `yaml:"shutdown_timeout"`
	RestartInitial      Duration `yaml:"restart_initial"`
	RestartMax          Duration `yaml:"restart_max"`
	DrainTimeout        Duration `yaml:"drain_timeout"`
	ComponentPortOffset int      `yaml:"component_port_offset"`
}

func Defaults() Config {
	return Config{
		Listen: ":3100", DataDir: "./data", ServeWebUI: true,
		OTLPGRPC: OTLPGRPC{
			Enabled:      false,
			Listen:       ":4317",
			MaxRecvBytes: 52 << 20,
		},
		IngestRateLimit: IngestRateLimit{
			Enabled:            false,
			RequestsPerSecond:  ingestlimit.DefaultRequestsPerSecond,
			Burst:              ingestlimit.DefaultBurst,
			BytesPerSecond:     0,
			MaxTrackedProjects: ingestlimit.DefaultMaxTrackedProjects,
		},
		MetricsEndpoint: MetricsEndpoint{
			Enabled:              false,
			BearerToken:          "",
			AllowUnauthenticated: false,
		},
		Meta:    Meta{MigrationLockTimeout: Duration(time.Minute)},
		Redis:   Redis{Addr: "127.0.0.1:6379", KeyPrefix: "lazy-observe"},
		Storage: Storage{Metrics: Metrics{Kind: "vm"}},
		Retention: Retention{
			Metrics: Duration(
				90 * 24 * time.Hour,
			), Logs: Duration(14 * 24 * time.Hour), Traces: Duration(7 * 24 * time.Hour),
			Replay: Duration(
				3 * 24 * time.Hour,
			), SourceMaps: Duration(90 * 24 * time.Hour), AlertEvents: Duration(30 * 24 * time.Hour), Profiles: Duration(7 * 24 * time.Hour), IssuesClosed: true,
		},
		Replay: Replay{CleanupInterval: Duration(time.Hour)},
		Blob:   Blob{Backend: "local", S3: S3{Region: "us-east-1"}},
		Alert: Alert{
			StormThreshold: 10,
			GroupWait:      Duration(30 * time.Second),
			RepeatInterval: Duration(4 * time.Hour),
		},
		Scrape: Scrape{Mode: ScrapeModeAuto},
		ErrorInbox: ErrorInbox{
			MaxItems: 100_000, MaxBytes: 1 << 30, ReadinessItems: 80_000, ReadinessBytes: 800 << 20,
		},
		Process: Process{
			StartupTimeout:  Duration(20 * time.Second),
			ShutdownTimeout: Duration(15 * time.Second),
			RestartInitial:  Duration(time.Second),
			RestartMax:      Duration(time.Minute),
			DrainTimeout:    Duration(20 * time.Second),
		},
		SelfMonitoring: SelfMonitoring{Enabled: true},
		Security: Security{
			CSRF:           true,
			AuditRetention: Duration(90 * 24 * time.Hour),
		},
	}
}

// Load parses server arguments. lookupEnv is injectable so tests don't mutate the process environment.
func Load(args []string, lookupEnv func(string) (string, bool)) (Config, string, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	cfg := Defaults()
	configPath, err := configFlag(args)
	if err != nil {
		return Config{}, "", err
	}
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return Config{}, configPath, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, configPath, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := applyEnv(&cfg, lookupEnv); err != nil {
		return Config{}, configPath, err
	}
	fs := pflag.NewFlagSet("server", pflag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bindFlags(fs, &cfg, &configPath)
	if err := fs.Parse(args); err != nil {
		return Config{}, configPath, err
	}
	if fs.NArg() != 0 {
		return Config{}, configPath, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, configPath, err
	}
	return cfg, configPath, nil
}

// BindFlags adds all server configuration flags to a Cobra/pflag command. The
// bound values are intentionally used only for parsing and help output;
// LoadFlagSet reapplies only explicitly changed flags after YAML and environment
// configuration have been loaded.
func BindFlags(fs *pflag.FlagSet) {
	cfg := Defaults()
	configPath := ""
	bindFlags(fs, &cfg, &configPath)
}

// LoadFlagSet loads configuration using the flags explicitly set on a
// Cobra/pflag command while preserving flag > environment > file > default
// precedence.
func LoadFlagSet(fs *pflag.FlagSet, lookupEnv func(string) (string, bool)) (Config, string, error) {
	args := make([]string, 0, fs.NFlag())
	fs.VisitAll(func(flag *pflag.Flag) {
		// Cobra merges the configuration FlagSet into the command FlagSet.
		// The shared flag records Changed, while the source FlagSet's internal
		// "actual" map is not populated; Visit would therefore silently drop
		// every CLI override after AddFlagSet.
		if flag.Changed {
			args = append(args, "--"+flag.Name+"="+flag.Value.String())
		}
	})
	return Load(args, lookupEnv)
}

func bindFlags(fs *pflag.FlagSet, cfg *Config, configPath *string) {
	fs.StringVar(configPath, "config", *configPath, "configuration YAML file")
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "public gateway listen address")
	fs.BoolVar(
		&cfg.OTLPGRPC.Enabled,
		"otlp-grpc-enabled",
		cfg.OTLPGRPC.Enabled,
		"enable the native OTLP/gRPC ingest server (traces, metrics, logs)",
	)
	fs.StringVar(
		&cfg.OTLPGRPC.Listen,
		"otlp-grpc-listen",
		cfg.OTLPGRPC.Listen,
		"OTLP/gRPC ingest listen address",
	)
	fs.IntVar(
		&cfg.OTLPGRPC.MaxRecvBytes,
		"otlp-grpc-max-recv-bytes",
		cfg.OTLPGRPC.MaxRecvBytes,
		"maximum decoded OTLP/gRPC request message size in bytes",
	)
	fs.BoolVar(
		&cfg.IngestRateLimit.Enabled,
		"ingest-rate-limit-enabled",
		cfg.IngestRateLimit.Enabled,
		"enable unified per-project token-bucket ingest rate limiting across HTTP and OTLP/gRPC",
	)
	fs.Float64Var(
		&cfg.IngestRateLimit.RequestsPerSecond,
		"ingest-rate-limit-requests-per-second",
		cfg.IngestRateLimit.RequestsPerSecond,
		"steady-state per-project ingest request rate limit (requests per second)",
	)
	fs.IntVar(
		&cfg.IngestRateLimit.Burst,
		"ingest-rate-limit-burst",
		cfg.IngestRateLimit.Burst,
		"per-project ingest request token-bucket burst capacity",
	)
	fs.Int64Var(
		&cfg.IngestRateLimit.BytesPerSecond,
		"ingest-rate-limit-bytes-per-second",
		cfg.IngestRateLimit.BytesPerSecond,
		"optional per-project ingest byte rate limit in bytes per second (0 disables byte limiting)",
	)
	fs.IntVar(
		&cfg.IngestRateLimit.MaxTrackedProjects,
		"ingest-rate-limit-max-tracked-projects",
		cfg.IngestRateLimit.MaxTrackedProjects,
		"maximum number of projects tracked concurrently by the in-memory ingest rate limiter",
	)
	fs.BoolVar(
		&cfg.MetricsEndpoint.Enabled,
		"metrics-endpoint-enabled",
		cfg.MetricsEndpoint.Enabled,
		"expose the gateway Prometheus self-observability /metrics endpoint",
	)
	fs.StringVar(
		&cfg.MetricsEndpoint.BearerToken,
		"metrics-endpoint-bearer-token",
		cfg.MetricsEndpoint.BearerToken,
		"optional static Bearer token required to scrape GET /metrics",
	)
	fs.BoolVar(
		&cfg.MetricsEndpoint.AllowUnauthenticated,
		"metrics-endpoint-allow-unauthenticated",
		cfg.MetricsEndpoint.AllowUnauthenticated,
		"allow scraping GET /metrics without authentication when bearer_token is unset",
	)
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "root data directory")
	fs.BoolVar(&cfg.ServeWebUI, "serve-web-ui", cfg.ServeWebUI, "serve the embedded web UI from the gateway")
	fs.StringVar(
		&cfg.ExternalURL,
		"external-url",
		cfg.ExternalURL,
		"external console base URL used for links in alert notifications",
	)
	fs.StringVar(
		&cfg.Topology,
		"topology",
		cfg.Topology,
		"explicit deployment topology: all-in-one, standalone, or ha (empty derives from standalone/shared backends)",
	)
	fs.StringVar(
		&cfg.Meta.DBURL,
		"meta-db-url",
		cfg.Meta.DBURL,
		"metadata database URL (sqlite:// path or postgres:// URL)",
	)
	fs.Var(&cfg.Meta.MigrationLockTimeout, "meta-migration-lock-timeout", "PostgreSQL metadata migration lock wait")
	fs.BoolVar(&cfg.Redis.Enabled, "redis-enabled", cfg.Redis.Enabled, "store shared sessions and rate limits in Redis")
	fs.StringVar(&cfg.Redis.Addr, "redis-addr", cfg.Redis.Addr, "Redis host:port")
	fs.StringVar(&cfg.Redis.Password, "redis-password", cfg.Redis.Password, "Redis password")
	fs.IntVar(&cfg.Redis.DB, "redis-db", cfg.Redis.DB, "Redis database number")
	fs.StringVar(&cfg.Redis.KeyPrefix, "redis-key-prefix", cfg.Redis.KeyPrefix, "Redis key prefix")
	fs.BoolVar(&cfg.Standalone, "standalone", cfg.Standalone, "use external storage endpoints")
	fs.StringVar(
		&cfg.StorageBinDir,
		"storage-bin-dir",
		cfg.StorageBinDir,
		"directory containing storage and platform binaries",
	)
	fs.StringVar(&cfg.Blob.Backend, "blob-store", cfg.Blob.Backend, "blob backend: local or s3")
	fs.StringVar(&cfg.Blob.S3.Bucket, "s3-bucket", cfg.Blob.S3.Bucket, "S3 bucket")
	fs.StringVar(&cfg.Blob.S3.Region, "s3-region", cfg.Blob.S3.Region, "S3 region")
	fs.StringVar(&cfg.Blob.S3.Endpoint, "s3-endpoint", cfg.Blob.S3.Endpoint, "S3-compatible endpoint")
	fs.StringVar(&cfg.Blob.S3.AccessKey, "s3-access-key", cfg.Blob.S3.AccessKey, "S3 access key")
	fs.StringVar(&cfg.Blob.S3.SecretKey, "s3-secret-key", cfg.Blob.S3.SecretKey, "S3 secret key")
	fs.Var(&cfg.Retention.Replay, "retention-replay", "replay retention period")
	fs.Var(&cfg.Replay.CleanupInterval, "replay-cleanup-interval", "replay cleanup interval")
	fs.IntVar(&cfg.Alert.StormThreshold, "alert-storm-threshold", cfg.Alert.StormThreshold, "alert storm threshold")
	fs.StringVar(
		&cfg.Scrape.Mode,
		"scrape-mode",
		cfg.Scrape.Mode,
		"embedded vmagent scrape mode: auto or disabled",
	)
	fs.StringVar(
		&cfg.AI.PricingFile,
		"ai-pricing-file",
		cfg.AI.PricingFile,
		"optional JSON file overriding or extending built-in LLM token pricing",
	)
	fs.Int64Var(
		&cfg.ErrorInbox.MaxItems,
		"error-inbox-max-items",
		cfg.ErrorInbox.MaxItems,
		"durable error Inbox item capacity",
	)
	fs.Int64Var(
		&cfg.ErrorInbox.MaxBytes,
		"error-inbox-max-bytes",
		cfg.ErrorInbox.MaxBytes,
		"durable error Inbox byte capacity",
	)
	fs.Int64Var(
		&cfg.ErrorInbox.ReadinessItems,
		"error-inbox-readiness-items",
		cfg.ErrorInbox.ReadinessItems,
		"durable error Inbox item readiness threshold",
	)
	fs.Int64Var(
		&cfg.ErrorInbox.ReadinessBytes,
		"error-inbox-readiness-bytes",
		cfg.ErrorInbox.ReadinessBytes,
		"durable error Inbox byte readiness threshold",
	)
	fs.Var(&cfg.Process.StartupTimeout, "storage-startup-timeout", "per-component startup deadline")
	fs.Var(&cfg.Process.ShutdownTimeout, "storage-shutdown-timeout", "component shutdown deadline")
	fs.Var(&cfg.Process.RestartInitial, "storage-restart-initial", "managed component initial restart delay")
	fs.Var(&cfg.Process.RestartMax, "storage-restart-max", "managed component maximum restart delay")
	fs.Var(&cfg.Process.DrainTimeout, "drain-timeout", "gateway request drain deadline")
	fs.IntVar(
		&cfg.Process.ComponentPortOffset,
		"component-port-offset",
		cfg.Process.ComponentPortOffset,
		"offset for managed component ports (test/isolation use)",
	)
	fs.BoolVar(
		&cfg.SelfMonitoring.Enabled,
		"self-monitoring",
		cfg.SelfMonitoring.Enabled,
		"observe lazy-observe with its own SDK",
	)
	fs.BoolVar(
		&cfg.SelfMonitoring.Profiling,
		"self-monitoring-profiling",
		cfg.SelfMonitoring.Profiling,
		"enable continuous profiling for lazy-observe",
	)
	fs.Var(&cfg.Retention.Metrics, "metrics-retention", "embedded metrics retention")
	fs.Var(&cfg.Retention.Logs, "logs-retention", "embedded logs retention")
	fs.Var(&cfg.Retention.Traces, "traces-retention", "embedded traces retention")
	fs.Var(&cfg.Retention.SourceMaps, "sourcemaps-retention", "source map retention")
	fs.Var(&cfg.Retention.AlertEvents, "alert-events-retention", "alert event retention")
	fs.Var(&cfg.Retention.Profiles, "profiles-retention", "profile retention")
	fs.BoolVar(
		&cfg.Security.CSRF,
		"security-csrf",
		cfg.Security.CSRF,
		"enforce the double-submit CSRF token for browser sessions",
	)
	fs.BoolVar(
		&cfg.Security.SessionSecureCookie,
		"security-secure-cookie",
		cfg.Security.SessionSecureCookie,
		"mark session and CSRF cookies Secure (automatically enabled with native TLS)",
	)
	fs.StringSliceVar(
		&cfg.Security.TrustedProxies,
		"security-trusted-proxies",
		cfg.Security.TrustedProxies,
		"comma-separated reverse proxy IPs or CIDRs trusted for X-Forwarded-For",
	)
	fs.Var(&cfg.Security.AuditRetention, "audit-retention", "management audit event retention")
	fs.StringVar(&cfg.Security.TLSCert, "tls-cert", cfg.Security.TLSCert, "TLS certificate file (enables HTTPS)")
	fs.StringVar(&cfg.Security.TLSKey, "tls-key", cfg.Security.TLSKey, "TLS private key file")
	fs.StringVar(
		&cfg.Security.SecretKeyFile,
		"security-secret-key-file",
		cfg.Security.SecretKeyFile,
		"platform encryption key file path",
	)
	fs.StringVar(
		&cfg.Security.AdminPasswordFile,
		"security-admin-password-file",
		cfg.Security.AdminPasswordFile,
		"initial admin password file path",
	)
	fs.StringSliceVar(
		&cfg.Security.OutboundAllowlist,
		"security-outbound-allowlist",
		cfg.Security.OutboundAllowlist,
		"comma-separated CIDRs, hosts, or host:port entries allowed for outbound notification and AI requests",
	)
	fs.BoolVar(
		&cfg.Retention.IssuesClosed,
		"issues-closed-retention",
		cfg.Retention.IssuesClosed,
		"delete closed issues after 90 days without events",
	)
	fs.Var(&cfg.Alert.GroupWait, "alert-group-wait", "alert group wait")
	fs.Var(&cfg.Alert.RepeatInterval, "alert-repeat-interval", "alert repeat interval")
	fs.StringVar(
		&cfg.Storage.Metrics.Kind,
		"metrics-kind",
		cfg.Storage.Metrics.Kind,
		"metrics backend: vm or prometheus",
	)
	fs.BoolVar(
		&cfg.Storage.Metrics.Cluster,
		"metrics-cluster",
		cfg.Storage.Metrics.Cluster,
		"use VictoriaMetrics cluster URL paths",
	)
	fs.StringVar(
		&cfg.Storage.Metrics.TenantAccountID,
		"metrics-tenant-account-id",
		cfg.Storage.Metrics.TenantAccountID,
		"platform-level VictoriaMetrics cluster account ID",
	)
	fs.StringVar(
		&cfg.Storage.Metrics.QueryURL,
		"metrics-query-url",
		cfg.Storage.Metrics.QueryURL,
		"external metrics query URL",
	)
	fs.StringVar(
		&cfg.Storage.Metrics.IngestURL,
		"metrics-ingest-url",
		cfg.Storage.Metrics.IngestURL,
		"external metrics ingest base URL",
	)
	fs.StringVar(&cfg.Storage.Logs.URL, "logs-url", cfg.Storage.Logs.URL, "external VictoriaLogs URL")
	fs.StringVar(
		&cfg.Storage.Logs.QueryURL,
		"logs-query-url",
		cfg.Storage.Logs.QueryURL,
		"optional external VictoriaLogs query URL (overrides logs-url for reads)",
	)
	fs.StringVar(
		&cfg.Storage.Logs.IngestURL,
		"logs-ingest-url",
		cfg.Storage.Logs.IngestURL,
		"optional external VictoriaLogs ingest URL (overrides logs-url for writes)",
	)
	fs.StringVar(&cfg.Storage.Traces.URL, "traces-url", cfg.Storage.Traces.URL, "external VictoriaTraces URL")
	fs.StringVar(
		&cfg.Storage.Traces.QueryURL,
		"traces-query-url",
		cfg.Storage.Traces.QueryURL,
		"optional external VictoriaTraces query URL (overrides traces-url for reads)",
	)
	fs.StringVar(
		&cfg.Storage.Traces.IngestURL,
		"traces-ingest-url",
		cfg.Storage.Traces.IngestURL,
		"optional external VictoriaTraces ingest URL (overrides traces-url for writes)",
	)
}

func configFlag(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" || args[i] == "-config" {
			if i+1 == len(args) {
				return "", errors.New("--config requires a value")
			}
			return args[i+1], nil
		}
		if strings.HasPrefix(args[i], "--config=") {
			return strings.TrimPrefix(args[i], "--config="), nil
		}
		if strings.HasPrefix(args[i], "-config=") {
			return strings.TrimPrefix(args[i], "-config="), nil
		}
	}
	return "", nil
}

func applyEnv(c *Config, lookup func(string) (string, bool)) error {
	stringsMap := map[string]*string{
		"LO_LISTEN":                        &c.Listen,
		"LO_EXTERNAL_URL":                  &c.ExternalURL,
		"LO_OTLP_GRPC_LISTEN":              &c.OTLPGRPC.Listen,
		"LO_METRICS_ENDPOINT_BEARER_TOKEN": &c.MetricsEndpoint.BearerToken,
		"LO_DATA_DIR":                      &c.DataDir,
		"LO_TOPOLOGY":                      &c.Topology,
		"LO_STORAGE_BIN_DIR":               &c.StorageBinDir,
		"LO_META_DB_URL":                   &c.Meta.DBURL,
		"LO_REDIS_ADDR":                    &c.Redis.Addr,
		"LO_REDIS_PASSWORD":                &c.Redis.Password,
		"LO_REDIS_KEY_PREFIX":              &c.Redis.KeyPrefix,
		"LO_SCRAPE_MODE":                   &c.Scrape.Mode,
		"LO_AI_PRICING_FILE":               &c.AI.PricingFile,
		// Paths, not the secrets: the key material itself never travels via env.
		"LO_SECURITY_SECRET_KEY_FILE":     &c.Security.SecretKeyFile,
		"LO_SECURITY_ADMIN_PASSWORD_FILE": &c.Security.AdminPasswordFile,
		"LO_SECURITY_TLS_CERT":            &c.Security.TLSCert,
		"LO_SECURITY_TLS_KEY":             &c.Security.TLSKey,
	}
	for name, dst := range stringsMap {
		if v, ok := lookup(name); ok {
			*dst = v
		}
	}
	pathStrings := map[string]*string{
		"LO_BLOB_BACKEND":                      &c.Blob.Backend,
		"LO_BLOB_S3_BUCKET":                    &c.Blob.S3.Bucket,
		"LO_BLOB_S3_REGION":                    &c.Blob.S3.Region,
		"LO_BLOB_S3_ENDPOINT":                  &c.Blob.S3.Endpoint,
		"LO_BLOB_S3_ACCESS_KEY":                &c.Blob.S3.AccessKey,
		"LO_BLOB_S3_SECRET_KEY":                &c.Blob.S3.SecretKey,
		"LO_STORAGE_METRICS_KIND":              &c.Storage.Metrics.Kind,
		"LO_STORAGE_METRICS_QUERY_URL":         &c.Storage.Metrics.QueryURL,
		"LO_STORAGE_METRICS_INGEST_URL":        &c.Storage.Metrics.IngestURL,
		"LO_STORAGE_METRICS_TENANT_ACCOUNT_ID": &c.Storage.Metrics.TenantAccountID,
		"LO_STORAGE_LOGS_URL":                  &c.Storage.Logs.URL,
		"LO_STORAGE_LOGS_QUERY_URL":            &c.Storage.Logs.QueryURL,
		"LO_STORAGE_LOGS_INGEST_URL":           &c.Storage.Logs.IngestURL,
		"LO_STORAGE_TRACES_URL":                &c.Storage.Traces.URL,
		"LO_STORAGE_TRACES_QUERY_URL":          &c.Storage.Traces.QueryURL,
		"LO_STORAGE_TRACES_INGEST_URL":         &c.Storage.Traces.IngestURL,
	}
	for name, dst := range pathStrings {
		if v, ok := lookup(name); ok {
			*dst = v
		}
	}
	if v, ok := lookup("LO_SECURITY_OUTBOUND_ALLOWLIST"); ok {
		parts := strings.Split(v, ",")
		allowlist := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				allowlist = append(allowlist, trimmed)
			}
		}
		c.Security.OutboundAllowlist = allowlist
	}
	if v, ok := lookup("LO_STANDALONE"); ok {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse LO_STANDALONE: %w", err)
		}
		c.Standalone = parsed
	}
	for name, dst := range map[string]*bool{
		"LO_OTLP_GRPC_ENABLED":                      &c.OTLPGRPC.Enabled,
		"LO_INGEST_RATE_LIMIT_ENABLED":              &c.IngestRateLimit.Enabled,
		"LO_METRICS_ENDPOINT_ENABLED":               &c.MetricsEndpoint.Enabled,
		"LO_METRICS_ENDPOINT_ALLOW_UNAUTHENTICATED": &c.MetricsEndpoint.AllowUnauthenticated,
		"LO_SERVE_WEB_UI":                           &c.ServeWebUI,
		"LO_STORAGE_METRICS_CLUSTER":                &c.Storage.Metrics.Cluster,
		"LO_SECURITY_CSRF":                          &c.Security.CSRF,
		"LO_SECURITY_SESSION_SECURE_COOKIE":         &c.Security.SessionSecureCookie,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*dst = parsed
		}
	}
	if v, ok := lookup("LO_REDIS_ENABLED"); ok {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse LO_REDIS_ENABLED: %w", err)
		}
		c.Redis.Enabled = parsed
	}
	if v, ok := lookup("LO_OTLP_GRPC_MAX_RECV_BYTES"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LO_OTLP_GRPC_MAX_RECV_BYTES: %w", err)
		}
		c.OTLPGRPC.MaxRecvBytes = parsed
	}
	if v, ok := lookup("LO_INGEST_RATE_LIMIT_REQUESTS_PER_SECOND"); ok {
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("parse LO_INGEST_RATE_LIMIT_REQUESTS_PER_SECOND: %w", err)
		}
		c.IngestRateLimit.RequestsPerSecond = parsed
	}
	for name, dst := range map[string]*int{
		"LO_INGEST_RATE_LIMIT_BURST":                &c.IngestRateLimit.Burst,
		"LO_INGEST_RATE_LIMIT_MAX_TRACKED_PROJECTS": &c.IngestRateLimit.MaxTrackedProjects,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*dst = parsed
		}
	}
	if v, ok := lookup("LO_INGEST_RATE_LIMIT_BYTES_PER_SECOND"); ok {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("parse LO_INGEST_RATE_LIMIT_BYTES_PER_SECOND: %w", err)
		}
		c.IngestRateLimit.BytesPerSecond = parsed
	}
	if v, ok := lookup("LO_REDIS_DB"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LO_REDIS_DB: %w", err)
		}
		c.Redis.DB = parsed
	}
	if v, ok := lookup("LO_PROCESS_COMPONENT_PORT_OFFSET"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LO_PROCESS_COMPONENT_PORT_OFFSET: %w", err)
		}
		c.Process.ComponentPortOffset = parsed
	}
	for name, dst := range map[string]*int64{
		"LO_ERROR_INBOX_MAX_ITEMS":       &c.ErrorInbox.MaxItems,
		"LO_ERROR_INBOX_MAX_BYTES":       &c.ErrorInbox.MaxBytes,
		"LO_ERROR_INBOX_READINESS_ITEMS": &c.ErrorInbox.ReadinessItems,
		"LO_ERROR_INBOX_READINESS_BYTES": &c.ErrorInbox.ReadinessBytes,
	} {
		if value, ok := lookup(name); ok {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*dst = parsed
		}
	}
	for name, dst := range map[string]*bool{
		"LO_SELF_MONITORING_ENABLED":   &c.SelfMonitoring.Enabled,
		"LO_SELF_MONITORING_PROFILING": &c.SelfMonitoring.Profiling,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*dst = parsed
		}
	}
	durations := map[string]*Duration{
		"LO_REPLAY_CLEANUP_INTERVAL": &c.Replay.CleanupInterval,
		"LO_ALERT_GROUP_WAIT":        &c.Alert.GroupWait,
		"LO_ALERT_REPEAT_INTERVAL":   &c.Alert.RepeatInterval,
	}
	for name, dst := range durations {
		if v, ok := lookup(name); ok {
			if err := dst.Set(v); err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
		}
	}
	canonicalDurations := map[string]*Duration{
		"LO_META_MIGRATION_LOCK_TIMEOUT": &c.Meta.MigrationLockTimeout,
		"LO_RETENTION_METRICS":           &c.Retention.Metrics,
		"LO_RETENTION_LOGS":              &c.Retention.Logs,
		"LO_RETENTION_TRACES":            &c.Retention.Traces,
		"LO_RETENTION_REPLAY":            &c.Retention.Replay,
		"LO_RETENTION_SOURCEMAPS":        &c.Retention.SourceMaps,
		"LO_RETENTION_ALERT_EVENTS":      &c.Retention.AlertEvents,
		"LO_RETENTION_PROFILES":          &c.Retention.Profiles,
		"LO_PROCESS_STARTUP_TIMEOUT":     &c.Process.StartupTimeout,
		"LO_PROCESS_SHUTDOWN_TIMEOUT":    &c.Process.ShutdownTimeout,
		"LO_PROCESS_RESTART_INITIAL":     &c.Process.RestartInitial,
		"LO_PROCESS_RESTART_MAX":         &c.Process.RestartMax,
		"LO_PROCESS_DRAIN_TIMEOUT":       &c.Process.DrainTimeout,
		"LO_SECURITY_AUDIT_RETENTION":    &c.Security.AuditRetention,
	}
	for name, dst := range canonicalDurations {
		if v, ok := lookup(name); ok {
			if err := dst.Set(v); err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
		}
	}
	if v, ok := lookup("LO_ALERT_STORM_THRESHOLD"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LO_ALERT_STORM_THRESHOLD: %w", err)
		}
		c.Alert.StormThreshold = parsed
	}
	if v, ok := lookup("LO_SECURITY_TRUSTED_PROXIES"); ok {
		parts := strings.Split(v, ",")
		proxies := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				proxies = append(proxies, trimmed)
			}
		}
		c.Security.TrustedProxies = proxies
	}
	if v, ok := lookup("LO_RETENTION_ISSUES_CLOSED"); ok {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse LO_RETENTION_ISSUES_CLOSED: %w", err)
		}
		c.Retention.IssuesClosed = parsed
	}
	return nil
}

// CheckResult represents one named topology/HA configuration validation check.
type CheckResult struct {
	Name string
	Err  error
}

// IsPostgresURL reports whether dbURL uses a PostgreSQL scheme.
func IsPostgresURL(dbURL string) bool {
	return strings.HasPrefix(dbURL, "postgres://") || strings.HasPrefix(dbURL, "postgresql://")
}

// EffectiveTopology returns the resolved deployment topology. When Topology is
// omitted, it derives "all-in-one", "standalone", or "ha" for backward
// compatibility.
func (c Config) EffectiveTopology() string {
	switch c.Topology {
	case TopologyAllInOne, TopologyStandalone, TopologyHA:
		return c.Topology
	}
	if !c.Standalone {
		return TopologyAllInOne
	}
	if IsPostgresURL(c.Meta.DBURL) && c.Redis.Enabled && c.Blob.Backend == "s3" {
		return TopologyHA
	}
	return TopologyStandalone
}

// IsStandalone reports whether signal storage is external rather than managed
// as embedded VictoriaMetrics/VictoriaLogs/VictoriaTraces child processes.
func (c Config) IsStandalone() bool {
	return c.Standalone || c.Topology == TopologyStandalone || c.Topology == TopologyHA
}

// HAConfigChecks evaluates static configuration prerequisites required by
// multi-replica HA deployments. Both Config.Validate (when topology=ha) and
// `lazy-observe doctor --ha` reuse this kernel so startup and preflight checks
// stay aligned.
func (c Config) HAConfigChecks() []CheckResult {
	results := make([]CheckResult, 0, 6)
	add := func(name string, err error) {
		results = append(results, CheckResult{Name: name, Err: err})
	}
	if !c.IsStandalone() {
		add("standalone", errors.New("standalone must be true (or topology must be ha) for HA replicas"))
	} else {
		add("standalone", nil)
	}
	if !c.Redis.Enabled {
		add("redis", errors.New("redis.enabled must be true for shared sessions and rate limits"))
	} else {
		add("redis", nil)
	}
	if !IsPostgresURL(c.Meta.DBURL) {
		add("metadata-postgres", errors.New("meta.db_url must be a PostgreSQL URL"))
	} else {
		add("metadata-postgres", nil)
	}
	if strings.TrimSpace(c.Security.SecretKeyFile) == "" {
		add("secret-key", errors.New("security.secret_key_file must be configured and shared across HA replicas"))
	} else {
		add("secret-key", nil)
	}
	if c.Blob.Backend != "s3" {
		add("blob-s3", errors.New("blob.backend must be s3 for replay and profile blobs"))
	} else if strings.TrimSpace(c.Blob.S3.Bucket) == "" {
		add("blob-s3", errors.New("blob.s3.bucket is required when blob.backend is s3"))
	} else {
		add("blob-s3", nil)
	}
	if strings.TrimSpace(c.Storage.Metrics.IngestURL) == "" {
		add("storage-metrics-ingest", errors.New("storage.metrics.ingest_url is not configured"))
	} else {
		add("storage-metrics-ingest", nil)
	}
	return results
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" || strings.TrimSpace(c.DataDir) == "" {
		return errors.New("listen and data-dir are required")
	}
	if _, err := NormalizeExternalURL(c.ExternalURL); err != nil {
		return err
	}
	otlpGRPCListen := strings.TrimSpace(c.OTLPGRPC.Listen)
	if c.OTLPGRPC.Enabled && otlpGRPCListen == "" {
		return errors.New("otlp_grpc.listen is required when otlp_grpc.enabled is true")
	}
	if otlpGRPCListen != "" {
		_, rawPort, err := net.SplitHostPort(otlpGRPCListen)
		if err != nil {
			return fmt.Errorf("otlp_grpc.listen must be a valid host:port address: %w", err)
		}
		if strings.TrimSpace(rawPort) == "" {
			return errors.New("otlp_grpc.listen must specify a port between 0 and 65535")
		}
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 0 || port > 65535 {
			return fmt.Errorf("otlp_grpc.listen port %q must be an integer between 0 and 65535", rawPort)
		}
	}
	if c.OTLPGRPC.MaxRecvBytes <= 0 {
		return errors.New("otlp_grpc.max_recv_bytes must be positive")
	}
	if math.IsNaN(c.IngestRateLimit.RequestsPerSecond) ||
		math.IsInf(c.IngestRateLimit.RequestsPerSecond, 0) ||
		c.IngestRateLimit.RequestsPerSecond <= 0 {
		return errors.New("ingest_rate_limit.requests_per_second must be a positive finite number")
	}
	if c.IngestRateLimit.Burst <= 0 {
		return errors.New("ingest_rate_limit.burst must be positive")
	}
	if c.IngestRateLimit.BytesPerSecond < 0 {
		return errors.New("ingest_rate_limit.bytes_per_second must not be negative")
	}
	if c.IngestRateLimit.MaxTrackedProjects <= 0 {
		return errors.New("ingest_rate_limit.max_tracked_projects must be positive")
	}
	if strings.ContainsAny(c.MetricsEndpoint.BearerToken, " \t\r\n") {
		return errors.New("metrics_endpoint.bearer_token must not contain whitespace")
	}
	if c.MetricsEndpoint.AllowUnauthenticated && strings.TrimSpace(c.MetricsEndpoint.BearerToken) != "" {
		return errors.New(
			"metrics_endpoint.allow_unauthenticated cannot be true when metrics_endpoint.bearer_token is set",
		)
	}
	switch c.Topology {
	case "", TopologyAllInOne, TopologyStandalone, TopologyHA:
	default:
		return fmt.Errorf(
			"topology must be %q, %q, or %q, got %q",
			TopologyAllInOne,
			TopologyStandalone,
			TopologyHA,
			c.Topology,
		)
	}
	if c.Topology == TopologyAllInOne && c.Standalone {
		return errors.New("topology all-in-one conflicts with standalone=true")
	}
	if c.Scrape.Mode != ScrapeModeAuto && c.Scrape.Mode != ScrapeModeDisabled {
		return fmt.Errorf("scrape.mode must be %q or %q, got %q", ScrapeModeAuto, ScrapeModeDisabled, c.Scrape.Mode)
	}
	if c.Storage.Metrics.Kind != "vm" && c.Storage.Metrics.Kind != "prometheus" {
		return fmt.Errorf("metrics kind must be vm or prometheus, got %q", c.Storage.Metrics.Kind)
	}
	standalone := c.IsStandalone()
	if c.Storage.Metrics.Cluster && (!standalone || c.Storage.Metrics.Kind != "vm") {
		return errors.New("metrics.cluster requires standalone mode with metrics.kind vm")
	}
	if c.Storage.Metrics.Cluster {
		if _, err := strconv.ParseUint(c.Storage.Metrics.TenantAccountID, 10, 32); err != nil {
			return errors.New("metrics.tenant_account_id must be an unsigned 32-bit integer in cluster mode")
		}
		if strings.TrimSpace(c.Storage.Metrics.IngestURL) == "" {
			return errors.New("metrics.ingest_url is required in cluster mode")
		}
	}
	if standalone {
		logsQuery := c.Storage.Logs.EffectiveQueryURL()
		logsIngest := c.Storage.Logs.EffectiveIngestURL()
		tracesQuery := c.Storage.Traces.EffectiveQueryURL()
		tracesIngest := c.Storage.Traces.EffectiveIngestURL()
		if strings.TrimSpace(c.Storage.Metrics.QueryURL) == "" ||
			(logsQuery == "" && logsIngest == "") ||
			(tracesQuery == "" && tracesIngest == "") {
			return errors.New("standalone requires metrics.query_url, logs.url, and traces.url")
		}
		if logsQuery == "" || logsIngest == "" {
			return errors.New("standalone logs requires url or both query_url and ingest_url")
		}
		if tracesQuery == "" || tracesIngest == "" {
			return errors.New("standalone traces requires url or both query_url and ingest_url")
		}
	}
	for _, field := range []struct {
		name string
		raw  string
	}{
		{"storage.metrics.query_url", c.Storage.Metrics.QueryURL},
		{"storage.metrics.ingest_url", c.Storage.Metrics.IngestURL},
		{"storage.logs.url", c.Storage.Logs.URL},
		{"storage.logs.query_url", c.Storage.Logs.QueryURL},
		{"storage.logs.ingest_url", c.Storage.Logs.IngestURL},
		{"storage.traces.url", c.Storage.Traces.URL},
		{"storage.traces.query_url", c.Storage.Traces.QueryURL},
		{"storage.traces.ingest_url", c.Storage.Traces.IngestURL},
	} {
		if err := validateOptionalHTTPURL(field.name, field.raw); err != nil {
			return err
		}
	}
	if c.Meta.MigrationLockTimeout <= 0 || c.Replay.CleanupInterval <= 0 || c.Alert.StormThreshold <= 0 ||
		c.Process.StartupTimeout <= 0 ||
		c.Process.ShutdownTimeout <= 0 ||
		c.Process.RestartInitial <= 0 ||
		c.Process.RestartMax < c.Process.RestartInitial ||
		c.Process.DrainTimeout <= 0 {
		return errors.New("retention, cleanup interval, alert threshold, and process timeouts must be positive")
	}
	if c.Process.ComponentPortOffset < 0 || c.Process.ComponentPortOffset > 30000 {
		return errors.New("process.component_port_offset must be between 0 and 30000")
	}
	if c.ErrorInbox.MaxItems <= 0 || c.ErrorInbox.MaxBytes <= 0 || c.ErrorInbox.ReadinessItems <= 0 ||
		c.ErrorInbox.ReadinessBytes <= 0 || c.ErrorInbox.ReadinessItems > c.ErrorInbox.MaxItems ||
		c.ErrorInbox.ReadinessBytes > c.ErrorInbox.MaxBytes {
		return errors.New("error Inbox capacity and readiness thresholds are invalid")
	}
	if c.Retention.Metrics <= 0 || c.Retention.Logs <= 0 || c.Retention.Traces <= 0 || c.Retention.Replay <= 0 ||
		c.Retention.SourceMaps <= 0 ||
		c.Retention.AlertEvents <= 0 ||
		c.Retention.Profiles <= 0 ||
		c.Security.AuditRetention <= 0 ||
		c.Alert.GroupWait <= 0 ||
		c.Alert.RepeatInterval <= 0 {
		return errors.New("signal retention, audit retention, and alert intervals must be positive")
	}
	if (strings.TrimSpace(c.Security.TLSCert) == "") != (strings.TrimSpace(c.Security.TLSKey) == "") {
		return errors.New("security.tls_cert and security.tls_key must be set together")
	}
	if c.Blob.Backend != "local" && c.Blob.Backend != "s3" {
		return fmt.Errorf("unsupported blob store %q", c.Blob.Backend)
	}
	if c.Meta.DBURL != "" && !strings.HasPrefix(c.Meta.DBURL, "sqlite://") && !IsPostgresURL(c.Meta.DBURL) {
		return fmt.Errorf("meta.db_url must use sqlite://, postgres://, or postgresql://")
	}
	if c.Redis.Enabled && (strings.TrimSpace(c.Redis.Addr) == "" || strings.TrimSpace(c.Redis.KeyPrefix) == "") {
		return errors.New("redis.addr and redis.key_prefix are required when Redis is enabled")
	}
	if c.Redis.DB < 0 {
		return errors.New("redis.db must not be negative")
	}
	if _, err := outbound.ParseAllowlist(c.Security.OutboundAllowlist); err != nil {
		return err
	}
	for _, raw := range c.Security.TrustedProxies {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return errors.New("security.trusted_proxies entries must not be empty")
		}
		if _, err := netip.ParsePrefix(entry); err != nil {
			if _, addrErr := netip.ParseAddr(entry); addrErr != nil {
				return fmt.Errorf("security.trusted_proxies entry %q must be a valid IP or CIDR", raw)
			}
		}
	}
	if c.Topology == TopologyHA {
		var haErrs []error
		for _, check := range c.HAConfigChecks() {
			if check.Err != nil {
				haErrs = append(haErrs, fmt.Errorf("%s: %w", check.Name, check.Err))
			}
		}
		if len(haErrs) > 0 {
			return fmt.Errorf("ha topology validation failed: %w", errors.Join(haErrs...))
		}
	}
	return nil
}

func validateOptionalHTTPURL(name, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	return nil
}

// NormalizeExternalURL validates and normalizes the public console base URL
// used for clickable links in alert notifications. Empty input returns ("", nil).
// Valid inputs return scheme://host[:port] with lowercase scheme/host and no
// trailing slash.
func NormalizeExternalURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	for _, r := range trimmed {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("external_url must not contain whitespace or control characters")
		}
	}
	if strings.Contains(trimmed, "#") {
		return "", errors.New("external_url must not contain a fragment")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("external_url must be a valid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("external_url must use http or https scheme")
	}
	if u.Opaque != "" {
		return "", errors.New("external_url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return "", errors.New("external_url must not contain userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("external_url must not contain a query string")
	}
	if u.Fragment != "" {
		return "", errors.New("external_url must not contain a fragment")
	}
	if u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("external_url must not include a path")
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "", errors.New("external_url must include a host")
	}
	lowerHost := strings.ToLower(hostname)
	var host string
	if rawPort := u.Port(); rawPort != "" {
		if strings.HasPrefix(u.Host, "[") {
			if _, err := netip.ParseAddr(hostname); err != nil {
				return "", errors.New("external_url must have a valid host")
			}
			lowerHost = "[" + lowerHost + "]"
		}
		_, canonicalPort, err := outbound.NormalizePort(rawPort)
		if err != nil {
			return "", errors.New("external_url must have a valid port between 1 and 65535")
		}
		host = lowerHost + ":" + canonicalPort
	} else if strings.HasPrefix(u.Host, "[") {
		if !strings.HasSuffix(u.Host, "]") {
			return "", errors.New("external_url must have a valid host and port")
		}
		if _, err := netip.ParseAddr(hostname); err != nil {
			return "", errors.New("external_url must have a valid host")
		}
		host = "[" + lowerHost + "]"
	} else {
		if strings.Contains(u.Host, ":") {
			return "", errors.New("external_url must have a valid port")
		}
		host = lowerHost
	}
	return scheme + "://" + host, nil
}
