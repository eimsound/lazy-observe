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
	scrapeConfigPath, err := writeInitialScrapeConfig(ctx, store, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("write initial scrape config: %w", err)
	}

	queryURL, ingestURL, logsURL, tracesURL := signalStorageURLs(cfg, logger)
	backend, err := storage.NewWithMetricsOptions(
		storage.MetricsKind(cfg.Storage.Metrics.Kind),
		queryURL,
		ingestURL,
		logsURL,
		tracesURL,
		storage.MetricsOptions{
			Cluster:         cfg.Storage.Metrics.Cluster,
			TenantAccountID: cfg.Storage.Metrics.TenantAccountID,
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("configure storage: %w", err)
	}

	binarySource, err := storageBinarySource(cfg, logger)
	if err != nil {
		return nil, err
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

func signalStorageURLs(cfg appconfig.Config, logger *slog.Logger) (string, string, string, string) {
	queryURL := embeddedURL(embed.MetricsAddr, cfg.Process.ComponentPortOffset)
	ingestURL := embeddedURL(embed.MetricsAddr, cfg.Process.ComponentPortOffset)
	logsURL := embeddedURL(embed.LogsAddr, cfg.Process.ComponentPortOffset)
	tracesURL := embeddedURL(embed.TracesAddr, cfg.Process.ComponentPortOffset)
	if cfg.Standalone {
		queryURL = cfg.Storage.Metrics.QueryURL
		ingestURL = cfg.Storage.Metrics.IngestURL
		logsURL = cfg.Storage.Logs.URL
		tracesURL = cfg.Storage.Traces.URL
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
	return queryURL, ingestURL, logsURL, tracesURL
}

func storageBinarySource(cfg appconfig.Config, logger *slog.Logger) (embed.BinarySource, error) {
	if cfg.StorageBinDir != "" {
		logger.Info("using storage binary directory override", "directory", cfg.StorageBinDir)
		return embed.DirectorySource(cfg.StorageBinDir), nil
	}
	extractedDir, err := embedbins.Extract(filepath.Join(cfg.DataDir, "bin"))
	if err != nil {
		return nil, fmt.Errorf("prepare embedded storage binaries: %w", err)
	}
	logger.Info("using embedded storage binaries", "directory", extractedDir)
	return embed.DirectorySource(extractedDir), nil
}
