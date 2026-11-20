package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
)

type fatalSupervisor struct {
	fatal     chan error
	shutdowns atomic.Int64
}

type inertGatewayLifecycle struct{}

func (inertGatewayLifecycle) BeginDrain()                 {}
func (inertGatewayLifecycle) Drain(context.Context) error { return nil }
func (inertGatewayLifecycle) Shutdown() error             { return nil }

func (s *fatalSupervisor) Done() <-chan error { return s.fatal }

func (s *fatalSupervisor) Shutdown() error {
	s.shutdowns.Add(1)
	return nil
}

func TestWaitForServerExitReturnsOnlySupervisorFatalErrors(t *testing.T) {
	supervisor := &fatalSupervisor{fatal: make(chan error, 1)}
	supervisor.fatal <- errors.New("restart loop invariant failed")
	running := &runningServerGateway{
		gateway: inertGatewayLifecycle{},
		errors:  make(chan error),
	}
	monitoring := &selfMonitoring{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := waitForServerExit(context.Background(), appconfig.Defaults(), monitoring, supervisor, running)

	if err == nil || !strings.Contains(err.Error(), "managed component supervisor failed") ||
		!strings.Contains(err.Error(), "restart loop invariant failed") {
		t.Fatalf("wait error=%v", err)
	}
	if supervisor.shutdowns.Load() != 1 {
		t.Fatalf("supervisor shutdowns=%d want=1", supervisor.shutdowns.Load())
	}
}

func TestCombinedHealthPreservesSupervisorRestartState(t *testing.T) {
	restarting := errors.New("component restarting")
	combined := combinedHealth{
		healthFunc(func(context.Context) map[string]error {
			return map[string]error{"logs": restarting, "metrics": nil}
		}),
		healthFunc(func(context.Context) map[string]error {
			return map[string]error{
				"logs":    errors.New("Get http://127.0.0.1:19428/health: connection refused"),
				"metrics": errors.New("metrics backend unavailable"),
			}
		}),
	}

	health := combined.Health(context.Background())

	if !errors.Is(health["logs"], restarting) || health["metrics"] == nil ||
		health["metrics"].Error() != "metrics backend unavailable" {
		t.Fatalf("combined health=%v", health)
	}
}

func TestLoadLLMPricingUsesConfiguredOverrideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	override := `[{"model_pattern":"custom-enterprise-model","input_per_1k":2.5,"output_per_1k":5.0,"currency":"USD"}]`
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appconfig.Defaults()
	cfg.AI.PricingFile = path
	table, err := loadLLMPricing(cfg)
	if err != nil {
		t.Fatalf("loadLLMPricing error=%v", err)
	}
	cost, unpriced := table.Cost("custom-enterprise-model", 1000, 1000)
	if unpriced || cost != 7.5 {
		t.Fatalf("cost=%v unpriced=%v want 7.5,false", cost, unpriced)
	}
}
