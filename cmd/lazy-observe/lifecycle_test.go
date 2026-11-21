package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
)

func TestCleanupStackRunsLIFOIdempotentlyWithoutSkippingOnError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stack := newCleanupStack(logger)

	var order []string
	errBoom := errors.New("boom")
	stack.Push("first", func() error {
		order = append(order, "first")
		return nil
	})
	stack.Push("second", func() error {
		order = append(order, "second")
		return errBoom
	})
	stack.Push("third", func() error {
		order = append(order, "third")
		return nil
	})

	err := stack.Close()
	if !errors.Is(err, errBoom) {
		t.Fatalf("Close() err=%v want wrapped %v", err, errBoom)
	}
	if !slices.Equal(order, []string{"third", "second", "first"}) {
		t.Fatalf("cleanup order=%v want [third second first]", order)
	}
	// Second Close is a no-op and returns the same result.
	if err2 := stack.Close(); !errors.Is(err2, errBoom) {
		t.Fatalf("second Close() err=%v", err2)
	}
	if len(order) != 3 {
		t.Fatalf("cleanup ran more than once: %v", order)
	}
}

func TestRunServerFailsFastOnPortConflictWithoutTouchingDataDir(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	dataDir := filepath.Join(t.TempDir(), "untouched-data")
	cfg := appconfig.Defaults()
	cfg.Listen = occupied.Addr().String()
	cfg.DataDir = dataDir

	if err := runServer(cfg); err == nil || !strings.Contains(err.Error(), "listen gateway") {
		t.Fatalf("runServer port conflict err=%v want listen gateway failure", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("runServer touched data_dir despite early port conflict: err=%v", err)
	}
}

func TestRunServerFailsFastOnOTLPGRPCPortConflictWithoutTouchingDataDir(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	dataDir := filepath.Join(t.TempDir(), "untouched-otlp-data")
	cfg := appconfig.Defaults()
	cfg.Listen = "127.0.0.1:0"
	cfg.OTLPGRPC.Enabled = true
	cfg.OTLPGRPC.Listen = occupied.Addr().String()
	cfg.DataDir = dataDir

	if err := runServer(cfg); err == nil || !strings.Contains(err.Error(), "listen otlp grpc") {
		t.Fatalf("runServer OTLP/gRPC port conflict err=%v want listen otlp grpc failure", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("runServer touched data_dir despite OTLP/gRPC port conflict: err=%v", err)
	}
}

func TestDoctorIsStrictlyReadOnly(t *testing.T) {
	t.Run("nonexistent data dir is not created", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "never-created")
		err := run([]string{
			"doctor",
			"--ha",
			"--data-dir", dataDir,
			"--standalone",
			"--metrics-query-url", "http://127.0.0.1:1",
			"--logs-url", "http://127.0.0.1:1",
			"--traces-url", "http://127.0.0.1:1",
		})
		if err == nil {
			t.Fatal("expected doctor failure for incomplete HA config")
		}
		if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
			t.Fatalf("doctor created data_dir: statErr=%v", statErr)
		}
	})

	t.Run("existing empty data dir stays completely empty", func(t *testing.T) {
		dataDir := t.TempDir()
		err := run([]string{
			"doctor",
			"--ha",
			"--data-dir", dataDir,
			"--standalone",
			"--metrics-query-url", "http://127.0.0.1:1",
			"--metrics-ingest-url", "http://127.0.0.1:1",
			"--logs-url", "http://127.0.0.1:1",
			"--traces-url", "http://127.0.0.1:1",
		})
		if err == nil {
			t.Fatal("expected doctor failure for incomplete HA config")
		}
		entries, readErr := os.ReadDir(dataDir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("doctor wrote files into data_dir: %v", entries)
		}
	})
}

func TestRequiredEmbeddedBinariesMatchesTopologyAndScrapeMode(t *testing.T) {
	allInOne := appconfig.Defaults()
	if got := requiredEmbeddedBinaries(allInOne, "http://127.0.0.1:18428"); !slices.Equal(
		got,
		[]string{"victoria-metrics-prod", "victoria-logs-prod", "victoria-traces-prod", "vmagent-prod"},
	) {
		t.Fatalf("all-in-one auto bins=%v", got)
	}

	allInOneNoScrape := appconfig.Defaults()
	allInOneNoScrape.Scrape.Mode = appconfig.ScrapeModeDisabled
	if got := requiredEmbeddedBinaries(allInOneNoScrape, "http://127.0.0.1:18428"); !slices.Equal(
		got,
		[]string{"victoria-metrics-prod", "victoria-logs-prod", "victoria-traces-prod"},
	) {
		t.Fatalf("all-in-one scrape disabled bins=%v", got)
	}

	standalone := appconfig.Defaults()
	standalone.Topology = appconfig.TopologyStandalone
	if got := requiredEmbeddedBinaries(standalone, "http://vminsert:8480"); !slices.Equal(
		got,
		[]string{"vmagent-prod"},
	) {
		t.Fatalf("standalone with ingest bins=%v", got)
	}
	if got := requiredEmbeddedBinaries(standalone, ""); len(got) != 0 {
		t.Fatalf("standalone without ingest bins=%v want empty", got)
	}

	haDisabledScrape := appconfig.Defaults()
	haDisabledScrape.Topology = appconfig.TopologyHA
	haDisabledScrape.Scrape.Mode = appconfig.ScrapeModeDisabled
	if got := requiredEmbeddedBinaries(haDisabledScrape, "http://vminsert:8480"); len(got) != 0 {
		t.Fatalf("ha with scrape.mode=disabled bins=%v want empty", got)
	}
}

func TestServerServicesCloseWaitsForBackgroundGoroutines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	services := &serverServices{
		bgCancel:        cancel,
		shutdownTimeout: 2 * time.Second,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	started := make(chan struct{})
	var exited atomic.Bool
	services.startBackground(ctx, func(runCtx context.Context) {
		close(started)
		<-runCtx.Done()
		time.Sleep(25 * time.Millisecond)
		exited.Store(true)
	})
	<-started
	services.Close()
	if !exited.Load() {
		t.Fatal("serverServices.Close returned before background goroutine finished cleanup")
	}
}
