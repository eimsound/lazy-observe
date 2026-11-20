// Package config loads the server configuration with flag > environment > file > default precedence.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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

type Config struct {
	Listen         string         `yaml:"listen"`
	DataDir        string         `yaml:"data_dir"`
	ServeWebUI     bool           `yaml:"serve_web_ui"`
	Standalone     bool           `yaml:"standalone"`
	StorageBinDir  string         `yaml:"storage_bin_dir"`
	Storage        Storage        `yaml:"storage"`
	Meta           Meta           `yaml:"meta"`
	Redis          Redis          `yaml:"redis"`
	Retention      Retention      `yaml:"retention"`
	Replay         Replay         `yaml:"replay"`
	Blob           Blob           `yaml:"blob"`
	Alert          Alert          `yaml:"alert"`
	AI             AI             `yaml:"ai"`
	ErrorInbox     ErrorInbox     `yaml:"error_inbox"`
	Process        Process        `yaml:"process"`
	Security       Security       `yaml:"security"`
	SelfMonitoring SelfMonitoring `yaml:"self_monitoring"`
}

// Security groups browser-facing hardening controls.
type Security struct {
	// CSRF enables the double-submit CSRF token check for session-authenticated
	// mutating requests. Enabled by default.
	CSRF bool `yaml:"csrf"`
	// SessionSecureCookie marks session and CSRF cookies Secure. Enable only
	// when TLS terminates in front of the gateway.
	SessionSecureCookie bool `yaml:"session_secure_cookie"`
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
	URL string `yaml:"url"`
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
		if err := dec.Decode(&cfg); err != nil {
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
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "root data directory")
	fs.BoolVar(&cfg.ServeWebUI, "serve-web-ui", cfg.ServeWebUI, "serve the embedded web UI from the gateway")
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
		"mark session and CSRF cookies Secure (TLS-terminated deployments)",
	)
	fs.Var(&cfg.Security.AuditRetention, "audit-retention", "management audit event retention")
	fs.StringVar(&cfg.Security.TLSCert, "tls-cert", cfg.Security.TLSCert, "TLS certificate file (enables HTTPS)")
	fs.StringVar(&cfg.Security.TLSKey, "tls-key", cfg.Security.TLSKey, "TLS private key file")
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
	fs.StringVar(&cfg.Storage.Traces.URL, "traces-url", cfg.Storage.Traces.URL, "external VictoriaTraces URL")
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
		"LO_LISTEN":           &c.Listen,
		"LO_DATA_DIR":         &c.DataDir,
		"LO_STORAGE_BIN_DIR":  &c.StorageBinDir,
		"LO_META_DB_URL":      &c.Meta.DBURL,
		"LO_REDIS_ADDR":       &c.Redis.Addr,
		"LO_REDIS_PASSWORD":   &c.Redis.Password,
		"LO_REDIS_KEY_PREFIX": &c.Redis.KeyPrefix,
		"LO_AI_PRICING_FILE":  &c.AI.PricingFile,
		// Paths, not the secrets: the key material itself never travels via env.
		"LO_SECURITY_SECRET_KEY_FILE":     &c.Security.SecretKeyFile,
		"LO_SECURITY_ADMIN_PASSWORD_FILE": &c.Security.AdminPasswordFile,
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
		"LO_STORAGE_TRACES_URL":                &c.Storage.Traces.URL,
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
		"LO_SERVE_WEB_UI":            &c.ServeWebUI,
		"LO_STORAGE_METRICS_CLUSTER": &c.Storage.Metrics.Cluster,
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
	if v, ok := lookup("LO_RETENTION_ISSUES_CLOSED"); ok {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse LO_RETENTION_ISSUES_CLOSED: %w", err)
		}
		c.Retention.IssuesClosed = parsed
	}
	return nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" || strings.TrimSpace(c.DataDir) == "" {
		return errors.New("listen and data-dir are required")
	}
	if c.Storage.Metrics.Kind != "vm" && c.Storage.Metrics.Kind != "prometheus" {
		return fmt.Errorf("metrics kind must be vm or prometheus, got %q", c.Storage.Metrics.Kind)
	}
	if c.Storage.Metrics.Cluster && (!c.Standalone || c.Storage.Metrics.Kind != "vm") {
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
	if c.Standalone {
		if c.Storage.Metrics.QueryURL == "" || c.Storage.Logs.URL == "" || c.Storage.Traces.URL == "" {
			return errors.New("standalone requires metrics.query_url, logs.url, and traces.url")
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
		c.Alert.GroupWait <= 0 ||
		c.Alert.RepeatInterval <= 0 {
		return errors.New("signal retention and alert intervals must be positive")
	}
	if c.Blob.Backend != "local" && c.Blob.Backend != "s3" {
		return fmt.Errorf("unsupported blob store %q", c.Blob.Backend)
	}
	if c.Meta.DBURL != "" && !strings.HasPrefix(c.Meta.DBURL, "sqlite://") &&
		!strings.HasPrefix(c.Meta.DBURL, "postgres://") &&
		!strings.HasPrefix(c.Meta.DBURL, "postgresql://") {
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
	return nil
}
