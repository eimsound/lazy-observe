package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const defaultGateConfigPath = "bench/performance-gates.json"

type gateConfig struct {
	Version int `json:"version"`
	Release struct {
		MaxBinaryBytes  int64 `json:"max_binary_bytes"`
		MaxIdleRSSBytes int64 `json:"max_idle_rss_bytes"`
	} `json:"release"`
	Query struct {
		WarmupRequests   int `json:"warmup_requests"`
		MeasuredRequests int `json:"measured_requests"`
		MediumFixture    struct {
			Issues int `json:"issues"`
			Logs   int `json:"logs"`
			Traces int `json:"traces"`
		} `json:"medium_fixture"`
		MaxP95MS map[string]float64 `json:"max_p95_ms"`
	} `json:"query"`
	HAScaling struct {
		SettleSeconds               float64 `json:"settle_seconds"`
		WarmupSeconds               float64 `json:"warmup_seconds"`
		RecoverySeconds             float64 `json:"recovery_seconds"`
		DurationSeconds             float64 `json:"duration_seconds"`
		ScaledConcurrencyPerReplica int     `json:"scaled_concurrency_per_replica"`
		GOMAXPROCSPerReplica        int     `json:"gomaxprocs_per_replica"`
		BatchSize                   int     `json:"batch_size"`
		MinReplicaEfficiency        float64 `json:"min_replica_efficiency"`
		MinPassingRoundRatio        float64 `json:"min_passing_round_ratio"`
		MinTargetBalanceRatio       float64 `json:"min_target_balance_ratio"`
		MinBaselineRatio            float64 `json:"min_baseline_ratio"`
		MinUpstreamHeadroomRatio    float64 `json:"min_upstream_headroom_ratio"`
		MaxFailedRequests           uint64  `json:"max_failed_requests"`
	} `json:"ha_scaling"`
}

func loadGateConfig(path string) (gateConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return gateConfig{}, fmt.Errorf("read gate config: %w", err)
	}
	var cfg gateConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return gateConfig{}, fmt.Errorf("parse gate config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return gateConfig{}, fmt.Errorf("validate gate config: %w", err)
	}
	return cfg, nil
}

func (cfg gateConfig) validate() error {
	if cfg.Version != 1 {
		return fmt.Errorf("unsupported version %d", cfg.Version)
	}
	if cfg.Release.MaxBinaryBytes <= 0 || cfg.Release.MaxIdleRSSBytes <= 0 {
		return fmt.Errorf("release size and idle RSS budgets must be positive")
	}
	if cfg.Query.WarmupRequests < 0 || cfg.Query.MeasuredRequests <= 0 {
		return fmt.Errorf("query request counts must be non-negative with measured_requests positive")
	}
	if cfg.Query.MediumFixture.Issues < 1000 || cfg.Query.MediumFixture.Logs < 1000 ||
		cfg.Query.MediumFixture.Traces < 1000 {
		return fmt.Errorf("query.medium_fixture must define at least 1000 issues, logs, and traces")
	}
	if cfg.Query.MediumFixture.Issues > 50_000 || cfg.Query.MediumFixture.Logs > 1_000_000 ||
		cfg.Query.MediumFixture.Traces > 10_000 {
		return fmt.Errorf(
			"query.medium_fixture exceeds the controlled gate maximum of 50000 issues, 1000000 logs, or 10000 traces",
		)
	}
	for _, name := range []string{"issues", "logs", "trace"} {
		if cfg.Query.MaxP95MS[name] <= 0 {
			return fmt.Errorf("query.max_p95_ms.%s must be positive", name)
		}
	}
	if cfg.HAScaling.SettleSeconds < 0 || cfg.HAScaling.SettleSeconds > 60 || cfg.HAScaling.WarmupSeconds < 0 ||
		cfg.HAScaling.RecoverySeconds < 0 || cfg.HAScaling.RecoverySeconds > 30 || cfg.HAScaling.DurationSeconds <= 0 {
		return fmt.Errorf(
			"ha_scaling settle/recovery/warmup durations must be non-negative and bounded with duration_seconds positive",
		)
	}
	if cfg.HAScaling.ScaledConcurrencyPerReplica < 2 || cfg.HAScaling.ScaledConcurrencyPerReplica%2 != 0 {
		return fmt.Errorf("ha_scaling.scaled_concurrency_per_replica must be an even integer of at least 2")
	}
	if cfg.HAScaling.GOMAXPROCSPerReplica <= 0 {
		return fmt.Errorf("ha_scaling.gomaxprocs_per_replica must be positive")
	}
	if cfg.HAScaling.BatchSize <= 0 {
		return fmt.Errorf("ha_scaling.batch_size must be positive")
	}
	if cfg.HAScaling.MinReplicaEfficiency <= 0 || cfg.HAScaling.MinReplicaEfficiency > 1 {
		return fmt.Errorf("ha_scaling.min_replica_efficiency must be in (0, 1]")
	}
	if cfg.HAScaling.MinPassingRoundRatio <= 0 || cfg.HAScaling.MinPassingRoundRatio > 1 {
		return fmt.Errorf("ha_scaling.min_passing_round_ratio must be in (0, 1]")
	}
	if cfg.HAScaling.MinTargetBalanceRatio <= 0 || cfg.HAScaling.MinTargetBalanceRatio > 1 {
		return fmt.Errorf("ha_scaling.min_target_balance_ratio must be in (0, 1]")
	}
	if cfg.HAScaling.MinBaselineRatio <= 0 || cfg.HAScaling.MinBaselineRatio > 1 {
		return fmt.Errorf("ha_scaling.min_baseline_ratio must be in (0, 1]")
	}
	if cfg.HAScaling.MinUpstreamHeadroomRatio <= 1 {
		return fmt.Errorf("ha_scaling.min_upstream_headroom_ratio must be greater than 1")
	}
	return nil
}
