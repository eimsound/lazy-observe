package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
)

func TestRetentionJanitorErrorReceiptsUsesEffectiveReceiptRetention(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name     string
		mutate   func(*appconfig.Config)
		expected time.Duration
	}{
		{
			name:     "inherits retention.logs when unset",
			mutate:   func(cfg *appconfig.Config) { cfg.Retention.Logs = appconfig.Duration(21 * 24 * time.Hour) },
			expected: 21 * 24 * time.Hour,
		},
		{
			name: "explicit receipt retention overrides retention.logs",
			mutate: func(cfg *appconfig.Config) {
				cfg.Retention.Logs = appconfig.Duration(7 * 24 * time.Hour)
				cfg.ErrorInbox.ReceiptRetention = appconfig.Duration(30 * 24 * time.Hour)
			},
			expected: 30 * 24 * time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := appconfig.Defaults()
			tc.mutate(&cfg)
			retentionJanitor := buildRetentionJanitor(cfg, logger, &serverMetadata{})
			got, ok := retentionJanitor.Retention("error_receipts")
			if !ok || got != tc.expected {
				t.Fatalf("error_receipts retention=%v registered=%v want %v", got, ok, tc.expected)
			}
		})
	}
}

func TestSignalStorageEndpointsStandaloneWarningExplainsReceiptRetention(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	cfg := appconfig.Defaults()
	cfg.Standalone = true
	cfg.Storage.Metrics.QueryURL = "http://metrics:8428"
	cfg.Storage.Logs.URL = "http://logs:9428"
	cfg.Storage.Traces.URL = "http://traces:10428"
	cfg.ErrorInbox.ReceiptRetention = appconfig.Duration(30 * 24 * time.Hour)

	signalStorageEndpoints(cfg, logger)

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("decode warning %q: %v", buf.String(), err)
	}
	msg, _ := record["msg"].(string)
	for _, want := range []string{
		"retention.{metrics,logs,traces} apply only to embedded storage",
		"error_inbox.receipt_retention",
		"external VictoriaLogs retention",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("standalone warning %q missing %q", msg, want)
		}
	}
	if got := record["error_inbox_receipt_retention"]; got != float64(30*24*time.Hour) {
		t.Fatalf("error_inbox_receipt_retention attr=%v want %d", got, 30*24*time.Hour)
	}
	if got := record["error_inbox_receipt_retention_inherited"]; got != false {
		t.Fatalf("error_inbox_receipt_retention_inherited attr=%v want false", got)
	}
}

func TestSignalStorageEndpointsAllInOneDoesNotWarnAboutRetention(t *testing.T) {
	var buf bytes.Buffer
	signalStorageEndpoints(appconfig.Defaults(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if buf.Len() != 0 {
		t.Fatalf("all-in-one must not log the standalone retention warning: %s", buf.String())
	}
}
