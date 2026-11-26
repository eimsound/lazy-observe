package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"math"

	"github.com/spf13/pflag"
)

func TestLoadPrecedenceFlagEnvFileDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("listen: file:1000\ndata_dir: file-data\nstorage_bin_dir: file-bin\nalert:\n  storm_threshold: 3\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LO_LISTEN": "env:2000", "LO_DATA_DIR": "env-data", "LO_ALERT_STORM_THRESHOLD": "7"}
	cfg, _, err := Load(
		[]string{"--config", path, "--listen", "flag:3000"},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "flag:3000" {
		t.Fatalf("flag precedence: %q", cfg.Listen)
	}
	if cfg.DataDir != "env-data" {
		t.Fatalf("env precedence: %q", cfg.DataDir)
	}
	if cfg.StorageBinDir != "file-bin" {
		t.Fatalf("file precedence: %q", cfg.StorageBinDir)
	}
	if cfg.Alert.StormThreshold != 7 {
		t.Fatalf("env integer precedence: %d", cfg.Alert.StormThreshold)
	}
	if cfg.Blob.S3.Region != "us-east-1" {
		t.Fatalf("default precedence: %q", cfg.Blob.S3.Region)
	}
}

func TestLoadFlagSetPreservesConfigurationPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(
		path,
		[]byte("listen: file:1000\ndata_dir: file-data\nstorage_bin_dir: file-bin\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	flags := pflag.NewFlagSet("server", pflag.ContinueOnError)
	BindFlags(flags)
	if err := flags.Parse([]string{"--config", path, "--listen", "flag:3000"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LO_LISTEN": "env:2000", "LO_DATA_DIR": "env-data"}
	cfg, configPath, err := LoadFlagSet(
		flags,
		func(key string) (string, bool) { value, ok := env[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if configPath != path || cfg.Listen != "flag:3000" || cfg.DataDir != "env-data" || cfg.StorageBinDir != "file-bin" {
		t.Fatalf("path=%q config=%#v", configPath, cfg)
	}
}

func TestLoadFlagSetReadsFlagsChangedAfterAddFlagSet(t *testing.T) {
	configFlags := pflag.NewFlagSet("server-config", pflag.ContinueOnError)
	BindFlags(configFlags)
	commandFlags := pflag.NewFlagSet("server", pflag.ContinueOnError)
	commandFlags.AddFlagSet(configFlags)
	if err := commandFlags.Parse([]string{"--serve-web-ui=false", "--listen=flag:3100"}); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadFlagSet(configFlags, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServeWebUI || cfg.Listen != "flag:3100" {
		t.Fatalf("merged command flags were not loaded: %#v", cfg)
	}
}

func TestSecurityOutboundAllowlistConfigAndValidation(t *testing.T) {
	cfg, _, err := Load(nil, func(key string) (string, bool) {
		if key == "LO_SECURITY_OUTBOUND_ALLOWLIST" {
			return "127.0.0.1:11434, 10.0.0.0/8", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.OutboundAllowlist) != 2 ||
		cfg.Security.OutboundAllowlist[0] != "127.0.0.1:11434" ||
		cfg.Security.OutboundAllowlist[1] != "10.0.0.0/8" {
		t.Fatalf("outbound allowlist=%#v", cfg.Security.OutboundAllowlist)
	}

	if _, _, err := Load(nil, func(key string) (string, bool) {
		if key == "LO_SECURITY_OUTBOUND_ALLOWLIST" {
			return "127.0.0.1:080", true
		}
		return "", false
	}); err == nil {
		t.Fatal("invalid outbound allowlist port should fail validation")
	}
}

func TestSecuritySessionSecureCookieFileEnvAndFlag(t *testing.T) {
	if Defaults().Security.SessionSecureCookie {
		t.Fatal("session_secure_cookie must default to false")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("security:\n  session_secure_cookie: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if !fromFile.Security.SessionSecureCookie {
		t.Fatal("YAML security.session_secure_cookie=true was not loaded")
	}
	fromEnv, _, err := Load([]string{"--config", path}, func(key string) (string, bool) {
		if key == "LO_SECURITY_SESSION_SECURE_COOKIE" {
			return "false", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.Security.SessionSecureCookie {
		t.Fatal("LO_SECURITY_SESSION_SECURE_COOKIE=false did not override YAML")
	}
	fromFlag, _, err := Load(
		[]string{"--config", path, "--security-secure-cookie=true"},
		func(key string) (string, bool) {
			if key == "LO_SECURITY_SESSION_SECURE_COOKIE" {
				return "false", true
			}
			return "", false
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !fromFlag.Security.SessionSecureCookie {
		t.Fatal("--security-secure-cookie=true did not override environment")
	}
	if _, _, err := Load(nil, func(key string) (string, bool) {
		if key == "LO_SECURITY_SESSION_SECURE_COOKIE" {
			return "not-a-bool", true
		}
		return "", false
	}); err == nil {
		t.Fatal("invalid LO_SECURITY_SESSION_SECURE_COOKIE value must fail")
	}
}

func TestRetentionDefaults(t *testing.T) {
	cfg := Defaults()
	if !cfg.ServeWebUI {
		t.Fatal("embedded web UI must be enabled by default")
	}
	if cfg.Meta.MigrationLockTimeout.Value() != time.Minute {
		t.Fatalf("migration lock timeout default: %s", cfg.Meta.MigrationLockTimeout)
	}
	if cfg.Process.DrainTimeout.Value() != 20*time.Second {
		t.Fatalf("drain timeout default: %s", cfg.Process.DrainTimeout)
	}
	if cfg.Process.RestartInitial.Value() != time.Second || cfg.Process.RestartMax.Value() != time.Minute {
		t.Fatalf("restart defaults: %#v", cfg.Process)
	}
	if !cfg.SelfMonitoring.Enabled || !cfg.SelfMonitoring.Web || cfg.SelfMonitoring.WebReplay ||
		cfg.SelfMonitoring.Profiling {
		t.Fatalf("self-monitoring defaults: %#v", cfg.SelfMonitoring)
	}
	if !cfg.SelfMonitoring.WebEnabled() {
		t.Fatalf("expected WebEnabled() true by default: %#v", cfg.SelfMonitoring)
	}
	if cfg.SelfMonitoring.WebReplayEnabled() {
		t.Fatalf("expected WebReplayEnabled() false by default: %#v", cfg.SelfMonitoring)
	}
	if cfg.Retention.Metrics.Value() != 90*24*time.Hour || cfg.Retention.Logs.Value() != 14*24*time.Hour ||
		cfg.Retention.Traces.Value() != 7*24*time.Hour {
		t.Fatalf("signal defaults: %#v", cfg.Retention)
	}
	if cfg.Retention.Replay.Value() != 3*24*time.Hour || cfg.Retention.SourceMaps.Value() != 90*24*time.Hour ||
		cfg.Retention.AlertEvents.Value() != 30*24*time.Hour ||
		cfg.Retention.Profiles.Value() != 7*24*time.Hour ||
		!cfg.Retention.IssuesClosed {
		t.Fatalf("platform defaults: %#v", cfg.Retention)
	}
}

func TestErrorInboxDefaultsPrecedenceAndValidation(t *testing.T) {
	defaults := Defaults().ErrorInbox
	if defaults.MaxItems != 100_000 || defaults.MaxBytes != 1<<30 || defaults.ReadinessItems != 80_000 ||
		defaults.ReadinessBytes != 800<<20 {
		t.Fatalf("error Inbox defaults=%#v", defaults)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`error_inbox:
  max_items: 20
  max_bytes: 2000
  readiness_items: 15
  readiness_bytes: 1500
`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LO_ERROR_INBOX_MAX_ITEMS": "30", "LO_ERROR_INBOX_MAX_BYTES": "3000"}
	cfg, _, err := Load(
		[]string{"--config", path, "--error-inbox-max-items", "40"},
		func(key string) (string, bool) { value, ok := env[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ErrorInbox.MaxItems != 40 || cfg.ErrorInbox.MaxBytes != 3000 || cfg.ErrorInbox.ReadinessItems != 15 ||
		cfg.ErrorInbox.ReadinessBytes != 1500 {
		t.Fatalf("error Inbox precedence=%#v", cfg.ErrorInbox)
	}
	for _, mutate := range []func(*Config){
		func(value *Config) { value.ErrorInbox.MaxItems = 0 },
		func(value *Config) { value.ErrorInbox.MaxBytes = 0 },
		func(value *Config) { value.ErrorInbox.ReadinessItems = value.ErrorInbox.MaxItems + 1 },
		func(value *Config) { value.ErrorInbox.ReadinessBytes = value.ErrorInbox.MaxBytes + 1 },
	} {
		invalid := Defaults()
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid error Inbox config accepted: %#v", invalid.ErrorInbox)
		}
	}
}

func TestServeWebUIEnvironmentAndFlag(t *testing.T) {
	env := map[string]string{"LO_SERVE_WEB_UI": "false"}
	fromEnv, _, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.ServeWebUI {
		t.Fatal("LO_SERVE_WEB_UI=false did not disable the embedded web UI")
	}

	fromFlag, _, err := Load(
		[]string{"--serve-web-ui=true"},
		func(key string) (string, bool) { value, ok := env[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !fromFlag.ServeWebUI {
		t.Fatal("--serve-web-ui=true did not override the environment")
	}
}

func TestDrainProcessEnvironment(t *testing.T) {
	env := map[string]string{
		"LO_PROCESS_DRAIN_TIMEOUT":         "7s",
		"LO_PROCESS_RESTART_INITIAL":       "2s",
		"LO_PROCESS_RESTART_MAX":           "9s",
		"LO_PROCESS_COMPONENT_PORT_OFFSET": "100",
	}
	cfg, _, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Process.DrainTimeout.Value() != 7*time.Second || cfg.Process.RestartInitial.Value() != 2*time.Second ||
		cfg.Process.RestartMax.Value() != 9*time.Second || cfg.Process.ComponentPortOffset != 100 {
		t.Fatalf("process environment: %#v", cfg.Process)
	}
}

func TestRestartMaximumMustCoverInitialDelay(t *testing.T) {
	cfg := Defaults()
	cfg.Process.RestartInitial = Duration(2 * time.Minute)
	if err := cfg.Validate(); err == nil {
		t.Fatal("restart maximum shorter than initial delay was accepted")
	}
}

func TestRedisDefaultsAndEnvironment(t *testing.T) {
	cfg := Defaults()
	if cfg.Redis.Enabled || cfg.Redis.Addr != "127.0.0.1:6379" || cfg.Redis.DB != 0 ||
		cfg.Redis.KeyPrefix != "lazy-observe" {
		t.Fatalf("redis defaults: %#v", cfg.Redis)
	}
	env := map[string]string{
		"LO_REDIS_ENABLED":    "true",
		"LO_REDIS_ADDR":       "redis.internal:6380",
		"LO_REDIS_PASSWORD":   "secret",
		"LO_REDIS_DB":         "3",
		"LO_REDIS_KEY_PREFIX": "cluster",
	}
	loaded, _, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Redis.Enabled || loaded.Redis.Addr != "redis.internal:6380" || loaded.Redis.Password != "secret" ||
		loaded.Redis.DB != 3 ||
		loaded.Redis.KeyPrefix != "cluster" {
		t.Fatalf("redis env: %#v", loaded.Redis)
	}
}

func TestSelfMonitoringEnvironment(t *testing.T) {
	env := map[string]string{
		"LO_SELF_MONITORING_ENABLED":    "false",
		"LO_SELF_MONITORING_WEB":        "false",
		"LO_SELF_MONITORING_WEB_REPLAY": "true",
		"LO_SELF_MONITORING_PROFILING":  "true",
	}
	cfg, _, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SelfMonitoring.Enabled || cfg.SelfMonitoring.Web || !cfg.SelfMonitoring.WebReplay ||
		!cfg.SelfMonitoring.Profiling {
		t.Fatalf("self-monitoring environment: %#v", cfg.SelfMonitoring)
	}
	if cfg.SelfMonitoring.WebEnabled() || cfg.SelfMonitoring.WebReplayEnabled() {
		t.Fatalf("expected WebEnabled()/WebReplayEnabled() false when disabled: %#v", cfg.SelfMonitoring)
	}

	envOnlyWebDisabled := map[string]string{
		"LO_SELF_MONITORING_WEB":        "false",
		"LO_SELF_MONITORING_WEB_REPLAY": "true",
	}
	cfgWebOff, _, err := Load(
		nil,
		func(key string) (string, bool) { value, ok := envOnlyWebDisabled[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cfgWebOff.SelfMonitoring.Enabled || cfgWebOff.SelfMonitoring.Web || cfgWebOff.SelfMonitoring.WebEnabled() ||
		cfgWebOff.SelfMonitoring.WebReplayEnabled() {
		t.Fatalf("web self-monitoring toggle: %#v", cfgWebOff.SelfMonitoring)
	}

	envParentOff := map[string]string{
		"LO_SELF_MONITORING_ENABLED":    "false",
		"LO_SELF_MONITORING_WEB":        "true",
		"LO_SELF_MONITORING_WEB_REPLAY": "true",
	}
	cfgParentOff, _, err := Load(
		nil,
		func(key string) (string, bool) { value, ok := envParentOff[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfgParentOff.SelfMonitoring.WebEnabled() || cfgParentOff.SelfMonitoring.WebReplayEnabled() {
		t.Fatalf(
			"expected WebEnabled()/WebReplayEnabled() false when parent self_monitoring.enabled=false: %#v",
			cfgParentOff.SelfMonitoring,
		)
	}

	envReplayOn := map[string]string{"LO_SELF_MONITORING_WEB_REPLAY": "true"}
	cfgReplayOn, _, err := Load(
		nil,
		func(key string) (string, bool) { value, ok := envReplayOn[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cfgReplayOn.SelfMonitoring.WebEnabled() || !cfgReplayOn.SelfMonitoring.WebReplayEnabled() {
		t.Fatalf(
			"expected WebReplayEnabled() true when enabled & web & web_replay are true: %#v",
			cfgReplayOn.SelfMonitoring,
		)
	}
}

func TestDefaultConfigUsesEmbeddedStorage(t *testing.T) {
	cfg, _, err := Load(nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "./data" || cfg.StorageBinDir != "" {
		t.Fatalf("default data-dir=%q storage-bin-dir=%q", cfg.DataDir, cfg.StorageBinDir)
	}
}

func TestStandaloneFlagLoadsExternalSignalEndpoints(t *testing.T) {
	cfg, _, err := Load([]string{
		"--standalone",
		"--metrics-kind", "prometheus",
		"--metrics-query-url", "http://prometheus:9090",
		"--metrics-ingest-url", "http://victoria-metrics:8428",
		"--logs-url", "http://victoria-logs:9428",
		"--traces-url", "http://victoria-traces:10428",
	}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Standalone || cfg.Storage.Metrics.Kind != "prometheus" ||
		cfg.Storage.Metrics.QueryURL != "http://prometheus:9090" ||
		cfg.Storage.Metrics.IngestURL != "http://victoria-metrics:8428" ||
		cfg.Storage.Logs.URL != "http://victoria-logs:9428" ||
		cfg.Storage.Traces.URL != "http://victoria-traces:10428" {
		t.Fatalf("standalone flags=%#v", cfg)
	}
}

func TestStandaloneRequiresAllReadEndpoints(t *testing.T) {
	_, _, err := Load(
		[]string{"--standalone", "--metrics-query-url", "http://metrics", "--logs-url", "http://logs"},
		func(string) (string, bool) { return "", false },
	)
	if err == nil || err.Error() != "standalone requires metrics.query_url, logs.url, and traces.url" {
		t.Fatalf("error=%v", err)
	}
}

func TestAIKeyFileDefaultAndLegacyAISectionRejected(t *testing.T) {
	cfg := Defaults()
	if cfg.Security.SecretKeyFile != "" {
		t.Fatalf("secret key file default = %q, want empty (data-dir fallback)", cfg.Security.SecretKeyFile)
	}
	// The ai.* section was removed from the server configuration: AI settings
	// are managed exclusively from the admin UI (stored encrypted in the
	// metadata database). A legacy config that still carries the section must
	// fail loudly instead of silently ignoring credentials.
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "ai:\n  enabled: true\n  provider: deepseek\n  api_key: sk-legacy\n  model: deepseek-chat\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(
		[]string{"--config", path},
		func(string) (string, bool) { return "", false },
	); err == nil || !strings.Contains(strings.ToLower(err.Error()), "ai") {
		t.Fatalf("legacy ai section error=%v, want rejection mentioning the ai section", err)
	}
}

func TestAIPricingFilePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "ai:\n  pricing_file: /etc/lazy-observe/file-prices.json\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.PricingFile != "/etc/lazy-observe/file-prices.json" {
		t.Fatalf("file ai.pricing_file = %q", cfg.AI.PricingFile)
	}
	env := map[string]string{"LO_AI_PRICING_FILE": "/etc/lazy-observe/env-prices.json"}
	cfg, _, err = Load(
		[]string{"--config", path},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.PricingFile != "/etc/lazy-observe/env-prices.json" {
		t.Fatalf("env ai.pricing_file = %q", cfg.AI.PricingFile)
	}
	cfg, _, err = Load(
		[]string{"--config", path, "--ai-pricing-file", "/etc/lazy-observe/flag-prices.json"},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.PricingFile != "/etc/lazy-observe/flag-prices.json" {
		t.Fatalf("flag ai.pricing_file = %q", cfg.AI.PricingFile)
	}
}

// The Helm chart renders a security block that only sets secret_key_file; the
// other security defaults must survive that partial section.
func TestSecretKeyFileFromPartialSecuritySection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "security:\n  secret_key_file: \"/etc/lazy-observe-key/secret.key\"\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.SecretKeyFile != "/etc/lazy-observe-key/secret.key" {
		t.Fatalf("secret_key_file = %q", cfg.Security.SecretKeyFile)
	}
	if !cfg.Security.CSRF || cfg.Security.AuditRetention != Defaults().Security.AuditRetention {
		t.Fatalf("partial security section dropped defaults: %+v", cfg.Security)
	}
}

func TestVMClusterValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Storage.Metrics.Cluster = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("cluster accepted outside standalone mode")
	}
	cfg.Standalone = true
	cfg.Storage.Metrics.QueryURL = "http://vmselect:8481"
	cfg.Storage.Metrics.IngestURL = "http://vminsert:8480"
	cfg.Storage.Metrics.TenantAccountID = "7"
	cfg.Storage.Logs.URL = "http://vlselect:9471"
	cfg.Storage.Traces.URL = "http://vtselect:10471"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRetentionUsesCanonicalConfigurationOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{name: "yaml", yaml: "retention:\n  replay: 4d\n", want: 4 * 24 * time.Hour},
		{name: "environment", env: map[string]string{"LO_RETENTION_REPLAY": "7d"}, want: 7 * 24 * time.Hour},
		{name: "flag", args: []string{"--retention-replay", "8d"}, want: 8 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			base := "storage_bin_dir: bins\n"
			if err := os.WriteFile(path, []byte(base+tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--config", path}, tc.args...)
			cfg, _, err := Load(args, func(key string) (string, bool) { value, ok := tc.env[key]; return value, ok })
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Retention.Replay.Value() != tc.want {
				t.Fatalf("retention.replay=%s want=%s", cfg.Retention.Replay, tc.want)
			}
		})
	}
}

func TestReplayRetentionLegacyYAMLAndFlagAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("storage_bin_dir: bins\nreplay:\n  retention: 5d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("legacy replay.retention YAML was accepted")
	}
	if _, _, err := Load(
		[]string{"--replay-retention", "5d"},
		func(string) (string, bool) { return "", false },
	); err == nil {
		t.Fatal("legacy --replay-retention flag was accepted")
	}
}

func TestLegacyShortEnvironmentAliasesAreIgnored(t *testing.T) {
	legacy := map[string]string{
		"LO_BLOB_STORE":              "s3",
		"LO_METRICS_QUERY_URL":       "http://legacy-metrics",
		"LO_LOGS_URL":                "http://legacy-logs",
		"LO_TRACES_URL":              "http://legacy-traces",
		"LO_METRICS_CLUSTER":         "true",
		"LO_REPLAY_RETENTION":        "5d",
		"LO_STORAGE_STARTUP_TIMEOUT": "9m",
		"LO_DRAIN_TIMEOUT":           "9m",
	}
	cfg, _, err := Load(nil, func(name string) (string, bool) { value, ok := legacy[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	if cfg.Blob.Backend != want.Blob.Backend || cfg.Storage.Metrics.QueryURL != want.Storage.Metrics.QueryURL ||
		cfg.Storage.Logs.URL != want.Storage.Logs.URL || cfg.Storage.Traces.URL != want.Storage.Traces.URL ||
		cfg.Storage.Metrics.Cluster != want.Storage.Metrics.Cluster || cfg.Retention.Replay != want.Retention.Replay ||
		cfg.Process.StartupTimeout != want.Process.StartupTimeout || cfg.Process.DrainTimeout != want.Process.DrainTimeout {
		t.Fatalf("legacy environment aliases changed configuration: %#v", cfg)
	}
}

func TestConfigExampleParses(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	if _, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
}

// config.example.yaml promises that only canonical LO_ names are accepted, so
// every security file variable it lists must be wired into applyEnv. A name that
// is not mapped leaves its field on the empty default, which this test catches.
func TestConfigExampleSecurityFileEnvNamesAreMapped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "# Security fields:")
		if !ok {
			continue
		}
		for _, field := range strings.Split(rest, ",") {
			name := strings.TrimSuffix(strings.TrimSpace(field), ".")
			if strings.HasSuffix(name, "_FILE") {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("config.example.yaml documents no security file environment variable")
	}
	sentinel := "/run/secrets/from-env"
	env := map[string]string{}
	for _, name := range names {
		env[name] = sentinel
	}
	cfg, _, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		var got string
		switch name {
		case "LO_SECURITY_SECRET_KEY_FILE":
			got = cfg.Security.SecretKeyFile
		case "LO_SECURITY_ADMIN_PASSWORD_FILE":
			got = cfg.Security.AdminPasswordFile
		default:
			t.Fatalf("config.example.yaml documents %s; teach this test which field it maps to", name)
		}
		if got != sentinel {
			t.Fatalf("%s is documented in config.example.yaml but applyEnv does not map it", name)
		}
	}
}

func TestAdminPasswordFileFileAndEnvironment(t *testing.T) {
	if Defaults().Security.AdminPasswordFile != "" {
		t.Fatal("admin password file default must be empty (generated password fallback)")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "security:\n  admin_password_file: /run/secrets/file-admin\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.AdminPasswordFile != "/run/secrets/file-admin" {
		t.Fatalf("file admin_password_file = %q", cfg.Security.AdminPasswordFile)
	}
	env := map[string]string{"LO_SECURITY_ADMIN_PASSWORD_FILE": "/run/secrets/env-admin"}
	cfg, _, err = Load(
		[]string{"--config", path},
		func(key string) (string, bool) { value, ok := env[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.AdminPasswordFile != "/run/secrets/env-admin" {
		t.Fatalf("env admin_password_file = %q", cfg.Security.AdminPasswordFile)
	}
}

func TestSecretKeyFileFileAndEnvironment(t *testing.T) {
	if Defaults().Security.SecretKeyFile != "" {
		t.Fatal("secret key file default must be empty (data_dir/secret.key fallback)")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "security:\n  secret_key_file: /run/secrets/file-platform\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.SecretKeyFile != "/run/secrets/file-platform" {
		t.Fatalf("file secret_key_file = %q", cfg.Security.SecretKeyFile)
	}
	env := map[string]string{"LO_SECURITY_SECRET_KEY_FILE": "/run/secrets/env-platform"}
	cfg, _, err = Load(
		[]string{"--config", path},
		func(key string) (string, bool) { value, ok := env[key]; return value, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.SecretKeyFile != "/run/secrets/env-platform" {
		t.Fatalf("env secret_key_file = %q", cfg.Security.SecretKeyFile)
	}
}

func TestMetaDBURLPrecedenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(
		path,
		[]byte("storage_bin_dir: bins\nmeta:\n  db_url: sqlite://file.db\n  migration_lock_timeout: 2m\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"LO_META_DB_URL":                 "postgres://user:pass@localhost/meta",
		"LO_META_MIGRATION_LOCK_TIMEOUT": "3m",
	}
	cfg, _, err := Load(
		[]string{
			"--config",
			path,
			"--meta-db-url",
			"postgresql://flag:pass@localhost/meta",
			"--meta-migration-lock-timeout",
			"4m",
		},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Meta.DBURL != "postgresql://flag:pass@localhost/meta" {
		t.Fatalf("meta db URL precedence: %q", cfg.Meta.DBURL)
	}
	if cfg.Meta.MigrationLockTimeout.Value() != 4*time.Minute {
		t.Fatalf("meta lock timeout precedence: %s", cfg.Meta.MigrationLockTimeout)
	}
	if _, _, err := Load([]string{"--config", path, "--meta-db-url", "mysql://localhost/meta"}, nil); err == nil {
		t.Fatal("unsupported metadata database URL accepted")
	}
	if _, _, err := Load([]string{"--config", path, "--meta-migration-lock-timeout", "0s"}, nil); err == nil {
		t.Fatal("zero metadata migration lock timeout accepted")
	}
}

func TestSecurityTrustedProxiesPrecedenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("security:\n  trusted_proxies:\n    - 10.0.0.0/8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.TrustedProxies) != 1 || cfg.Security.TrustedProxies[0] != "10.0.0.0/8" {
		t.Fatalf("yaml trusted_proxies = %#v", cfg.Security.TrustedProxies)
	}

	env := map[string]string{"LO_SECURITY_TRUSTED_PROXIES": "127.0.0.1, 172.16.0.0/12"}
	cfg, _, err = Load([]string{"--config", path}, func(key string) (string, bool) { v, ok := env[key]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.TrustedProxies) != 2 || cfg.Security.TrustedProxies[0] != "127.0.0.1" ||
		cfg.Security.TrustedProxies[1] != "172.16.0.0/12" {
		t.Fatalf("env trusted_proxies = %#v", cfg.Security.TrustedProxies)
	}

	cfg, _, err = Load(
		[]string{"--config", path, "--security-trusted-proxies", "192.168.1.10"},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.TrustedProxies) != 1 || cfg.Security.TrustedProxies[0] != "192.168.1.10" {
		t.Fatalf("flag trusted_proxies = %#v", cfg.Security.TrustedProxies)
	}

	invalid := Defaults()
	invalid.Security.TrustedProxies = []string{"not-an-ip-or-cidr"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid trusted_proxies entry was accepted")
	}
}

func TestEmptyAndCommentOnlyYAMLConfigLoadsDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "comments-only", content: "# default configuration\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
			if err != nil {
				t.Fatalf("Load(%s): %v", tc.name, err)
			}
			if cfg.Listen != Defaults().Listen || !cfg.Security.CSRF {
				t.Fatalf("unexpected defaults from %s YAML: %+v", tc.name, cfg)
			}
		})
	}
}

func TestSecurityEnvironmentAndValidation(t *testing.T) {
	env := map[string]string{
		"LO_SECURITY_CSRF":                  "false",
		"LO_SECURITY_SESSION_SECURE_COOKIE": "true",
		"LO_SECURITY_AUDIT_RETENTION":       "30d",
		"LO_SECURITY_TLS_CERT":              "/etc/tls/cert.pem",
		"LO_SECURITY_TLS_KEY":               "/etc/tls/key.pem",
	}
	cfg, _, err := Load(nil, func(key string) (string, bool) { v, ok := env[key]; return v, ok })
	if err != nil {
		t.Fatalf("Load security env: %v", err)
	}
	if cfg.Security.CSRF || !cfg.Security.SessionSecureCookie ||
		cfg.Security.AuditRetention.Value() != 30*24*time.Hour ||
		cfg.Security.TLSCert != "/etc/tls/cert.pem" || cfg.Security.TLSKey != "/etc/tls/key.pem" {
		t.Fatalf("security env mismatch: %+v", cfg.Security)
	}

	// Single-sided TLS config must fail validation.
	badTLS := Defaults()
	badTLS.Security.TLSCert = "/etc/tls/cert.pem"
	if err := badTLS.Validate(); err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Fatalf("single-sided TLS err=%v", err)
	}

	// Non-positive audit retention must fail validation.
	badAudit := Defaults()
	badAudit.Security.AuditRetention = 0
	if err := badAudit.Validate(); err == nil {
		t.Fatal("zero audit retention was accepted")
	}
}

func TestStandaloneLogsAndTracesSplitURLsPrecedenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlData := `standalone: true
storage:
  metrics:
    query_url: http://vmselect:8481
  logs:
    url: http://vlogs-fallback:9428
    query_url: http://vlselect-file:9471
  traces:
    query_url: http://vtselect-file:10471
    ingest_url: http://vtinsert-file:10481
`
	if err := os.WriteFile(path, []byte(yamlData), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("load split YAML: %v", err)
	}
	if cfg.Storage.Logs.EffectiveQueryURL() != "http://vlselect-file:9471" ||
		cfg.Storage.Logs.EffectiveIngestURL() != "http://vlogs-fallback:9428" ||
		cfg.Storage.Traces.EffectiveQueryURL() != "http://vtselect-file:10471" ||
		cfg.Storage.Traces.EffectiveIngestURL() != "http://vtinsert-file:10481" {
		t.Fatalf("unexpected YAML split endpoints: logs=%+v traces=%+v", cfg.Storage.Logs, cfg.Storage.Traces)
	}

	env := map[string]string{
		"LO_STORAGE_LOGS_QUERY_URL":    "http://vlselect-env:9471",
		"LO_STORAGE_LOGS_INGEST_URL":   "http://vlinsert-env:9481",
		"LO_STORAGE_TRACES_QUERY_URL":  "http://vtselect-env:10471",
		"LO_STORAGE_TRACES_INGEST_URL": "http://vtinsert-env:10481",
	}
	cfg, _, err = Load(
		[]string{
			"--config", path,
			"--logs-query-url", "http://vlselect-flag:9471",
			"--traces-ingest-url", "http://vtinsert-flag:10481",
		},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatalf("load split precedence: %v", err)
	}
	if cfg.Storage.Logs.EffectiveQueryURL() != "http://vlselect-flag:9471" ||
		cfg.Storage.Logs.EffectiveIngestURL() != "http://vlinsert-env:9481" ||
		cfg.Storage.Traces.EffectiveQueryURL() != "http://vtselect-env:10471" ||
		cfg.Storage.Traces.EffectiveIngestURL() != "http://vtinsert-flag:10481" {
		t.Fatalf("unexpected env/flag split endpoints: logs=%+v traces=%+v", cfg.Storage.Logs, cfg.Storage.Traces)
	}

	// Partial split without fallback url must fail validation in standalone mode.
	partialLogs := Defaults()
	partialLogs.Standalone = true
	partialLogs.Storage.Metrics.QueryURL = "http://metrics:8428"
	partialLogs.Storage.Logs.QueryURL = "http://vlselect:9471"
	partialLogs.Storage.Traces.URL = "http://vtraces:10428"
	if err := partialLogs.Validate(); err == nil {
		t.Fatal("partial logs split without logs.url should fail validation")
	}

	partialTraces := Defaults()
	partialTraces.Standalone = true
	partialTraces.Storage.Metrics.QueryURL = "http://metrics:8428"
	partialTraces.Storage.Logs.URL = "http://vlogs:9428"
	partialTraces.Storage.Traces.IngestURL = "http://vtinsert:10481"
	if err := partialTraces.Validate(); err == nil {
		t.Fatal("partial traces split without traces.url should fail validation")
	}

	// Non-http(s) URL in split override must fail validation.
	badScheme := Defaults()
	badScheme.Storage.Logs.QueryURL = "ftp://vlselect:9471"
	if err := badScheme.Validate(); err == nil {
		t.Fatal("non-http logs.query_url should fail validation")
	}
}

func TestOTLPGRPCDefaultsPrecedenceAndValidation(t *testing.T) {
	def := Defaults().OTLPGRPC
	if def.Enabled || def.Listen != ":4317" || def.MaxRecvBytes != 52<<20 {
		t.Fatalf("unexpected OTLP/gRPC defaults: %+v", def)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlData := "otlp_grpc:\n  enabled: true\n  listen: 127.0.0.1:14317\n  max_recv_bytes: 1048576\n"
	if err := os.WriteFile(path, []byte(yamlData), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"LO_OTLP_GRPC_LISTEN":         "127.0.0.1:24317",
		"LO_OTLP_GRPC_MAX_RECV_BYTES": "2097152",
	}
	cfg, _, err := Load(
		[]string{"--config", path, "--otlp-grpc-listen", "127.0.0.1:34317"},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatalf("Load OTLP/gRPC precedence: %v", err)
	}
	if !cfg.OTLPGRPC.Enabled || cfg.OTLPGRPC.Listen != "127.0.0.1:34317" || cfg.OTLPGRPC.MaxRecvBytes != 2097152 {
		t.Fatalf("unexpected OTLP/gRPC precedence result: %+v", cfg.OTLPGRPC)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "enabled with empty listen",
			mutate: func(c *Config) {
				c.OTLPGRPC.Enabled = true
				c.OTLPGRPC.Listen = ""
			},
		},
		{
			name: "invalid listen address",
			mutate: func(c *Config) {
				c.OTLPGRPC.Listen = "not-a-host-port"
			},
		},
		{
			name: "port out of range while disabled",
			mutate: func(c *Config) {
				c.OTLPGRPC.Enabled = false
				c.OTLPGRPC.Listen = ":99999"
			},
		},
		{
			name: "negative port",
			mutate: func(c *Config) {
				c.OTLPGRPC.Listen = ":-1"
			},
		},
		{
			name: "empty port",
			mutate: func(c *Config) {
				c.OTLPGRPC.Listen = "127.0.0.1:"
			},
		},
		{
			name: "non-numeric port",
			mutate: func(c *Config) {
				c.OTLPGRPC.Listen = ":otlp"
			},
		},
		{
			name: "non-positive max_recv_bytes",
			mutate: func(c *Config) {
				c.OTLPGRPC.MaxRecvBytes = 0
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := Defaults()
			tc.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid OTLP/gRPC config accepted: %+v", invalid.OTLPGRPC)
			}
		})
	}

	for _, addr := range []string{"127.0.0.1:0", "  127.0.0.1:4317  ", ":65535"} {
		valid := Defaults()
		valid.OTLPGRPC.Enabled = true
		valid.OTLPGRPC.Listen = addr
		if err := valid.Validate(); err != nil {
			t.Fatalf("valid OTLP/gRPC listen %q rejected: %v", addr, err)
		}
	}
}

func TestIngestRateLimitAndMetricsEndpointPrecedenceAndValidation(t *testing.T) {
	def := Defaults()
	if def.IngestRateLimit.Enabled ||
		def.IngestRateLimit.RequestsPerSecond != 500 ||
		def.IngestRateLimit.Burst != 1000 ||
		def.IngestRateLimit.BytesPerSecond != 0 ||
		def.IngestRateLimit.MaxTrackedProjects != 4096 {
		t.Fatalf("unexpected IngestRateLimit defaults: %+v", def.IngestRateLimit)
	}
	if def.MetricsEndpoint.Enabled ||
		def.MetricsEndpoint.BearerToken != "" ||
		def.MetricsEndpoint.AllowUnauthenticated {
		t.Fatalf("unexpected MetricsEndpoint defaults: %+v", def.MetricsEndpoint)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlData := `ingest_rate_limit:
  enabled: true
  requests_per_second: 120.5
  burst: 250
  bytes_per_second: 1048576
  max_tracked_projects: 128
metrics_endpoint:
  enabled: true
  bearer_token: file-scrape-token
`
	if err := os.WriteFile(path, []byte(yamlData), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"LO_INGEST_RATE_LIMIT_REQUESTS_PER_SECOND": "200.25",
		"LO_INGEST_RATE_LIMIT_BURST":               "400",
		"LO_METRICS_ENDPOINT_BEARER_TOKEN":         "env-scrape-token",
	}
	cfg, _, err := Load(
		[]string{
			"--config", path,
			"--ingest-rate-limit-burst", "600",
			"--metrics-endpoint-bearer-token", "flag-scrape-token",
		},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatalf("Load precedence failed: %v", err)
	}
	if !cfg.IngestRateLimit.Enabled ||
		cfg.IngestRateLimit.RequestsPerSecond != 200.25 ||
		cfg.IngestRateLimit.Burst != 600 ||
		cfg.IngestRateLimit.BytesPerSecond != 1048576 ||
		cfg.IngestRateLimit.MaxTrackedProjects != 128 {
		t.Fatalf("unexpected IngestRateLimit precedence: %+v", cfg.IngestRateLimit)
	}
	if !cfg.MetricsEndpoint.Enabled || cfg.MetricsEndpoint.BearerToken != "flag-scrape-token" {
		t.Fatalf("unexpected MetricsEndpoint precedence: %+v", cfg.MetricsEndpoint)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "zero rps", mutate: func(c *Config) { c.IngestRateLimit.RequestsPerSecond = 0 }},
		{name: "negative rps", mutate: func(c *Config) { c.IngestRateLimit.RequestsPerSecond = -5 }},
		{name: "NaN rps", mutate: func(c *Config) { c.IngestRateLimit.RequestsPerSecond = math.NaN() }},
		{name: "Inf rps", mutate: func(c *Config) { c.IngestRateLimit.RequestsPerSecond = math.Inf(1) }},
		{name: "neg Inf rps", mutate: func(c *Config) { c.IngestRateLimit.RequestsPerSecond = math.Inf(-1) }},
		{name: "zero burst", mutate: func(c *Config) { c.IngestRateLimit.Burst = 0 }},
		{name: "negative bytes_per_second", mutate: func(c *Config) { c.IngestRateLimit.BytesPerSecond = -1 }},
		{name: "zero max_tracked_projects", mutate: func(c *Config) { c.IngestRateLimit.MaxTrackedProjects = 0 }},
		{
			name:   "metrics bearer token with whitespace",
			mutate: func(c *Config) { c.MetricsEndpoint.BearerToken = "bad token" },
		},
		{
			name: "metrics bearer token conflicts with allow_unauthenticated",
			mutate: func(c *Config) {
				c.MetricsEndpoint.Enabled = true
				c.MetricsEndpoint.BearerToken = "scrape-secret"
				c.MetricsEndpoint.AllowUnauthenticated = true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := Defaults()
			tc.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("expected validation failure for %s", tc.name)
			}
		})
	}
}

func TestNormalizeExternalURL(t *testing.T) {
	validCases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "whitespace only", raw: "  \t\n ", want: ""},
		{name: "http", raw: "http://observe.example.com", want: "http://observe.example.com"},
		{name: "https", raw: "https://observe.example.com", want: "https://observe.example.com"},
		{name: "http with port", raw: "http://localhost:3100", want: "http://localhost:3100"},
		{name: "https with port", raw: "https://observe.example.com:8443", want: "https://observe.example.com:8443"},
		{name: "ipv6 with port", raw: "http://[::1]:3100/", want: "http://[::1]:3100"},
		{name: "ipv6 without port", raw: "https://[2001:DB8::1]/", want: "https://[2001:db8::1]"},
		{name: "trailing slash", raw: "https://observe.example.com/", want: "https://observe.example.com"},
		{
			name: "uppercase scheme and host",
			raw:  "  HTTPS://Observe.Example.COM:8443/  ",
			want: "https://observe.example.com:8443",
		},
	}
	for _, tc := range validCases {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			got, err := NormalizeExternalURL(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeExternalURL(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeExternalURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	invalidCases := []struct {
		name string
		raw  string
	}{
		{name: "ftp scheme", raw: "ftp://observe.example.com"},
		{name: "no scheme", raw: "observe.example.com"},
		{name: "opaque", raw: "http:observe.example.com"},
		{name: "no host", raw: "https://"},
		{name: "no host with port", raw: "https://:8080"},
		{name: "port zero", raw: "https://observe.example.com:0"},
		{name: "port leading zero", raw: "https://observe.example.com:080"},
		{name: "port overflow", raw: "https://observe.example.com:65536"},
		{name: "non-numeric port", raw: "https://observe.example.com:abc"},
		{name: "trailing empty port", raw: "https://observe.example.com:"},
		{name: "userinfo", raw: "https://user:pass@observe.example.com"},
		{name: "empty userinfo", raw: "https://@observe.example.com"},
		{name: "query", raw: "https://observe.example.com?a=1"},
		{name: "question mark only", raw: "https://observe.example.com?"},
		{name: "fragment", raw: "https://observe.example.com#section"},
		{name: "hash only", raw: "https://observe.example.com#"},
		{name: "subpath", raw: "https://observe.example.com/sub"},
		{name: "double slash path", raw: "https://observe.example.com//"},
		{name: "internal space", raw: "https://obs erve.example.com"},
		{name: "internal newline", raw: "https://observe\n.example.com"},
		{name: "control char", raw: "https://observe.example.com\x00"},
		{name: "bel control char", raw: "https://observe\x07.example.com"},
	}
	for _, tc := range invalidCases {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			got, err := NormalizeExternalURL(tc.raw)
			if err == nil {
				t.Fatalf("NormalizeExternalURL(%q) = %q, expected error", tc.raw, got)
			}
			if !strings.HasPrefix(err.Error(), "external_url must ") {
				t.Fatalf(
					"NormalizeExternalURL(%q) error = %q, want prefix %q",
					tc.raw,
					err.Error(),
					"external_url must ",
				)
			}
		})
	}
}

func TestExternalURLPrecedenceAndValidation(t *testing.T) {
	if Defaults().ExternalURL != "" {
		t.Fatalf("default external_url = %q, want empty", Defaults().ExternalURL)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("external_url: https://yaml.example.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("Load YAML external_url: %v", err)
	}
	if cfg.ExternalURL != "https://yaml.example.com/" {
		t.Fatalf("YAML external_url = %q", cfg.ExternalURL)
	}
	env := map[string]string{"LO_EXTERNAL_URL": "https://env.example.com"}
	cfg, _, err = Load(
		[]string{"--config", path},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatalf("Load env external_url: %v", err)
	}
	if cfg.ExternalURL != "https://env.example.com" {
		t.Fatalf("env external_url = %q", cfg.ExternalURL)
	}
	cfg, _, err = Load(
		[]string{"--config", path, "--external-url", "https://flag.example.com"},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatalf("Load flag external_url: %v", err)
	}
	if cfg.ExternalURL != "https://flag.example.com" {
		t.Fatalf("flag external_url = %q", cfg.ExternalURL)
	}

	if _, _, err := Load([]string{"--external-url", "https://observe.example.com/sub"}, nil); err == nil {
		t.Fatal("invalid external_url with subpath should fail validation")
	}
}

func TestErrorInboxReceiptRetentionInheritsLogsAndOverrides(t *testing.T) {
	defaults := Defaults()
	if defaults.ErrorInbox.ReceiptRetention != 0 {
		t.Fatalf("receipt retention must default to unset, got %v", defaults.ErrorInbox.ReceiptRetention)
	}
	if got := defaults.EffectiveErrorInboxReceiptRetention(); got != 14*24*time.Hour {
		t.Fatalf("default effective receipt retention=%v want retention.logs 336h", got)
	}

	inherited, _, err := Load([]string{"--logs-retention", "21d"}, mapLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := inherited.EffectiveErrorInboxReceiptRetention(); got != 21*24*time.Hour {
		t.Fatalf("unset receipt retention must inherit retention.logs, got %v", got)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(
		path,
		[]byte("retention:\n  logs: 7d\nerror_inbox:\n  receipt_retention: 30d\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	fromYAML, _, err := Load([]string{"--config", path}, mapLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if fromYAML.Retention.Logs.Value() != 7*24*time.Hour ||
		fromYAML.EffectiveErrorInboxReceiptRetention() != 30*24*time.Hour {
		t.Fatalf("explicit YAML receipt retention must not follow retention.logs: logs=%v receipts=%v",
			fromYAML.Retention.Logs, fromYAML.EffectiveErrorInboxReceiptRetention())
	}

	env := map[string]string{"LO_ERROR_INBOX_RECEIPT_RETENTION": "45d"}
	fromEnv, _, err := Load([]string{"--config", path}, mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if got := fromEnv.EffectiveErrorInboxReceiptRetention(); got != 45*24*time.Hour {
		t.Fatalf("env receipt retention=%v want 1080h", got)
	}
	fromFlag, _, err := Load(
		[]string{"--config", path, "--error-inbox-receipt-retention", "60d"},
		mapLookup(env),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := fromFlag.EffectiveErrorInboxReceiptRetention(); got != 60*24*time.Hour {
		t.Fatalf("flag receipt retention=%v want 1440h", got)
	}

	explicitZero, _, err := Load(nil, mapLookup(map[string]string{"LO_ERROR_INBOX_RECEIPT_RETENTION": "0s"}))
	if err != nil {
		t.Fatalf("explicit zero must mean inherit: %v", err)
	}
	if got := explicitZero.EffectiveErrorInboxReceiptRetention(); got != explicitZero.Retention.Logs.Value() {
		t.Fatalf("explicit zero receipt retention=%v want retention.logs %v", got, explicitZero.Retention.Logs)
	}
}

func TestErrorInboxReceiptRetentionRejectsNegative(t *testing.T) {
	for _, raw := range []string{"-1h", "-1d"} {
		_, _, err := Load(nil, mapLookup(map[string]string{"LO_ERROR_INBOX_RECEIPT_RETENTION": raw}))
		if err == nil || !strings.Contains(err.Error(), "error_inbox.receipt_retention") {
			t.Fatalf("negative receipt retention %q err=%v", raw, err)
		}
	}
	if _, _, err := Load(nil, mapLookup(map[string]string{"LO_ERROR_INBOX_RECEIPT_RETENTION": "soon"})); err == nil {
		t.Fatal("malformed receipt retention accepted")
	}
}

func TestAlertRepeatIntervalDefaultAndOverrides(t *testing.T) {
	cfg := Defaults()
	if cfg.Alert.RepeatInterval.Value() != time.Hour {
		t.Fatalf("alert.repeat_interval default=%s want 1h", cfg.Alert.RepeatInterval)
	}
	if cfg.Alert.GroupWait.Value() != 30*time.Second {
		t.Fatalf("alert.group_wait default=%s want 30s", cfg.Alert.GroupWait)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("alert:\n  repeat_interval: 4h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, _, err := Load([]string{"--config", path}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if fromFile.Alert.RepeatInterval.Value() != 4*time.Hour {
		t.Fatalf("file alert.repeat_interval=%s want 4h", fromFile.Alert.RepeatInterval)
	}

	env := map[string]string{"LO_ALERT_REPEAT_INTERVAL": "2d"}
	fromEnv, _, err := Load(
		[]string{"--config", path},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.Alert.RepeatInterval.Value() != 48*time.Hour {
		t.Fatalf("env alert.repeat_interval=%s want 48h", fromEnv.Alert.RepeatInterval)
	}

	fromFlag, _, err := Load(
		[]string{"--config", path, "--alert-repeat-interval", "15m"},
		func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	if fromFlag.Alert.RepeatInterval.Value() != 15*time.Minute {
		t.Fatalf("flag alert.repeat_interval=%s want 15m", fromFlag.Alert.RepeatInterval)
	}
}
