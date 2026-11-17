// Command ingest-bench starts a complete lazy-observe instance and measures
// authenticated OTLP/HTTP metrics and logs ingestion through the public gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

type result struct {
	Version          int      `json:"version"`
	DurationSeconds  float64  `json:"duration_seconds"`
	Concurrency      int      `json:"concurrency"`
	BatchSize        int      `json:"batch_size"`
	MetricSamples    uint64   `json:"metric_samples"`
	LogRecords       uint64   `json:"log_records"`
	SamplesPerSecond float64  `json:"samples_per_second"`
	LogsPerSecond    float64  `json:"logs_per_second"`
	P99LatencyMS     float64  `json:"p99_gateway_latency_ms"`
	MaxRSSBytes      uint64   `json:"max_rss_bytes"`
	RequestsFailed   uint64   `json:"requests_failed"`
	GatePassed       bool     `json:"gate_passed"`
	GateFailures     []string `json:"gate_failures,omitempty"`
}

func main() {
	var binary, binDir, output, baseline string
	var duration time.Duration
	var concurrency, batchSize int
	flag.StringVar(&binary, "binary", "bin/lazy-observe", "lazy-observe server binary")
	flag.StringVar(&binDir, "storage-bin-dir", ".cache/storage-bin", "embedded storage binary directory")
	flag.DurationVar(&duration, "duration", 30*time.Second, "load duration")
	flag.IntVar(&concurrency, "concurrency", 4, "number of workers (split evenly between signals)")
	flag.IntVar(&batchSize, "batch-size", 200, "samples or log records per request")
	flag.StringVar(&output, "output", "", "optional JSON output file")
	flag.StringVar(&baseline, "baseline", "", "optional baseline JSON used for the regression gate")
	flag.Parse()
	if duration <= 0 || concurrency < 2 || batchSize <= 0 {
		fatal(errors.New("duration and batch-size must be positive and concurrency must be at least 2"))
	}
	r, err := run(binary, binDir, duration, concurrency, batchSize)
	if err != nil {
		fatal(err)
	}
	r.GatePassed = true
	if baseline != "" {
		if err := applyGate(&r, baseline); err != nil {
			fatal(err)
		}
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fatal(err)
	}
	if output != "" {
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(output, append(data, '\n'), 0o644); err != nil {
			fatal(err)
		}
	}
	fmt.Println(string(data))
	fmt.Fprintf(
		os.Stderr,
		"ingest bench: %.0f samples/s, %.0f logs/s, p99 %.2f ms, max RSS %.1f MiB, failed requests %d\n",
		r.SamplesPerSecond,
		r.LogsPerSecond,
		r.P99LatencyMS,
		float64(r.MaxRSSBytes)/(1<<20),
		r.RequestsFailed,
	)
	if !r.GatePassed {
		fmt.Fprintln(os.Stderr, "regression gate failed: "+strings.Join(r.GateFailures, "; "))
		os.Exit(1)
	}
}

func run(binary, binDir string, duration time.Duration, concurrency, batchSize int) (result, error) {
	dataDir, err := os.MkdirTemp("", "lazy-observe-bench-")
	if err != nil {
		return result{}, err
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	metaPath := filepath.Join(dataDir, "meta", "lazy-observe.db")
	store, err := meta.OpenStore(context.Background(), metaPath)
	if err != nil {
		return result{}, err
	}
	project, err := store.CreateProject(context.Background(), "Benchmark", "benchmark")
	if err != nil {
		store.Close()
		return result{}, err
	}
	token, err := store.CreateProjectAPIToken(context.Background(), project.ID)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return result{}, err
	}
	addr, err := freeAddress()
	if err != nil {
		return result{}, err
	}
	serverLog, err := os.CreateTemp("", "lazy-observe-bench-server-*.log")
	if err != nil {
		return result{}, err
	}
	defer func() { _ = os.Remove(serverLog.Name()) }()
	defer func() { _ = serverLog.Close() }()
	cmd := exec.Command(binary, "server", "--listen", addr, "--storage-bin-dir", binDir, "--data-dir", dataDir)
	cmd.Env = append(os.Environ(), "LO_META_DB_URL=sqlite://"+metaPath)
	cmd.Stdout, cmd.Stderr = serverLog, serverLog
	if err := cmd.Start(); err != nil {
		return result{}, fmt.Errorf("start lazy-observe: %w", err)
	}
	stopped := false
	defer func() {
		if !stopped && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()
	baseURL := "http://" + addr
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConns: concurrency * 2, MaxIdleConnsPerHost: concurrency * 2},
		Timeout:   30 * time.Second,
	}
	if err := waitHealthy(client, baseURL, 45*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		stopped = true
		content, _ := os.ReadFile(serverLog.Name())
		return result{}, fmt.Errorf("server readiness: %w\n%s", err, content)
	}
	metricsBody := metricsPayload(batchSize)
	logsBody := logsPayload(batchSize)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var metricCount, logCount, failures atomic.Uint64
	latencies := make([]float64, 0, 1<<16)
	var latencyMu sync.Mutex
	var maxRSS atomic.Uint64
	var workers sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		isMetrics := worker%2 == 0
		workers.Add(1)
		go func() {
			defer workers.Done()
			path, body := "/v1/logs", logsBody
			if isMetrics {
				path, body = "/v1/metrics", metricsBody
			}
			for ctx.Err() == nil {
				requestStart := time.Now()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, requestErr := client.Do(req)
				if requestErr == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if ctx.Err() != nil {
					return
				}
				latency := float64(time.Since(requestStart).Microseconds()) / 1000
				latencyMu.Lock()
				latencies = append(latencies, latency)
				latencyMu.Unlock()
				if requestErr != nil || resp.StatusCode/100 != 2 {
					failures.Add(1)
					continue
				}
				if isMetrics {
					metricCount.Add(uint64(batchSize))
				} else {
					logCount.Add(uint64(batchSize))
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if rss, rssErr := processRSS(cmd.Process.Pid); rssErr == nil {
				for old := maxRSS.Load(); rss > old && !maxRSS.CompareAndSwap(old, rss); old = maxRSS.Load() {
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	workers.Wait()
	elapsed := time.Since(start).Seconds()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		return result{}, fmt.Errorf("stop lazy-observe: %w", err)
	}
	stopped = true
	sort.Float64s(latencies)
	p99 := 0.0
	if len(latencies) != 0 {
		p99 = latencies[(len(latencies)*99-1)/100]
	}
	return result{Version: 1, DurationSeconds: elapsed, Concurrency: concurrency, BatchSize: batchSize,
		MetricSamples: metricCount.Load(), LogRecords: logCount.Load(),
		SamplesPerSecond: float64(metricCount.Load()) / elapsed, LogsPerSecond: float64(logCount.Load()) / elapsed,
		P99LatencyMS: p99, MaxRSSBytes: maxRSS.Load(), RequestsFailed: failures.Load()}, nil
}

func applyGate(current *result, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}
	var baseline result
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("parse baseline: %w", err)
	}
	if current.SamplesPerSecond < baseline.SamplesPerSecond*0.6 {
		current.GateFailures = append(
			current.GateFailures,
			fmt.Sprintf("samples/s %.0f < baseline %.0f x 0.6", current.SamplesPerSecond, baseline.SamplesPerSecond),
		)
	}
	if current.LogsPerSecond < baseline.LogsPerSecond*0.6 {
		current.GateFailures = append(
			current.GateFailures,
			fmt.Sprintf("logs/s %.0f < baseline %.0f x 0.6", current.LogsPerSecond, baseline.LogsPerSecond),
		)
	}
	if baseline.MaxRSSBytes > 0 && current.MaxRSSBytes > baseline.MaxRSSBytes*3/2 {
		current.GateFailures = append(
			current.GateFailures,
			fmt.Sprintf("RSS %d > baseline %d x 1.5", current.MaxRSSBytes, baseline.MaxRSSBytes),
		)
	}
	current.GatePassed = len(current.GateFailures) == 0
	return nil
}

func freeAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	return addr, listener.Close()
}

func waitHealthy(client *http.Client, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out waiting for /healthz")
}

func processRSS(pid int) (uint64, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				kb, parseErr := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024, parseErr
			}
		}
		return 0, errors.New("VmRSS not found")
	}
	data, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return kb * 1024, err
}

func metricsPayload(batchSize int) []byte {
	now := uint64(time.Now().UnixNano())
	points := make([]*metricsdata.NumberDataPoint, batchSize)
	for i := range points {
		points[i] = &metricsdata.NumberDataPoint{
			TimeUnixNano: now,
			Attributes:   []*commonpb.KeyValue{{Key: "series", Value: stringValue(strconv.Itoa(i))}},
			Value:        &metricsdata.NumberDataPoint_AsInt{AsInt: int64(i)},
		}
	}
	request := &metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsdata.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{Attributes: serviceAttributes()},
				ScopeMetrics: []*metricsdata.ScopeMetrics{
					{
						Metrics: []*metricsdata.Metric{
							{
								Name: "lazy_observe_bench_samples",
								Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{DataPoints: points}},
							},
						},
					},
				},
			},
		},
	}
	data, _ := proto.Marshal(request)
	return data
}

func logsPayload(batchSize int) []byte {
	now := uint64(time.Now().UnixNano())
	records := make([]*logdata.LogRecord, batchSize)
	for i := range records {
		records[i] = &logdata.LogRecord{
			TimeUnixNano: now,
			SeverityText: "INFO",
			Body:         stringValue("lazy-observe ingest benchmark log record"),
			Attributes:   []*commonpb.KeyValue{{Key: "sequence", Value: stringValue(strconv.Itoa(i))}},
		}
	}
	request := &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*logdata.ResourceLogs{
			{
				Resource:  &resourcepb.Resource{Attributes: serviceAttributes()},
				ScopeLogs: []*logdata.ScopeLogs{{LogRecords: records}},
			},
		},
	}
	data, _ := proto.Marshal(request)
	return data
}

func serviceAttributes() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{{Key: "service.name", Value: stringValue("lazy-observe-ingest-bench")}}
}

func stringValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ingest-bench:", err)
	os.Exit(2)
}
