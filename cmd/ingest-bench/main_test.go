package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyGateThresholds(t *testing.T) {
	baseline := result{SamplesPerSecond: 100, LogsPerSecond: 50, P99LatencyMS: 10, MaxRSSBytes: 1000}
	data, _ := json.Marshal(baseline)
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	passing := result{SamplesPerSecond: 60, LogsPerSecond: 30, P99LatencyMS: 30, MaxRSSBytes: 1500}
	if err := applyGate(&passing, path); err != nil || !passing.GatePassed {
		t.Fatalf("boundary should pass: result=%#v err=%v", passing, err)
	}
	failing := result{SamplesPerSecond: 59, LogsPerSecond: 29, P99LatencyMS: 30.1, MaxRSSBytes: 1501, RequestsFailed: 2}
	if err := applyGate(&failing, path); err != nil || failing.GatePassed || len(failing.GateFailures) != 5 {
		t.Fatalf("regressions should fail: result=%#v err=%v", failing, err)
	}
}

func TestApplyGateRejectsFailedRequestsAndTailLatencyRegression(t *testing.T) {
	baseline := result{SamplesPerSecond: 100, LogsPerSecond: 50, P99LatencyMS: 1.0, MaxRSSBytes: 1000}
	data, _ := json.Marshal(baseline)
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	withFailures := result{
		SamplesPerSecond: 200,
		LogsPerSecond:    100,
		P99LatencyMS:     2.0,
		MaxRSSBytes:      500,
		RequestsFailed:   1,
	}
	if err := applyGate(&withFailures, path); err != nil {
		t.Fatal(err)
	}
	if withFailures.GatePassed || len(withFailures.GateFailures) != 1 ||
		!strings.Contains(withFailures.GateFailures[0], "failed requests") {
		t.Fatalf("failed requests must fail gate: result=%#v", withFailures)
	}

	withHighP99 := result{SamplesPerSecond: 200, LogsPerSecond: 100, P99LatencyMS: 10.5, MaxRSSBytes: 500}
	if err := applyGate(&withHighP99, path); err != nil {
		t.Fatal(err)
	}
	if withHighP99.GatePassed || len(withHighP99.GateFailures) != 1 ||
		!strings.Contains(withHighP99.GateFailures[0], "p99 latency") {
		t.Fatalf("p99 latency above floor budget must fail gate: result=%#v", withHighP99)
	}
}
