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
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	essentials, err := loadServerEssentials(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shared, err := openServerSharedState(ctx, cfg)
	if err != nil {
		return err
	}
	defer shared.Close()

	metadata, err := openServerMetadata(ctx, cfg, logger, essentials.secretBox)
	if err != nil {
		return err
	}
	defer metadata.Close()

	signals, err := startSignalStorage(ctx, cfg, metadata.store, logger)
	if err != nil {
		return err
	}

	loopback, err := openInternalLoopback(cfg)
	if err != nil {
		return err
	}
	defer loopback.Close()

	monitoring := startSelfMonitoring(cfg, loopback.baseURL, metadata.selfProject, metadata.selfToken, logger)
	defer monitoring.Shutdown()

	services := startServerServices(ctx, cfg, monitoring.logger, metadata, signals, shared.limits)
	defer services.Close()

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
	)
	if err != nil {
		return err
	}
	return waitForServerExit(ctx, cfg, monitoring, signals.supervisor, runningGateway)
}

func loadServerEssentials(cfg appconfig.Config) (serverEssentials, error) {
	llmPricing, err := loadLLMPricing()
	if err != nil {
		return serverEssentials{}, fmt.Errorf("configure LLM pricing: %w", err)
	}
	secretBox, err := loadSecretBox(cfg)
	if err != nil {
		return serverEssentials{}, fmt.Errorf("load platform encryption key: %w", err)
	}
	return serverEssentials{llmPricing: llmPricing, secretBox: secretBox}, nil
}
