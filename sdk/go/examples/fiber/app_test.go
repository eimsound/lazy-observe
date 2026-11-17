package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	lazyobserve "github.com/eimsound/lazy-observe/sdk/go"
)

const testTraceID = "11111111111111111111111111111111"

func TestOrderTraceKeepsIncomingTraceID(t *testing.T) {
	app := testApp(t)
	req := httptest.NewRequest(http.MethodGet, "/trace/order-42", nil)
	req.Header.Set("traceparent", "00-"+testTraceID+"-2222222222222222-01")
	resp, err := app.Test(req, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body struct {
		OrderID string `json:"order_id"`
		Items   int    `json:"items"`
		Total   int    `json:"total"`
		TraceID string `json:"trace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.OrderID != "order-42" || body.Items != 3 || body.Total != 126 || body.TraceID != testTraceID {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestPerformanceWorkloadsExposeLatencyAndErrors(t *testing.T) {
	app := testApp(t)
	tests := []struct {
		path       string
		status     int
		workload   string
		minElapsed int64
	}{
		{path: "/performance/fast", status: http.StatusOK, workload: "fast", minElapsed: 4},
		{path: "/performance/slow", status: http.StatusOK, workload: "slow", minElapsed: 140},
		{path: "/performance/slow?delay_ms=400", status: http.StatusOK, workload: "slow", minElapsed: 390},
		{path: "/performance/error", status: http.StatusServiceUnavailable, workload: "error", minElapsed: 25},
	}
	for _, tt := range tests {
		t.Run(tt.workload, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tt.path, nil), 2_000)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			var body struct {
				Workload   string `json:"workload"`
				DurationMS int64  `json:"duration_ms"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Workload != tt.workload || body.DurationMS < tt.minElapsed {
				t.Fatalf("unexpected response: %+v", body)
			}
		})
	}
}

func TestLoadPathMix(t *testing.T) {
	want := map[string]int{
		"/performance/fast":  5,
		"/performance/slow":  3,
		"/performance/error": 1,
		"/trace/load-4":      1,
	}
	got := make(map[string]int)
	for i := int64(1); i <= 10; i++ {
		got[loadPath(i)]++
	}
	for path, count := range want {
		if got[path] != count {
			t.Fatalf("%s count = %d, want %d (all: %#v)", path, got[path], count, got)
		}
	}
}

func testApp(t *testing.T) interface {
	Test(*http.Request, ...int) (*http.Response, error)
} {
	t.Helper()
	profiling := false
	lo := lazyobserve.Init(lazyobserve.Config{DSN: "invalid", Profiling: &profiling})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = lo.Shutdown(ctx)
	})
	db, err := sql.Open("sqlite", "file:lazy-observe-example-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newExampleApp(lo, db, "http://127.0.0.1:1")
}
