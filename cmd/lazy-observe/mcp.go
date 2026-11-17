package main

import (
	"context"
	"errors"
	"fmt"
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
	command.Flags().StringVar(
		&dsn,
		"dsn",
		"",
		"MCP DSN http://host:3100/p/<slug>@<token>; <token> must be a user API token ("+mcpUserTokenPrefix+
			"…), not a project token (lo_…), and <slug> selects the project via X-LO-Project",
	)
	return command
}

// mcpUserTokenPrefix marks user API tokens. ADR 0010: project API tokens are
// ingest-only, so MCP (a read client) only accepts user tokens.
const mcpUserTokenPrefix = "lou_"

// serveMCP runs the stdio MCP server; tests replace it to avoid taking over
// the process's stdin/stdout.
var serveMCP = observemcp.RunStdio

func runMCP(dsn string) error {
	api, err := observemcp.ParseDSN(dsn)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(api.Token, mcpUserTokenPrefix) {
		return fmt.Errorf(
			"MCP DSN token must be a user API token (%s…); project API tokens are ingest-only and every "+
				"query answers them with 403 ingest_only_token. Sign in to the web UI, open Settings → "+
				"Personal API tokens and create one (or POST /api/auth/tokens with a session), then use "+
				"http(s)://host:port/p/<slug>@%s…",
			mcpUserTokenPrefix,
			mcpUserTokenPrefix,
		)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveMCP(ctx, api)
}
