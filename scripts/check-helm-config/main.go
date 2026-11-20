// Command check-helm-config validates rendered Helm manifests against
// internal/config.Load (YAML KnownFields(true) + Validate()) so ConfigMap
// keys and Deployment LO_* env bindings cannot drift from the server config.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/eimsound/lazy-observe/internal/config"
	"gopkg.in/yaml.v3"
)

type manifestDoc struct {
	Kind string            `yaml:"kind"`
	Data map[string]string `yaml:"data"`
	Spec deploymentSpec    `yaml:"spec"`
}

type deploymentSpec struct {
	Template podTemplateSpec `yaml:"template"`
}

type podTemplateSpec struct {
	Spec podSpec `yaml:"spec"`
}

type podSpec struct {
	Containers []containerSpec `yaml:"containers"`
}

type containerSpec struct {
	Name string   `yaml:"name"`
	Env  []envVar `yaml:"env"`
}

type envVar struct {
	Name string `yaml:"name"`
}

// syntheticEnvValues holds valid placeholder values for every LO_* environment
// variable that the Helm Deployment template may inject into the gateway container.
// Unknown LO_* env vars fail validation so Deployment templates cannot drift
// from internal/config's applyEnv bindings.
var syntheticEnvValues = map[string]string{
	"LO_META_DB_URL":        "postgres://lazy_observe:test@postgresql:5432/lazy_observe?sslmode=require",
	"LO_REDIS_PASSWORD":     "test-redis-password",
	"LO_BLOB_S3_ACCESS_KEY": "test-access-key",
	"LO_BLOB_S3_SECRET_KEY": "test-secret-key",
}

func validateManifestBytes(name string, raw []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var configYAML string
	foundConfigMap := false
	foundDeployment := false
	var envNames []string

	for {
		var doc manifestDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: decode manifest YAML: %w", name, err)
		}
		switch doc.Kind {
		case "ConfigMap":
			if val, ok := doc.Data["config.yaml"]; ok {
				if foundConfigMap {
					return fmt.Errorf("%s: multiple ConfigMaps define data[\"config.yaml\"]", name)
				}
				foundConfigMap = true
				configYAML = val
			}
		case "Deployment":
			foundDeployment = true
			for _, c := range doc.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					if strings.HasPrefix(e.Name, "LO_") {
						envNames = append(envNames, e.Name)
					}
				}
			}
		}
	}

	if !foundConfigMap || strings.TrimSpace(configYAML) == "" {
		return fmt.Errorf("%s: missing ConfigMap with non-empty data[\"config.yaml\"]", name)
	}

	tmpDir, err := os.MkdirTemp("", "helm-config-check-*")
	if err != nil {
		return fmt.Errorf("%s: create temp dir: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(configYAML), 0o600); err != nil {
		return fmt.Errorf("%s: write temp config.yaml: %w", name, err)
	}

	emptyLookup := func(string) (string, bool) { return "", false }
	cfg, _, err := config.Load([]string{"--config", cfgPath}, emptyLookup)
	if err != nil {
		return fmt.Errorf("%s: config.Load(ConfigMap) failed: %w", name, err)
	}

	if foundDeployment {
		envMap := make(map[string]string, len(envNames))
		for _, envName := range envNames {
			val, ok := syntheticEnvValues[envName]
			if !ok {
				return fmt.Errorf(
					"%s: Deployment injects unexpected LO_* env var %q (update contract if intentional)",
					name,
					envName,
				)
			}
			envMap[envName] = val
		}
		if cfg.Standalone {
			if _, ok := envMap["LO_META_DB_URL"]; !ok {
				return fmt.Errorf("%s: standalone ConfigMap requires Deployment to inject LO_META_DB_URL", name)
			}
		} else {
			if cfg.Redis.Enabled {
				return fmt.Errorf("%s: allinone ConfigMap must not enable redis", name)
			}
			if cfg.Blob.Backend != "local" {
				return fmt.Errorf("%s: allinone ConfigMap must use local blob backend, got %q", name, cfg.Blob.Backend)
			}
		}
		if len(envMap) > 0 {
			lookup := func(key string) (string, bool) {
				v, ok := envMap[key]
				return v, ok
			}
			loadedWithEnv, _, err := config.Load([]string{"--config", cfgPath}, lookup)
			if err != nil {
				return fmt.Errorf("%s: config.Load(ConfigMap + Deployment env) failed: %w", name, err)
			}
			if cfg.Standalone && !strings.HasPrefix(loadedWithEnv.Meta.DBURL, "postgres://") {
				return fmt.Errorf("%s: LO_META_DB_URL did not bind to Meta.DBURL", name)
			}
		}
	}

	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: check-helm-config <rendered-manifest.yaml> [...]")
	}
	for _, path := range args {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := validateManifestBytes(path, raw); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PASS: validated %d rendered Helm manifest(s) with internal/config.Load\n", len(os.Args)-1)
}
