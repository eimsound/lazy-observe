package embed

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDisableVMAgentOmitsAgentInAllInOneAndStandalone(t *testing.T) {
	dataDir := t.TempDir()

	allInOneDefs := componentDefinitions(Config{
		Standalone:     false,
		DisableVMAgent: true,
	}, dataDir)
	if len(allInOneDefs) != 3 {
		t.Fatalf("all-in-one DisableVMAgent defs=%d want 3", len(allInOneDefs))
	}
	for i := range allInOneDefs {
		if allInOneDefs[i].name == "vmagent" {
			t.Fatal("vmagent must not be defined when DisableVMAgent is true")
		}
	}

	standaloneDefs := componentDefinitions(Config{
		Standalone:       true,
		DisableVMAgent:   true,
		MetricsIngestURL: "http://vminsert:8480",
	}, dataDir)
	if len(standaloneDefs) != 0 {
		t.Fatalf("standalone DisableVMAgent defs=%d want 0", len(standaloneDefs))
	}

	// Starting supervisor in standalone + DisableVMAgent requires no BinarySource
	// and does not create any scrape config directory.
	sup, err := Start(context.Background(), Config{
		DataDir:          dataDir,
		Standalone:       true,
		DisableVMAgent:   true,
		MetricsIngestURL: "http://vminsert:8480",
		Output:           io.Discard,
	})
	if err != nil {
		t.Fatalf("Start standalone DisableVMAgent: %v", err)
	}
	defer func() { _ = sup.Shutdown() }()

	if _, err := os.Stat(filepath.Join(dataDir, "scrape")); !os.IsNotExist(err) {
		t.Fatalf("scrape directory should not be created when vmagent is disabled: err=%v", err)
	}
}
