package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/spf13/cobra"
)

func newServerCommand() *cobra.Command {
	configFlags := newConfigFlagSet("server")
	command := &cobra.Command{
		Use:   "server",
		Short: "Start the gateway and managed storage components",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, _, err := appconfig.LoadFlagSet(configFlags, os.LookupEnv)
			if err != nil {
				return err
			}
			return runServer(cfg)
		},
	}
	command.Flags().AddFlagSet(configFlags)
	return command
}

func runServer(cfg appconfig.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid server configuration: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cleanups := newCleanupStack(logger)
	defer func() { _ = cleanups.Close() }()

	// Fail fast on listen-address conflicts or broken TLS certificates before
	// touching data_dir, opening metadata databases, or spawning storage.
	publicListener, err := openPublicListener(cfg, logger)
	if err != nil {
		return err
	}
	cleanups.Push("public-listener", publicListener.Close)

	loopback, err := openInternalLoopback(cfg)
	if err != nil {
		return err
	}
	cleanups.PushFunc("internal-loopback", loopback.Close)

	essentials, err := loadServerEssentials(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cleanups.PushFunc("signal-context", stop)
	drainSig := make(chan os.Signal, 1)
	signal.Notify(drainSig, syscall.SIGUSR1)
	cleanups.PushFunc("drain-signal", func() { signal.Stop(drainSig) })

	shared, err := openServerSharedState(ctx, cfg)
	if err != nil {
		return err
	}
	cleanups.PushFunc("shared-state", shared.Close)

	metadata, err := openServerMetadata(ctx, cfg, logger, essentials.secretBox)
	if err != nil {
		return err
	}
	cleanups.PushFunc("metadata", metadata.Close)

	signals, err := startSignalStorage(ctx, cfg, metadata.store, logger)
	if err != nil {
		return err
	}
	cleanups.Push("signal-storage", signals.supervisor.Shutdown)

	enrichServerOutboundPolicy(&essentials.outboundPolicy, cfg, loopback, signals)

	monitoring := startSelfMonitoring(cfg, loopback.baseURL, metadata.selfProject, metadata.selfToken, logger)
	cleanups.PushFunc("self-monitoring", monitoring.Shutdown)

	services := startServerServices(ctx, cfg, monitoring.logger, essentials, metadata, signals, loopback, shared.limits)
	cleanups.PushFunc("server-services", services.Close)

	runningGateway, err := startGateway(
		cfg,
		monitoring.logger,
		essentials,
		shared,
		metadata,
		signals,
		monitoring,
		services,
		loopback,
		publicListener,
	)
	if err != nil {
		return err
	}
	return waitForServerExit(ctx, drainSig, cfg, monitoring, signals.supervisor, runningGateway)
}

func loadServerEssentials(cfg appconfig.Config) (serverEssentials, error) {
	llmPricing, err := loadLLMPricing(cfg)
	if err != nil {
		return serverEssentials{}, fmt.Errorf("configure LLM pricing: %w", err)
	}
	secretBox, err := loadSecretBox(cfg)
	if err != nil {
		return serverEssentials{}, fmt.Errorf("load platform encryption key: %w", err)
	}
	outboundPolicy, err := serverOutboundPolicy(cfg)
	if err != nil {
		return serverEssentials{}, fmt.Errorf("configure outbound security policy: %w", err)
	}
	return serverEssentials{llmPricing: llmPricing, secretBox: secretBox, outboundPolicy: outboundPolicy}, nil
}
