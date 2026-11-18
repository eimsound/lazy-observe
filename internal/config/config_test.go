package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !cfg.SelfMonitoring.Enabled || cfg.SelfMonitoring.Profiling {
		t.Fatalf("self-monitoring defaults: %#v", cfg.SelfMonitoring)
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
	env := map[string]string{"LO_SELF_MONITORING_ENABLED": "false", "LO_SELF_MONITORING_PROFILING": "true"}
	cfg, _, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SelfMonitoring.Enabled || !cfg.SelfMonitoring.Profiling {
		t.Fatalf("self-monitoring environment: %#v", cfg.SelfMonitoring)
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
	); err == nil || !strings.Contains(err.Error(), "ai") {
		t.Fatalf("legacy ai section error=%v, want rejection mentioning the ai section", err)
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
