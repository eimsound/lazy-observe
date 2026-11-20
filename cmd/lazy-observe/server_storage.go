package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/embed"
	embedbins "github.com/eimsound/lazy-observe/internal/embed/bins"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/scrape"
	"github.com/eimsound/lazy-observe/internal/storage"
)

type serverSignalStorage struct {
	backend          *storage.Backend
	supervisor       *embed.Supervisor
	ingestURL        string
	scrapeConfigPath string
}

func startSignalStorage(
	ctx context.Context,
	cfg appconfig.Config,
	store *meta.Store,
	logger *slog.Logger,
) (*serverSignalStorage, error) {
	queryURL, ingestURL, logsEndpoint, tracesEndpoint := signalStorageEndpoints(cfg, logger)
	backend, err := storage.NewWithOptions(
		storage.BackendOptions{
			MetricsKind:      storage.MetricsKind(cfg.Storage.Metrics.Kind),
			MetricsQueryURL:  queryURL,
			MetricsIngestURL: ingestURL,
			Metrics: storage.MetricsOptions{
				Cluster:         cfg.Storage.Metrics.Cluster,
				TenantAccountID: cfg.Storage.Metrics.TenantAccountID,
			},
			Logs:   logsEndpoint,
			Traces: tracesEndpoint,
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("configure storage: %w", err)
	}

	var scrapeConfigPath string
	var binarySource embed.BinarySource
	needsManagedComponents := !cfg.Standalone || ingestURL != ""
	if needsManagedComponents {
		if store.IsClustered() {
			// HA replicas start vmagent with an empty scrape config until the
			// PostgreSQL advisory-lock leader loop elects the single active
			// scraper, preventing duplicate target scrapes across replicas.
			scrapeConfigPath = filepath.Join(cfg.DataDir, "scrape", "scrape.yml")
			if err := scrape.WriteEmptyConfig(scrapeConfigPath); err != nil {
				return nil, fmt.Errorf("write initial standby scrape config: %w", err)
			}
		} else {
			scrapeConfigPath, err = writeInitialScrapeConfig(ctx, store, cfg.DataDir)
			if err != nil {
				return nil, fmt.Errorf("write initial scrape config: %w", err)
			}
		}
		var requiredBins []string
		if cfg.Standalone {
			requiredBins = []string{"vmagent-prod"}
		}
		binarySource, err = storageBinarySource(cfg, logger, requiredBins...)
		if err != nil {
			return nil, err
		}
	}
	supervisor, err := embed.Start(ctx, embed.Config{
		Source:           binarySource,
		DataDir:          filepath.Join(cfg.DataDir, "storage"),
		ScrapeConfigPath: scrapeConfigPath,
		StartupTimeout:   cfg.Process.StartupTimeout.Value(),
		ShutdownTimeout:  cfg.Process.ShutdownTimeout.Value(),
		RestartInitial:   cfg.Process.RestartInitial.Value(),
		RestartMax:       cfg.Process.RestartMax.Value(),
		Output:           os.Stderr,
		Standalone:       cfg.Standalone,
		MetricsIngestURL: backend.PrometheusRemoteWriteBase(),
		MetricsRetention: cfg.Retention.Metrics.Value(),
		LogsRetention:    cfg.Retention.Logs.Value(),
		TracesRetention:  cfg.Retention.Traces.Value(),
		PortOffset:       cfg.Process.ComponentPortOffset,
	})
	if err != nil {
		return nil, fmt.Errorf("start components: %w", err)
	}
	return &serverSignalStorage{
		backend:          backend,
		supervisor:       supervisor,
		ingestURL:        ingestURL,
		scrapeConfigPath: scrapeConfigPath,
	}, nil
}

func writeInitialScrapeConfig(ctx context.Context, store *meta.Store, dataDir string) (string, error) {
	path := filepath.Join(dataDir, "scrape", "scrape.yml")
	if err := scrape.WriteConfig(ctx, store, path); err != nil {
		return "", err
	}
	return path, nil
}

func signalStorageEndpoints(
	cfg appconfig.Config,
	logger *slog.Logger,
) (string, string, storage.SignalEndpoint, storage.SignalEndpoint) {
	queryURL := embeddedURL(embed.MetricsAddr, cfg.Process.ComponentPortOffset)
	ingestURL := embeddedURL(embed.MetricsAddr, cfg.Process.ComponentPortOffset)
	logsEndpoint := storage.SignalEndpoint{URL: embeddedURL(embed.LogsAddr, cfg.Process.ComponentPortOffset)}
	tracesEndpoint := storage.SignalEndpoint{URL: embeddedURL(embed.TracesAddr, cfg.Process.ComponentPortOffset)}
	if cfg.Standalone {
		queryURL = cfg.Storage.Metrics.QueryURL
		ingestURL = cfg.Storage.Metrics.IngestURL
		logsEndpoint = storage.SignalEndpoint{
			URL:       cfg.Storage.Logs.URL,
			QueryURL:  cfg.Storage.Logs.QueryURL,
			IngestURL: cfg.Storage.Logs.IngestURL,
		}
		tracesEndpoint = storage.SignalEndpoint{
			URL:       cfg.Storage.Traces.URL,
			QueryURL:  cfg.Storage.Traces.QueryURL,
			IngestURL: cfg.Storage.Traces.IngestURL,
		}
		logger.Warn(
			"signal retention settings ignored in standalone mode; external storage owns retention",
			"metrics",
			cfg.Retention.Metrics,
			"logs",
			cfg.Retention.Logs,
			"traces",
			cfg.Retention.Traces,
		)
	}
	return queryURL, ingestURL, logsEndpoint, tracesEndpoint
}

func storageBinarySource(cfg appconfig.Config, logger *slog.Logger, names ...string) (embed.BinarySource, error) {
	if cfg.StorageBinDir != "" {
		logger.Info("using storage binary directory override", "directory", cfg.StorageBinDir)
		return embed.DirectorySource(cfg.StorageBinDir), nil
	}
	extractedDir, err := embedbins.ExtractOnly(filepath.Join(cfg.DataDir, "bin"), names...)
	if err != nil {
		return nil, fmt.Errorf("prepare embedded storage binaries: %w", err)
	}
	logger.Info("using embedded storage binaries", "directory", extractedDir)
	return embed.DirectorySource(extractedDir), nil
}
