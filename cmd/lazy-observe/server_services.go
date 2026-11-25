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
		Compression:    lo.CompressionNone,
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
	bgCancel                context.CancelFunc
	bgWG                    sync.WaitGroup
	shutdownTimeout         time.Duration
	logger                  *slog.Logger
	closeOnce               sync.Once
}

func startServerServices(
	ctx context.Context,
	cfg appconfig.Config,
	logger *slog.Logger,
	essentials serverEssentials,
	metadata *serverMetadata,
	signals *serverSignalStorage,
	loopback *internalLoopback,
	limits sharedstate.Limiter,
) *serverServices {
	if logger == nil {
		logger = slog.Default()
	}
	bgCtx, bgCancel := context.WithCancel(ctx)
	services := &serverServices{
		bgCancel:        bgCancel,
		shutdownTimeout: cfg.Process.ShutdownTimeout.Value(),
		logger:          logger,
	}
	externalURL, _ := appconfig.NormalizeExternalURL(cfg.ExternalURL)
	if externalURL == "" {
		logger.Info("alert notification console links disabled: external_url is not set")
	}
	services.alerts = alert.NewService(
		alert.Config{
			Store:          metadata.store,
			OutboundPolicy: essentials.outboundPolicy,
			ExternalURL:    externalURL,
			StormThreshold: cfg.Alert.StormThreshold,
			GroupWait:      cfg.Alert.GroupWait.Value(),
			RepeatInterval: cfg.Alert.RepeatInterval.Value(),
			Signals:        signals.backend,
			SharedLimiter:  sharedAlertLimiter(cfg, limits),
		},
	)
	scrapes, unavailableReason := startScrapeService(
		bgCtx,
		&services.bgWG,
		cfg,
		logger,
		essentials,
		metadata.store,
		signals,
		loopback,
	)
	services.scrapes = scrapes
	services.scrapeUnavailableReason = unavailableReason
	retentionJanitor := buildRetentionJanitor(cfg, logger, metadata)
	services.startBackground(bgCtx, retentionJanitor.Run)
	return services
}

func (services *serverServices) startBackground(ctx context.Context, fn func(context.Context)) {
	if fn == nil {
		return
	}
	services.bgWG.Add(1)
	go func() {
		defer services.bgWG.Done()
		fn(ctx)
	}()
}

func sharedAlertLimiter(cfg appconfig.Config, limits sharedstate.Limiter) sharedstate.Limiter {
	if cfg.Redis.Enabled {
		return limits
	}
	return nil
}

func startScrapeService(
	ctx context.Context,
	wg *sync.WaitGroup,
	cfg appconfig.Config,
	logger *slog.Logger,
	essentials serverEssentials,
	store *meta.Store,
	signals *serverSignalStorage,
	loopback *internalLoopback,
) (*scrape.Service, string) {
	if cfg.Scrape.Mode == appconfig.ScrapeModeDisabled {
		const unavailableReason = "scrape management is disabled: scrape.mode is disabled"
		logger.Info("embedded vmagent and scrape management disabled by configuration")
		return nil, unavailableReason
	}
	if signals.ingestURL != "" {
		platformPorts, blockedHostPorts := collectPlatformEndpoints(cfg, loopback, signals)
		service := scrape.NewService(
			scrape.Config{
				Store:            store,
				ConfigPath:       signals.scrapeConfigPath,
				Reload:           signals.supervisor.ReloadVMAgent,
				PlatformPorts:    platformPorts,
				BlockedHostPorts: blockedHostPorts,
				OutboundPolicy:   essentials.outboundPolicy,
				Logger:           logger,
			},
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			service.RunReconciler(ctx, logger, scrape.DefaultLeaderSyncInterval)
		}()
		return service, ""
	}
	const unavailableReason = "scrape management is disabled: storage.metrics.ingest_url is not configured"
	logger.Warn(
		"metrics ingestion and scrape management disabled",
		"reason",
		"storage.metrics.ingest_url is not configured",
	)
	return nil, unavailableReason
}

func buildRetentionJanitor(
	cfg appconfig.Config,
	logger *slog.Logger,
	metadata *serverMetadata,
) *janitor.Janitor {
	retentionJanitor := janitor.New(logger, janitor.DefaultInterval)
	retentionJanitor.SetCoordinator(metadata.store)
	retentionJanitor.RegisterWithInterval(
		"replay",
		cfg.Retention.Replay.Value(),
		cfg.Replay.CleanupInterval.Value(),
		&replay.Cleaner{
			Metadata:    metadata.store,
			Blobs:       metadata.blobs,
			GracePeriod: replay.DefaultGracePeriod,
		},
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
		"notification_deliveries",
		cfg.Retention.AlertEvents.Value(),
		janitor.NotificationDeliveries{Metadata: metadata.store},
	)
	retentionJanitor.Register(
		"notification_outbox",
		cfg.Retention.AlertEvents.Value(),
		janitor.NotificationOutbox{Metadata: metadata.store},
	)
	retentionJanitor.Register(
		"sessions",
		time.Minute,
		janitor.Sessions{Metadata: metadata.store},
	)
	retentionJanitor.Register(
		"deleted_project_blobs",
		time.Minute,
		&janitor.DeletedProjectBlobs{Metadata: metadata.store, Blobs: metadata.blobs},
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
	return retentionJanitor
}

func (services *serverServices) Close() {
	services.closeOnce.Do(func() {
		if services.bgCancel != nil {
			services.bgCancel()
		}
		done := make(chan struct{})
		go func() {
			services.bgWG.Wait()
			close(done)
		}()
		timeout := services.shutdownTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		timer := time.NewTimer(timeout)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			if services.logger != nil {
				services.logger.Warn("background services shutdown timed out", "timeout", timeout)
			}
		}
		if services.alerts != nil {
			services.alerts.Close()
		}
	})
}
