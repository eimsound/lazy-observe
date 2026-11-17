package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyGateThresholds(t *testing.T) {
	baseline := result{SamplesPerSecond: 100, LogsPerSecond: 50, MaxRSSBytes: 1000}
	data, _ := json.Marshal(baseline)
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	passing := result{SamplesPerSecond: 60, LogsPerSecond: 30, MaxRSSBytes: 1500}
	if err := applyGate(&passing, path); err != nil || !passing.GatePassed {
		t.Fatalf("boundary should pass: result=%#v err=%v", passing, err)
	}
	failing := result{SamplesPerSecond: 59, LogsPerSecond: 29, MaxRSSBytes: 1501}
	if err := applyGate(&failing, path); err != nil || failing.GatePassed || len(failing.GateFailures) != 3 {
		t.Fatalf("regressions should fail: result=%#v err=%v", failing, err)
	}
}
