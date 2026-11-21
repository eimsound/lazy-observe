package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/secret"
	"github.com/eimsound/lazy-observe/internal/storage"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

const (
	doctorMinTimeout   = 30 * time.Second
	doctorProbeTimeout = 10 * time.Second
)

func newDoctorCommand() *cobra.Command {
	var ha bool
	configFlags := newConfigFlagSet("doctor")
	command := &cobra.Command{
		Use:   "doctor --ha",
		Short: "Check whether an HA deployment is ready",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, _, err := appconfig.LoadFlagSet(configFlags, os.LookupEnv)
			if err != nil {
				if !ha {
					return errors.New("--ha is required")
				}
				return fmt.Errorf("load HA configuration: %w", err)
			}
			if !ha && cfg.Topology != appconfig.TopologyHA {
				return errors.New("--ha is required")
			}
			return runDoctor(cfg)
		},
	}
	command.Flags().BoolVar(&ha, "ha", false, "check shared state and external storage required by HA replicas")
	command.Flags().AddFlagSet(configFlags)
	return command
}

func doctorTimeout(cfg appconfig.Config) time.Duration {
	timeout := doctorMinTimeout
	if lockTimeout := cfg.Meta.MigrationLockTimeout.Value(); lockTimeout > timeout {
		timeout = lockTimeout
	}
	return timeout
}

func runDoctor(cfg appconfig.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout(cfg))
	defer cancel()
	var failures []string
	check := func(name string, err error) {
		if err != nil {
			failures = append(failures, name+": "+err.Error())
			fmt.Fprintf(os.Stdout, "FAIL %-22s %v\n", name, err)
			return
		}
		fmt.Fprintf(os.Stdout, "PASS %s\n", name)
	}
	staticChecks := make(map[string]error)
	for _, res := range cfg.HAConfigChecks() {
		staticChecks[res.Name] = res.Err
	}

	check("standalone", staticChecks["standalone"])
	if err := staticChecks["redis"]; err != nil {
		check("redis", err)
	} else {
		redisCtx, cancelRedis := context.WithTimeout(ctx, doctorProbeTimeout)
		client := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
		check("redis", client.Ping(redisCtx).Err())
		_ = client.Close()
		cancelRedis()
	}

	var pgDiag meta.PostgresDiagnostics
	var pgOK bool
	if err := staticChecks["metadata-postgres"]; err != nil {
		check("metadata-postgres", err)
	} else {
		diag, diagErr := meta.DiagnosePostgresReadOnly(ctx, cfg.Meta.DBURL, meta.OpenOptions{
			MigrationLockTimeout: cfg.Meta.MigrationLockTimeout.Value(),
			ApplicationVersion:   Version,
		})
		check("metadata-postgres", diagErr)
		if diagErr == nil {
			pgDiag = diag
			pgOK = true
		}
	}

	if err := staticChecks["secret-key"]; err != nil {
		check("secret-key", err)
	} else {
		box, loadErr := secret.Load(cfg.Security.SecretKeyFile)
		if loadErr == nil && pgOK && pgDiag.PlatformKeyFingerprint != "" &&
			box.Fingerprint() != pgDiag.PlatformKeyFingerprint {
			loadErr = fmt.Errorf(
				"platform encryption key fingerprint mismatch (recorded %s, file %s)",
				pgDiag.PlatformKeyFingerprint,
				box.Fingerprint(),
			)
		}
		check("secret-key", loadErr)
	}

	if strings.TrimSpace(cfg.Security.AdminPasswordFile) != "" {
		_, pwErr := meta.ReadAdminPasswordFile(cfg.Security.AdminPasswordFile)
		check("admin-password", pwErr)
	} else if !pgOK || !pgDiag.HasBootstrapAdmin {
		check(
			"admin-password",
			errors.New("security.admin_password_file must be configured before initial HA bootstrap"),
		)
	} else {
		check("admin-password", nil)
	}

	if err := staticChecks["blob-s3"]; err != nil {
		check("blob-s3", err)
	} else {
		s3Ctx, cancelS3 := context.WithTimeout(ctx, doctorProbeTimeout)
		blobs, openErr := blob.NewS3(
			blob.S3Config{
				Bucket:    cfg.Blob.S3.Bucket,
				Region:    cfg.Blob.S3.Region,
				Endpoint:  cfg.Blob.S3.Endpoint,
				AccessKey: cfg.Blob.S3.AccessKey,
				SecretKey: cfg.Blob.S3.SecretKey,
			},
		)
		if openErr == nil {
			_, openErr = blobs.List(s3Ctx, "doctor/")
		}
		cancelS3()
		check("blob-s3", openErr)
	}

	metricsIngestConfigured := staticChecks["storage-metrics-ingest"] == nil
	if !metricsIngestConfigured {
		check("storage-metrics-ingest", staticChecks["storage-metrics-ingest"])
	}
	probeClient := &http.Client{Timeout: doctorProbeTimeout}
	backend, backendErr := storage.NewWithOptions(
		storage.BackendOptions{
			MetricsKind:      storage.MetricsKind(cfg.Storage.Metrics.Kind),
			MetricsQueryURL:  cfg.Storage.Metrics.QueryURL,
			MetricsIngestURL: cfg.Storage.Metrics.IngestURL,
			Metrics: storage.MetricsOptions{
				Cluster:         cfg.Storage.Metrics.Cluster,
				TenantAccountID: cfg.Storage.Metrics.TenantAccountID,
			},
			Logs: storage.SignalEndpoint{
				URL:       cfg.Storage.Logs.URL,
				QueryURL:  cfg.Storage.Logs.QueryURL,
				IngestURL: cfg.Storage.Logs.IngestURL,
			},
			Traces: storage.SignalEndpoint{
				URL:       cfg.Storage.Traces.URL,
				QueryURL:  cfg.Storage.Traces.QueryURL,
				IngestURL: cfg.Storage.Traces.IngestURL,
			},
		},
		probeClient,
	)
	if backendErr != nil {
		check("storage-metrics", backendErr)
		if metricsIngestConfigured {
			check("storage-metrics-ingest", backendErr)
		}
		logsQueryURL := cfg.Storage.Logs.EffectiveQueryURL()
		logsIngestURL := cfg.Storage.Logs.EffectiveIngestURL()
		check("storage-logs", checkEndpoint(ctx, probeClient, logsQueryURL, "/health"))
		if logsIngestURL != "" && logsIngestURL != logsQueryURL {
			check("storage-logs-ingest", checkEndpoint(ctx, probeClient, logsIngestURL, "/health"))
		}
		tracesQueryURL := cfg.Storage.Traces.EffectiveQueryURL()
		tracesIngestURL := cfg.Storage.Traces.EffectiveIngestURL()
		check("storage-traces", checkEndpoint(ctx, probeClient, tracesQueryURL, "/health"))
		if tracesIngestURL != "" && tracesIngestURL != tracesQueryURL {
			check("storage-traces-ingest", checkEndpoint(ctx, probeClient, tracesIngestURL, "/health"))
		}
	} else {
		probeCtx, cancelProbe := context.WithTimeout(ctx, doctorProbeTimeout)
		health := backend.Health(probeCtx)
		cancelProbe()
		check("storage-metrics", health["metrics"])
		if metricsIngestConfigured {
			if ingestErr, ok := health["metrics-ingest"]; ok {
				check("storage-metrics-ingest", ingestErr)
			} else {
				check("storage-metrics-ingest", health["metrics"])
			}
		}
		check("storage-logs", health["logs"])
		if logsIngestErr, ok := health["logs-ingest"]; ok {
			check("storage-logs-ingest", logsIngestErr)
		}
		check("storage-traces", health["traces"])
		if tracesIngestErr, ok := health["traces-ingest"]; ok {
			check("storage-traces-ingest", tracesIngestErr)
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("HA readiness failed: %s", strings.Join(failures, "; "))
	}
	if _, err := fmt.Fprintln(
		os.Stdout,
		"HA ready: replicas can use shared PostgreSQL, Redis, S3, and signal storage",
	); err != nil {
		return fmt.Errorf("write HA readiness result: %w", err)
	}
	return nil
}

func checkEndpoint(ctx context.Context, client *http.Client, endpoint string, paths ...string) error {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if base == "" {
		return errors.New("endpoint is not configured")
	}
	if client == nil {
		client = &http.Client{Timeout: doctorProbeTimeout}
	}
	if len(paths) == 0 {
		paths = []string{""}
	}
	var lastErr error
	for _, path := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return nil
		}
		lastErr = fmt.Errorf("%s returned HTTP %d", base+path, resp.StatusCode)
	}
	return lastErr
}
