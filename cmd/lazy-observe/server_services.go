package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/janitor"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/replay"
	"github.com/eimsound/lazy-observe/internal/scrape"
	sharedstate "github.com/eimsound/lazy-observe/internal/state"
	lo "github.com/eimsound/lazy-observe/sdk/go"
	"github.com/gofiber/fiber/v3"
)

type selfMonitoring struct {
	client          *lo.Client
	logger          *slog.Logger
	instrumentation fiber.Handler
	shutdownTimeout time.Duration
	shutdownOnce    sync.Once
}

func startSelfMonitoring(
	cfg appconfig.Config,
	loopbackURL string,
	project meta.ProjectInfo,
	token string,
	logger *slog.Logger,
) *selfMonitoring {
	monitoring := &selfMonitoring{logger: logger, shutdownTimeout: cfg.Process.ShutdownTimeout.Value()}
	if !cfg.SelfMonitoring.Enabled {
		logger.Info("self-monitoring disabled")
		return monitoring
	}

	profiling := cfg.SelfMonitoring.Profiling
	client := lo.Init(lo.Config{
		DSN:            loopbackURL + "/p/" + project.Slug + "@" + token,
		ServiceName:    "lazy-observe",
		ExportInterval: 5 * time.Second,
		Profiling:      &profiling,
		TeeToStderr:    boolPointer(true),
	})
	if err := client.Err(); err != nil {
		logger.Error("self-monitoring SDK initialization failed", "error", err)
		return monitoring
	}
	monitoring.client = client
	monitoring.logger = client.Slog()
	monitoring.instrumentation = client.Fiber()
	monitoring.logger.Info("self-monitoring enabled", "project", project.Slug, "profiling", profiling)
	return monitoring
}

func (monitoring *selfMonitoring) Shutdown() {
	monitoring.shutdownOnce.Do(func() {
		if monitoring.client == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), monitoring.shutdownTimeout)
		defer cancel()
		if err := monitoring.client.Shutdown(ctx); err != nil {
			monitoring.logger.Error("self-monitoring shutdown failed", "error", err)
		}
	})
}

func boolPointer(value bool) *bool { return &value }

type serverServices struct {
	alerts                  *alert.Service
	scrapes                 *scrape.Service
	scrapeUnavailableReason string
}

func startServerServices(
	ctx context.Context,
	cfg appconfig.Config,
	logger *slog.Logger,
	essentials serverEssentials,
	metadata *serverMetadata,
	signals *serverSignalStorage,
	limits sharedstate.Limiter,
) *serverServices {
	alerts := alert.NewService(
		alert.Config{
			Store:          metadata.store,
			OutboundPolicy: essentials.outboundPolicy,
			StormThreshold: cfg.Alert.StormThreshold,
			GroupWait:      cfg.Alert.GroupWait.Value(),
			RepeatInterval: cfg.Alert.RepeatInterval.Value(),
			Signals:        signals.backend,
			SharedLimiter:  sharedAlertLimiter(cfg, limits),
		},
	)
	scrapes, unavailableReason := startScrapeService(cfg, logger, metadata.store, signals)
	startRetentionJanitor(ctx, cfg, logger, metadata)
	return &serverServices{
		alerts:                  alerts,
		scrapes:                 scrapes,
		scrapeUnavailableReason: unavailableReason,
	}
}

func sharedAlertLimiter(cfg appconfig.Config, limits sharedstate.Limiter) sharedstate.Limiter {
	if cfg.Redis.Enabled {
		return limits
	}
	return nil
}

func startScrapeService(
	cfg appconfig.Config,
	logger *slog.Logger,
	store *meta.Store,
	signals *serverSignalStorage,
) (*scrape.Service, string) {
	if signals.ingestURL != "" {
		return scrape.NewService(
			scrape.Config{
				Store:         store,
				ConfigPath:    signals.scrapeConfigPath,
				Reload:        signals.supervisor.ReloadVMAgent,
				PlatformPorts: []int{listenPort(cfg.Listen)},
			},
		), ""
	}
	const unavailableReason = "scrape management is disabled: storage.metrics.ingest_url is not configured"
	logger.Warn(
		"metrics ingestion and scrape management disabled",
		"reason",
		"storage.metrics.ingest_url is not configured",
	)
	return nil, unavailableReason
}

func startRetentionJanitor(
	ctx context.Context,
	cfg appconfig.Config,
	logger *slog.Logger,
	metadata *serverMetadata,
) {
	retentionJanitor := janitor.New(logger, janitor.DefaultInterval)
	retentionJanitor.Register(
		"replay",
		cfg.Retention.Replay.Value(),
		&replay.Cleaner{Metadata: metadata.store, Blobs: metadata.blobs},
	)
	retentionJanitor.Register(
		"sourcemaps",
		cfg.Retention.SourceMaps.Value(),
		&janitor.SourceMaps{Metadata: metadata.store, Blobs: metadata.blobs},
	)
	retentionJanitor.Register(
		"error_receipts",
		cfg.Retention.Logs.Value(),
		janitor.ErrorReceipts{Metadata: metadata.store},
	)
	retentionJanitor.Register(
		"alert_events",
		cfg.Retention.AlertEvents.Value(),
		janitor.AlertEvents{Metadata: metadata.store},
	)
	retentionJanitor.Register(
		"profiles",
		cfg.Retention.Profiles.Value(),
		&janitor.Profiles{Metadata: metadata.store, Blobs: metadata.blobs},
	)
	if cfg.Retention.IssuesClosed {
		retentionJanitor.Register("issues_closed", 90*24*time.Hour, janitor.ClosedIssues{Metadata: metadata.store})
	}
	retentionJanitor.Register(
		"audit_events",
		cfg.Security.AuditRetention.Value(),
		janitor.AuditEvents{Metadata: metadata.store},
	)
	go retentionJanitor.Run(ctx)
}

func (services *serverServices) Close() {
	services.alerts.Close()
}
