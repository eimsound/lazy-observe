package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"syscall"

	observemcp "github.com/eimsound/lazy-observe/internal/mcp"
	"github.com/spf13/cobra"
)

func newMCPCommand() *cobra.Command {
	var dsn string
	command := &cobra.Command{
		Use:   "mcp --dsn URL",
		Short: "Run a project-scoped MCP stdio proxy",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(dsn) == "" {
				return errors.New("--dsn is required")
			}
			return runMCP(dsn)
		},
	}
	command.Flags().StringVar(&dsn, "dsn", "", "project DSN, for example http://host:3100/p/<slug>@<token>")
	return command
}

func runMCP(dsn string) error {
	api, err := observemcp.ParseDSN(dsn)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return observemcp.RunStdio(ctx, api)
}
