package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	observemcp "github.com/eimsound/lazy-observe/internal/mcp"
	"github.com/spf13/cobra"
)

const (
	// mcpUserTokenPrefix marks user API tokens. ADR 0010: project API tokens are
	// ingest-only, so MCP (a read client) only accepts user tokens.
	mcpUserTokenPrefix = "lou_"
	// mcpDSNEnvVar is the recommended environment variable for passing the MCP DSN
	// without exposing the lou_ token in process listings.
	mcpDSNEnvVar = "LO_MCP_DSN"
)

func newMCPCommand() *cobra.Command {
	var dsn string
	var dsnFile string
	command := &cobra.Command{
		Use:   "mcp",
		Short: "Run a project-scoped MCP stdio proxy",
		Long: "Run a project-scoped MCP stdio proxy.\n\n" +
			"Supply the DSN via " + mcpDSNEnvVar + " or --dsn-file (preferred, avoids exposing " +
			mcpUserTokenPrefix + "… tokens in process listings), or via --dsn.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, usedFlag, err := resolveMCPDSN(dsn, dsnFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if usedFlag {
				fmt.Fprintf(
					cmd.ErrOrStderr(),
					"warning: passing --dsn on the command line exposes the %s token in process listings; "+
						"prefer %s or --dsn-file\n",
					mcpUserTokenPrefix,
					mcpDSNEnvVar,
				)
			}
			return runMCP(resolved)
		},
	}
	command.Flags().StringVar(
		&dsn,
		"dsn",
		"",
		"MCP DSN http://host:3100/p/<slug>@<token>; prefer "+mcpDSNEnvVar+
			" or --dsn-file to avoid exposing the user API token ("+mcpUserTokenPrefix+
			"…) in process listings",
	)
	command.Flags().StringVar(
		&dsnFile,
		"dsn-file",
		"",
		"Path to a file containing the MCP DSN (use '-' to read from stdin); "+
			"preferred over --dsn to keep "+mcpUserTokenPrefix+"… tokens out of argv",
	)
	return command
}

func resolveMCPDSN(flagDSN, dsnFile string, stdin io.Reader) (string, bool, error) {
	flagDSN = strings.TrimSpace(flagDSN)
	dsnFile = strings.TrimSpace(dsnFile)
	if flagDSN != "" && dsnFile != "" {
		return "", false, errors.New("specify only one of --dsn or --dsn-file")
	}
	if dsnFile != "" {
		var data []byte
		var err error
		if dsnFile == "-" {
			data, err = io.ReadAll(io.LimitReader(stdin, 64<<10))
		} else {
			data, err = os.ReadFile(dsnFile)
		}
		if err != nil {
			return "", false, fmt.Errorf("read --dsn-file: %w", err)
		}
		resolved := strings.TrimSpace(string(data))
		if resolved == "" {
			return "", false, errors.New("--dsn-file is empty")
		}
		return resolved, false, nil
	}
	if flagDSN != "" {
		return flagDSN, true, nil
	}
	if envDSN := strings.TrimSpace(os.Getenv(mcpDSNEnvVar)); envDSN != "" {
		return envDSN, false, nil
	}
	return "", false, fmt.Errorf(
		"%s, --dsn-file, or --dsn is required",
		mcpDSNEnvVar,
	)
}

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
