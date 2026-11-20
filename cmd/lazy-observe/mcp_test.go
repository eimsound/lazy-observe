package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	observemcp "github.com/eimsound/lazy-observe/internal/mcp"
)

func stubServeMCP(t *testing.T) *[]*observemcp.APIClient {
	t.Helper()
	var served []*observemcp.APIClient
	previous := serveMCP
	serveMCP = func(_ context.Context, api *observemcp.APIClient) error {
		served = append(served, api)
		return nil
	}
	t.Cleanup(func() { serveMCP = previous })
	return &served
}

func runMCPCommand(args ...string) (string, error) {
	command := newMCPCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

// ADR 0010: project API tokens are ingest-only, so an MCP DSN carrying one must
// fail at startup instead of on the first tool call with 403 ingest_only_token.
func TestMCPRejectsProjectTokenDSNAtStartup(t *testing.T) {
	served := stubServeMCP(t)
	for _, token := range []string{"lo_0123456789abcdef", "legacy-token"} {
		_, err := runMCPCommand("--dsn", "http://127.0.0.1:3100/p/checkout@"+token)
		if err == nil {
			t.Fatalf("token %q: MCP started with a non-user token", token)
		}
		for _, want := range []string{"lou_", "Personal API tokens"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("token %q: error %q does not mention %q", token, err, want)
			}
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("token %q: error leaks the secret: %q", token, err)
		}
	}
	if len(*served) != 0 {
		t.Fatalf("serve called %d times with a rejected DSN", len(*served))
	}
}

func TestMCPAcceptsUserTokenDSN(t *testing.T) {
	served := stubServeMCP(t)
	if _, err := runMCPCommand("--dsn", "https://observe.example/p/checkout@lou_reader"); err != nil {
		t.Fatal(err)
	}
	if len(*served) != 1 {
		t.Fatalf("serve called %d times, want 1", len(*served))
	}
	api := (*served)[0]
	if api.BaseURL != "https://observe.example" || api.Token != "lou_reader" || api.Project != "checkout" {
		t.Fatalf("served client = %#v", api)
	}
}

func TestMCPHelpNamesUserToken(t *testing.T) {
	stubServeMCP(t)
	out, err := runMCPCommand("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lou_", "LO_MCP_DSN", "--dsn-file"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp --help does not mention %q:\n%s", want, out)
		}
	}
}

func TestMCPReadsDSNFromEnvAndFileWithoutCommandLineWarning(t *testing.T) {
	served := stubServeMCP(t)

	t.Setenv(mcpDSNEnvVar, "https://observe.example/p/envproj@lou_env_secret")
	out, err := runMCPCommand()
	if err != nil {
		t.Fatalf("env DSN failed: %v", err)
	}
	if strings.Contains(out, "warning:") {
		t.Fatalf("env DSN should not emit argv warning: %s", out)
	}
	if len(*served) != 1 || (*served)[0].Project != "envproj" || (*served)[0].Token != "lou_env_secret" {
		t.Fatalf("env served client = %#v", *served)
	}

	dsnPath := filepath.Join(t.TempDir(), "mcp.dsn")
	if writeErr := os.WriteFile(
		dsnPath,
		[]byte("https://observe.example/p/fileproj@lou_file_secret\n"),
		0o600,
	); writeErr != nil {
		t.Fatal(writeErr)
	}
	out, err = runMCPCommand("--dsn-file", dsnPath)
	if err != nil {
		t.Fatalf("--dsn-file failed: %v", err)
	}
	if strings.Contains(out, "warning:") {
		t.Fatalf("--dsn-file should not emit argv warning: %s", out)
	}
	if len(*served) != 2 || (*served)[1].Project != "fileproj" || (*served)[1].Token != "lou_file_secret" {
		t.Fatalf("file served client = %#v", *served)
	}

	cmd := newMCPCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader("https://observe.example/p/stdinproj@lou_stdin_secret\n"))
	cmd.SetArgs([]string{"--dsn-file", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--dsn-file - failed: %v", err)
	}
	if len(*served) != 3 || (*served)[2].Project != "stdinproj" || (*served)[2].Token != "lou_stdin_secret" {
		t.Fatalf("stdin served client = %#v", *served)
	}
}

func TestMCPFlagDSNWarnsWithoutLeakingTokenAndRejectsConflict(t *testing.T) {
	stubServeMCP(t)
	out, err := runMCPCommand("--dsn", "https://observe.example/p/checkout@lou_flag_secret_123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "LO_MCP_DSN") {
		t.Fatalf("expected process-listing warning for --dsn, got: %q", out)
	}
	if strings.Contains(out, "lou_flag_secret_123") {
		t.Fatalf("warning leaked token: %q", out)
	}
	if _, err := runMCPCommand("--dsn", "https://observe.example/p/a@lou_1", "--dsn-file", "/tmp/x"); err == nil {
		t.Fatal("expected error when both --dsn and --dsn-file are set")
	}
}
