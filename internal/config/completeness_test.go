package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

type reflectedConfigField struct {
	yamlPath  string
	envName   string
	flagName  string
	yamlVal   string
	envVal    string
	flagVal   string
	fieldPath []int
}

// TestConfigReflectionCompleteness walks every leaf field of Config using
// reflection so adding a struct field without wiring YAML, canonical LO_* env,
// CLI flag, or defaults immediately fails CI.
func TestConfigReflectionCompleteness(t *testing.T) {
	// Map YAML path -> CLI flag name (only when flag name intentionally differs
	// from the dash-joined YAML path; otherwise flag name == strings.ReplaceAll(yamlPath, ".", "-") with "_" -> "-").
	flagNameOverrides := map[string]string{
		"storage.metrics.kind":              "metrics-kind",
		"storage.metrics.cluster":           "metrics-cluster",
		"storage.metrics.tenant_account_id": "metrics-tenant-account-id",
		"storage.metrics.query_url":         "metrics-query-url",
		"storage.metrics.ingest_url":        "metrics-ingest-url",
		"storage.logs.url":                  "logs-url",
		"storage.logs.query_url":            "logs-query-url",
		"storage.logs.ingest_url":           "logs-ingest-url",
		"storage.traces.url":                "traces-url",
		"storage.traces.query_url":          "traces-query-url",
		"storage.traces.ingest_url":         "traces-ingest-url",
		"retention.metrics":                 "metrics-retention",
		"retention.logs":                    "logs-retention",
		"retention.traces":                  "traces-retention",
		"retention.sourcemaps":              "sourcemaps-retention",
		"retention.alert_events":            "alert-events-retention",
		"retention.profiles":                "profiles-retention",
		"retention.issues_closed":           "issues-closed-retention",
		"blob.backend":                      "blob-store",
		"blob.s3.bucket":                    "s3-bucket",
		"blob.s3.region":                    "s3-region",
		"blob.s3.endpoint":                  "s3-endpoint",
		"blob.s3.access_key":                "s3-access-key",
		"blob.s3.secret_key":                "s3-secret-key",
		"process.startup_timeout":           "storage-startup-timeout",
		"process.shutdown_timeout":          "storage-shutdown-timeout",
		"process.restart_initial":           "storage-restart-initial",
		"process.restart_max":               "storage-restart-max",
		"process.drain_timeout":             "drain-timeout",
		"process.component_port_offset":     "component-port-offset",
		"security.session_secure_cookie":    "security-secure-cookie",
		"security.audit_retention":          "audit-retention",
		"security.tls_cert":                 "tls-cert",
		"security.tls_key":                  "tls-key",
		"self_monitoring.enabled":           "self-monitoring",
	}

	// Distinct valid non-default values (yaml, env, flag) for fields with semantic constraints.
	customValues := map[string][3]string{
		"otlp_grpc.listen":                  {`":14317"`, ":24317", ":34317"},
		"topology":                          {`"standalone"`, "standalone", "all-in-one"},
		"standalone":                        {"true", "false", "true"},
		"storage.metrics.kind":              {`"prometheus"`, "prometheus", "prometheus"},
		"storage.metrics.cluster":           {"false", "false", "false"},
		"storage.metrics.tenant_account_id": {`"11"`, "22", "33"},
		"storage.metrics.query_url": {
			`"http://metrics-yaml:8428"`,
			"http://metrics-env:8428",
			"http://metrics-flag:8428",
		},
		"storage.metrics.ingest_url": {
			`"http://metrics-ingest-yaml:8428"`,
			"http://metrics-ingest-env:8428",
			"http://metrics-ingest-flag:8428",
		},
		"storage.logs.url": {
			`"http://logs-yaml:9428"`,
			"http://logs-env:9428",
			"http://logs-flag:9428",
		},
		"storage.logs.query_url": {
			`"http://logs-q-yaml:9428"`,
			"http://logs-q-env:9428",
			"http://logs-q-flag:9428",
		},
		"storage.logs.ingest_url": {
			`"http://logs-i-yaml:9428"`,
			"http://logs-i-env:9428",
			"http://logs-i-flag:9428",
		},
		"storage.traces.url": {
			`"http://traces-yaml:10428"`,
			"http://traces-env:10428",
			"http://traces-flag:10428",
		},
		"storage.traces.query_url": {
			`"http://traces-q-yaml:10428"`,
			"http://traces-q-env:10428",
			"http://traces-q-flag:10428",
		},
		"storage.traces.ingest_url": {
			`"http://traces-i-yaml:10428"`,
			"http://traces-i-env:10428",
			"http://traces-i-flag:10428",
		},
		"meta.db_url":                   {`"sqlite://yaml.db"`, "sqlite://env.db", "sqlite://flag.db"},
		"blob.backend":                  {`"s3"`, "s3", "s3"},
		"scrape.mode":                   {`"disabled"`, "disabled", "disabled"},
		"error_inbox.max_items":         {"200000", "300000", "400000"},
		"error_inbox.max_bytes":         {"2000000000", "3000000000", "4000000000"},
		"error_inbox.readiness_items":   {"10000", "20000", "30000"},
		"error_inbox.readiness_bytes":   {"10000000", "20000000", "30000000"},
		"process.restart_initial":       {`"2s"`, "3s", "4s"},
		"process.restart_max":           {`"2m"`, "3m", "4m"},
		"process.component_port_offset": {"100", "200", "300"},
		"security.trusted_proxies":      {`["10.0.0.1"]`, "10.0.0.2", "10.0.0.3"},
		"security.outbound_allowlist":   {`["10.0.0.1:8080"]`, "10.0.0.2:8080", "10.0.0.3:8080"},
	}

	var fields []reflectedConfigField
	seenYAML := make(map[string]bool)
	seenEnv := make(map[string]bool)
	seenFlags := make(map[string]bool)

	var walk func(t reflect.Type, yamlPrefix string, indexPath []int)
	walk = func(rt reflect.Type, yamlPrefix string, indexPath []int) {
		for i := range rt.NumField() {
			sf := rt.Field(i)
			if !sf.IsExported() {
				continue
			}
			tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				t.Errorf("Config field %s.%s is missing a yaml tag", rt.Name(), sf.Name)
				continue
			}
			path := tag
			if yamlPrefix != "" {
				path = yamlPrefix + "." + tag
			}
			nextIdx := append(append([]int(nil), indexPath...), i)
			if sf.Type.Kind() == reflect.Struct {
				walk(sf.Type, path, nextIdx)
				continue
			}
			if seenYAML[path] {
				t.Errorf("duplicate yaml path %q", path)
			}
			seenYAML[path] = true

			envName := "LO_" + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
			if seenEnv[envName] {
				t.Errorf("duplicate env name %q", envName)
			}
			seenEnv[envName] = true

			flagName, ok := flagNameOverrides[path]
			if !ok {
				flagName = strings.ReplaceAll(strings.ReplaceAll(path, ".", "-"), "_", "-")
			}
			if seenFlags[flagName] {
				t.Errorf("duplicate flag name %q", flagName)
			}
			seenFlags[flagName] = true

			var yVal, eVal, fVal string
			if custom, hasCustom := customValues[path]; hasCustom {
				yVal, eVal, fVal = custom[0], custom[1], custom[2]
			} else {
				switch sf.Type {
				case reflect.TypeFor[Duration]():
					yVal, eVal, fVal = `"11s"`, "12s", "13s"
				default:
					switch sf.Type.Kind() {
					case reflect.String:
						yVal, eVal, fVal = `"yaml-val"`, "env-val", "flag-val"
					case reflect.Bool:
						yVal, eVal, fVal = "false", "true", "false"
					case reflect.Int, reflect.Int64:
						yVal, eVal, fVal = "11", "12", "13"
					default:
						t.Fatalf("unsupported Config field type %s for %s", sf.Type, path)
					}
				}
			}
			fields = append(fields, reflectedConfigField{
				yamlPath:  path,
				envName:   envName,
				flagName:  flagName,
				yamlVal:   yVal,
				envVal:    eVal,
				flagVal:   fVal,
				fieldPath: nextIdx,
			})
		}
	}
	walk(reflect.TypeFor[Config](), "", nil)

	// 1. Verify every bound CLI flag corresponds to a Config field (except --config)
	// and every Config field has a bound CLI flag.
	fs := pflag.NewFlagSet("completeness", pflag.ContinueOnError)
	BindFlags(fs)
	boundFlags := make(map[string]bool)
	fs.VisitAll(func(f *pflag.Flag) {
		boundFlags[f.Name] = true
	})
	for _, field := range fields {
		if !boundFlags[field.flagName] {
			t.Errorf(
				"Config field %q expects CLI flag --%s, which is not registered in BindFlags",
				field.yamlPath,
				field.flagName,
			)
		}
	}
	for name := range boundFlags {
		if name == "config" {
			continue
		}
		if !seenFlags[name] {
			t.Errorf("CLI flag --%s is registered in BindFlags but has no matching reflected Config field", name)
		}
	}

	// 2. For every leaf field, verify YAML, canonical LO_* env, and CLI flag each
	// wire into that exact Config field cleanly.
	defaultsVal := reflect.ValueOf(Defaults())
	for _, field := range fields {
		t.Run(field.yamlPath, func(t *testing.T) {
			// Skip paired or interdependent fields that need companion settings during single-field Load.
			baseEnv := map[string]string{}
			switch field.yamlPath {
			case "topology", "standalone":
				baseEnv["LO_STORAGE_METRICS_QUERY_URL"] = "http://metrics:8428"
				baseEnv["LO_STORAGE_LOGS_URL"] = "http://logs:9428"
				baseEnv["LO_STORAGE_TRACES_URL"] = "http://traces:10428"
			case "storage.metrics.cluster":
				baseEnv["LO_STANDALONE"] = "true"
				baseEnv["LO_STORAGE_METRICS_QUERY_URL"] = "http://metrics:8428"
				baseEnv["LO_STORAGE_METRICS_INGEST_URL"] = "http://metrics-ingest:8428"
				baseEnv["LO_STORAGE_METRICS_TENANT_ACCOUNT_ID"] = "7"
				baseEnv["LO_STORAGE_LOGS_URL"] = "http://logs:9428"
				baseEnv["LO_STORAGE_TRACES_URL"] = "http://traces:10428"
			case "security.tls_cert":
				baseEnv["LO_SECURITY_TLS_KEY"] = "/tmp/test-key.pem"
			case "security.tls_key":
				baseEnv["LO_SECURITY_TLS_CERT"] = "/tmp/test-cert.pem"
			}

			// Special handling for bool fields: toggle away from default so mutation is observable.
			defaultFieldVal := defaultsVal.FieldByIndex(field.fieldPath)
			yamlVal, envVal, flagVal := field.yamlVal, field.envVal, field.flagVal
			if defaultFieldVal.Kind() == reflect.Bool {
				if field.yamlPath == "storage.metrics.cluster" {
					yamlVal, envVal, flagVal = "true", "true", "true"
				} else if defaultFieldVal.Bool() {
					yamlVal, envVal, flagVal = "false", "false", "false"
				} else {
					yamlVal, envVal, flagVal = "true", "true", "true"
				}
			}

			// Test YAML loading.
			yamlPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(yamlPath, []byte(renderYAMLField(field.yamlPath, yamlVal)), 0o600); err != nil {
				t.Fatal(err)
			}
			fromYAML, _, err := Load([]string{"--config", yamlPath}, mapLookup(baseEnv))
			if err != nil {
				t.Fatalf("YAML load failed for %s=%s: %v", field.yamlPath, yamlVal, err)
			}
			gotYAML := reflect.ValueOf(fromYAML).FieldByIndex(field.fieldPath).Interface()
			if reflect.DeepEqual(gotYAML, defaultFieldVal.Interface()) {
				t.Fatalf(
					"YAML %s=%s did not change field from default %#v",
					field.yamlPath,
					yamlVal,
					defaultFieldVal.Interface(),
				)
			}

			// Test canonical LO_* environment variable loading.
			envCopy := make(map[string]string, len(baseEnv)+1)
			for k, v := range baseEnv {
				envCopy[k] = v
			}
			envCopy[field.envName] = envVal
			fromEnv, _, err := Load(nil, mapLookup(envCopy))
			if err != nil {
				t.Fatalf("env %s=%q failed: %v", field.envName, envVal, err)
			}
			gotEnv := reflect.ValueOf(fromEnv).FieldByIndex(field.fieldPath).Interface()
			if reflect.DeepEqual(gotEnv, defaultFieldVal.Interface()) {
				t.Fatalf(
					"env %s=%q did not change field from default %#v",
					field.envName,
					envVal,
					defaultFieldVal.Interface(),
				)
			}

			// Test CLI flag loading.
			flagArg := "--" + field.flagName + "=" + flagVal
			fromFlag, _, err := Load([]string{flagArg}, mapLookup(baseEnv))
			if err != nil {
				t.Fatalf("flag %s failed: %v", flagArg, err)
			}
			gotFlag := reflect.ValueOf(fromFlag).FieldByIndex(field.fieldPath).Interface()
			if field.yamlPath != "topology" && reflect.DeepEqual(gotFlag, defaultFieldVal.Interface()) {
				t.Fatalf("flag %s did not change field from default %#v", flagArg, defaultFieldVal.Interface())
			}
		})
	}
}

func renderYAMLField(dottedPath, val string) string {
	parts := strings.Split(dottedPath, ".")
	var b strings.Builder
	for i, part := range parts {
		b.WriteString(strings.Repeat("  ", i))
		b.WriteString(part)
		if i == len(parts)-1 {
			b.WriteString(": ")
			b.WriteString(val)
			b.WriteByte('\n')
		} else {
			b.WriteString(":\n")
		}
	}
	return b.String()
}

func mapLookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestExplicitTopologyAndHalfHAFailClosed(t *testing.T) {
	validHA := func() Config {
		cfg := Defaults()
		cfg.Topology = TopologyHA
		cfg.Meta.DBURL = "postgres://postgres:pass@pg:5432/lazy_observe?sslmode=disable"
		cfg.Redis.Enabled = true
		cfg.Redis.Addr = "redis:6379"
		cfg.Blob.Backend = "s3"
		cfg.Blob.S3.Bucket = "lazy-observe-blobs"
		cfg.Security.SecretKeyFile = "/etc/lazy-observe/secret.key"
		cfg.Storage.Metrics.QueryURL = "http://vmselect:8481"
		cfg.Storage.Metrics.IngestURL = "http://vminsert:8480"
		cfg.Storage.Logs.URL = "http://vlogs:9428"
		cfg.Storage.Traces.URL = "http://vtraces:10428"
		return cfg
	}

	if cfg := validHA(); cfg.Validate() != nil {
		t.Fatalf("valid HA config failed validation: %v", cfg.Validate())
	} else if cfg.EffectiveTopology() != TopologyHA || !cfg.IsStandalone() {
		t.Fatalf("effective=%q standalone=%v", cfg.EffectiveTopology(), cfg.IsStandalone())
	}

	// Default backward-compatible derivation.
	def := Defaults()
	if def.EffectiveTopology() != TopologyAllInOne || def.IsStandalone() {
		t.Fatalf("default effective=%q standalone=%v", def.EffectiveTopology(), def.IsStandalone())
	}
	implicitStandalone := Defaults()
	implicitStandalone.Standalone = true
	implicitStandalone.Storage.Metrics.QueryURL = "http://vm:8428"
	implicitStandalone.Storage.Logs.URL = "http://vl:9428"
	implicitStandalone.Storage.Traces.URL = "http://vt:10428"
	if implicitStandalone.EffectiveTopology() != TopologyStandalone {
		t.Fatalf("implicit standalone effective=%q", implicitStandalone.EffectiveTopology())
	}
	implicitHA := validHA()
	implicitHA.Topology = ""
	implicitHA.Standalone = true
	if implicitHA.EffectiveTopology() != TopologyHA {
		t.Fatalf("implicit HA effective=%q", implicitHA.EffectiveTopology())
	}

	// Explicit conflicts and half-HA combinations must fail closed.
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "unknown topology",
			mutate: func(c *Config) { c.Topology = "clustered" },
			want:   "topology must be",
		},
		{
			name: "all-in-one conflicts with standalone",
			mutate: func(c *Config) {
				*c = Defaults()
				c.Topology = TopologyAllInOne
				c.Standalone = true
			},
			want: "conflicts with standalone=true",
		},
		{
			name:   "ha with default sqlite",
			mutate: func(c *Config) { c.Meta.DBURL = "" },
			want:   "meta.db_url",
		},
		{
			name:   "ha with explicit sqlite",
			mutate: func(c *Config) { c.Meta.DBURL = "sqlite:///tmp/meta.db" },
			want:   "meta.db_url",
		},
		{
			name:   "ha with memory limiter (redis disabled)",
			mutate: func(c *Config) { c.Redis.Enabled = false },
			want:   "redis.enabled",
		},
		{
			name:   "ha with local blob storage",
			mutate: func(c *Config) { c.Blob.Backend = "local" },
			want:   "blob.backend",
		},
		{
			name:   "ha with s3 missing bucket",
			mutate: func(c *Config) { c.Blob.S3.Bucket = "" },
			want:   "blob.s3.bucket",
		},
		{
			name:   "ha without shared secret key file",
			mutate: func(c *Config) { c.Security.SecretKeyFile = "" },
			want:   "security.secret_key_file",
		},
		{
			name:   "ha without metrics ingest url",
			mutate: func(c *Config) { c.Storage.Metrics.IngestURL = "" },
			want:   "storage.metrics.ingest_url",
		},
		{
			name:   "invalid scrape mode",
			mutate: func(c *Config) { c.Scrape.Mode = "always" },
			want:   "scrape.mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validHA()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() err=%v, want substring %q", err, tc.want)
			}
		})
	}
}
