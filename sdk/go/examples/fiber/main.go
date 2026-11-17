package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	lazyobserve "github.com/eimsound/lazy-observe/sdk/go"
	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "load" {
		os.Exit(runLoad(os.Args[2:], os.Stdout, os.Stderr))
	}

	dsn := envOrDefault("LO_DSN", "http://127.0.0.1:3100/p/default@replace-me")
	profiling := envBool("LO_PROFILING", true)
	captureLLMContent := envBool("LO_CAPTURE_LLM_CONTENT", false)
	lo := lazyobserve.Init(lazyobserve.Config{
		DSN:               dsn,
		ServiceName:       "lazy-observe-fiber-example",
		ExportInterval:    time.Second,
		Profiling:         &profiling,
		CaptureLLMContent: captureLLMContent,
	})
	if err := lo.Err(); err != nil {
		slog.Error("lazy-observe initialization failed; telemetry is degraded", "error", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() { _ = lo.Shutdown(shutdownCtx) }()

	slog.SetDefault(lo.Slog())
	db, err := lo.WrapDB("sqlite", "file:lazy-observe-example?mode=memory&cache=shared")
	if err != nil {
		slog.Error("open example database", "error", err)
		return
	}
	defer db.Close()

	app := newExampleApp(lo, db, envOrDefault("LO_LLM_URL", "http://127.0.0.1:3330"))
	go func() {
		port := envOrDefault("PORT", "3320")
		if err := app.Listen("127.0.0.1:" + port); err != nil {
			slog.Error("Fiber stopped", "error", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	_ = app.Shutdown()
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
