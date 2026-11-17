package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

func newDoctorCommand() *cobra.Command {
	var ha bool
	configFlags := newConfigFlagSet("doctor")
	command := &cobra.Command{
		Use:   "doctor --ha",
		Short: "Check whether an HA deployment is ready",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !ha {
				return errors.New("--ha is required")
			}
			cfg, _, err := appconfig.LoadFlagSet(configFlags, os.LookupEnv)
			if err != nil {
				return fmt.Errorf("load HA configuration: %w", err)
			}
			return runDoctor(cfg)
		},
	}
	command.Flags().BoolVar(&ha, "ha", false, "check shared state and external storage required by HA replicas")
	command.Flags().AddFlagSet(configFlags)
	return command
}

func runDoctor(cfg appconfig.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var failures []string
	check := func(name string, err error) {
		if err != nil {
			failures = append(failures, name+": "+err.Error())
			fmt.Fprintf(os.Stdout, "FAIL %-18s %v\n", name, err)
			return
		}
		fmt.Fprintf(os.Stdout, "PASS %s\n", name)
	}
	if !cfg.Standalone {
		check("standalone", errors.New("standalone must be true for HA replicas"))
	} else {
		check("standalone", nil)
	}
	if !cfg.Redis.Enabled {
		check("redis", errors.New("redis.enabled must be true for shared sessions and rate limits"))
	} else {
		client := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
		check("redis", client.Ping(ctx).Err())
		_ = client.Close()
	}
	if !strings.HasPrefix(cfg.Meta.DBURL, "postgres://") && !strings.HasPrefix(cfg.Meta.DBURL, "postgresql://") {
		check("metadata-postgres", errors.New("meta.db_url must be a PostgreSQL URL"))
	} else {
		store, openErr := meta.OpenStoreWithOptions(ctx, cfg.Meta.DBURL, meta.OpenOptions{
			MigrationLockTimeout: cfg.Meta.MigrationLockTimeout.Value(),
			ApplicationVersion:   Version,
		})
		check("metadata-postgres", openErr)
		if openErr == nil {
			_ = store.Close()
		}
	}
	if cfg.Blob.Backend != "s3" {
		check("blob-s3", errors.New("blob.backend must be s3 for replay and profile blobs"))
	} else {
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
			_, openErr = blobs.List(ctx, "doctor/")
		}
		check("blob-s3", openErr)
	}
	for name, endpoint := range map[string]string{
		"metrics": cfg.Storage.Metrics.QueryURL,
		"logs":    cfg.Storage.Logs.URL,
		"traces":  cfg.Storage.Traces.URL,
	} {
		check("storage-"+name, checkEndpoint(ctx, endpoint))
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

func checkEndpoint(ctx context.Context, endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
