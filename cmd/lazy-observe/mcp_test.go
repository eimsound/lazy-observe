package main

import (
	"bytes"
	"context"
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
	if !strings.Contains(out, "lou_") {
		t.Fatalf("mcp --help does not name the user API token prefix:\n%s", out)
	}
}
