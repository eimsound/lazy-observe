// Command ingest-bench starts a complete lazy-observe instance and measures
// authenticated OTLP/HTTP metrics, logs, and traces ingestion through the
// public gateway with varied payloads, process-tree RSS accounting, p99 tail
// latency gating, and post-ingest real-storage read-back verification.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

type latencyPercentiles struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}

type result struct {
	Version              int                           `json:"version"`
	GitCommit            string                        `json:"git_commit,omitempty"`
	GeneratedAt          string                        `json:"generated_at,omitempty"`
	RunID                string                        `json:"run_id,omitempty"`
	DurationSeconds      float64                       `json:"duration_seconds"`
	Concurrency          int                           `json:"concurrency"`
	BatchSize            int                           `json:"batch_size"`
	PayloadPoolSize      int                           `json:"payload_pool_size,omitempty"`
	Compression          string                        `json:"compression,omitempty"`
	MetricSamples        uint64                        `json:"metric_samples"`
	LogRecords           uint64                        `json:"log_records"`
	TraceSpans           uint64                        `json:"trace_spans,omitempty"`
	SamplesPerSecond     float64                       `json:"samples_per_second"`
	LogsPerSecond        float64                       `json:"logs_per_second"`
	SpansPerSecond       float64                       `json:"spans_per_second,omitempty"`
	P50LatencyMS         float64                       `json:"p50_gateway_latency_ms,omitempty"`
	P95LatencyMS         float64                       `json:"p95_gateway_latency_ms,omitempty"`
	P99LatencyMS         float64                       `json:"p99_gateway_latency_ms"`
	MaxP99LatencyMS      float64                       `json:"max_p99_gateway_latency_ms,omitempty"`
	SignalLatency        map[string]latencyPercentiles `json:"signal_latency,omitempty"`
	MaxSignalP99MS       map[string]float64            `json:"max_signal_p99_ms,omitempty"`
	MaxRSSBytes          uint64                        `json:"max_rss_bytes"`
	MaxRSSLimitBytes     uint64                        `json:"max_rss_limit_bytes,omitempty"`
	MaxMainRSSBytes      uint64                        `json:"max_main_rss_bytes,omitempty"`
	MaxComponentRSSBytes map[string]uint64             `json:"max_component_rss_bytes,omitempty"`
	PeakProcessRSS       []processRSSSample            `json:"peak_process_rss,omitempty"`
	RSSIncludesChildren  bool                          `json:"rss_includes_children,omitempty"`
	RequestsFailed       uint64                        `json:"requests_failed"`
	Readback             readbackResult                `json:"readback"`
	GatePassed           bool                          `json:"gate_passed"`
	GateFailures         []string                      `json:"gate_failures,omitempty"`
}

const (
	p99LatencyMultiplier = 3.0
	minP99BudgetMS       = 10.0
	otlpResponseMaxBytes = 1 << 20
	maxSampleTraceIDs    = 3
	rssSampleInterval    = 500 * time.Millisecond
)

func main() {
	var binary, binDir, output, baseline, compression string
	var duration, readbackTimeout time.Duration
	var concurrency, batchSize, poolSize int
	flag.StringVar(&binary, "binary", "bin/lazy-observe", "lazy-observe server binary")
	flag.StringVar(&binDir, "storage-bin-dir", ".cache/storage-bin", "embedded storage binary directory")
	flag.DurationVar(&duration, "duration", 30*time.Second, "load duration")
	flag.IntVar(&concurrency, "concurrency", 4, "number of workers (each worker rotates metrics, logs, and traces)")
	flag.IntVar(&batchSize, "batch-size", 200, "samples, log records, or spans per request")
	flag.IntVar(&poolSize, "payload-pool-size", defaultPayloadPoolSize, "number of varied payload templates per signal")
	flag.StringVar(&compression, "compression", "none", "OTLP/HTTP request compression: none or gzip")
	flag.DurationVar(
		&readbackTimeout,
		"readback-timeout",
		defaultReadbackTimeout,
		"maximum time to verify real-storage read-back after ingest",
	)
	flag.StringVar(&output, "output", "", "optional JSON output file")
	flag.StringVar(&baseline, "baseline", "", "optional baseline JSON used for the regression gate")
	flag.Parse()
	normalizedCompression, err := normalizeCompression(compression)
	if err != nil {
		fatal(err)
	}
	if duration <= 0 || concurrency < 2 || batchSize <= 0 || poolSize <= 0 || readbackTimeout <= 0 {
		fatal(
			errors.New(
				"duration, batch-size, payload-pool-size, and readback-timeout must be positive and concurrency must be at least 2",
			),
		)
	}
	r, err := run(binary, binDir, duration, concurrency, batchSize, poolSize, normalizedCompression, readbackTimeout)
	if err != nil {
		fatal(err)
	}
	r.GitCommit = currentGitCommit()
	r.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	r.GatePassed = true
	if baseline != "" {
		if err := applyGate(&r, baseline); err != nil {
			fatal(err)
		}
	} else {
		applyStandaloneGate(&r)
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
		"ingest bench: run_id=%s %.0f samples/s, %.0f logs/s, %.0f spans/s, p99 %.2f ms (metrics %.2f, logs %.2f, traces %.2f), "+
			"max tree RSS %.1f MiB (main %.1f MiB, processes=%d), failed requests %d, readback=%v\n",
		r.RunID,
		r.SamplesPerSecond,
		r.LogsPerSecond,
		r.SpansPerSecond,
		r.P99LatencyMS,
		r.SignalLatency["metrics"].P99MS,
		r.SignalLatency["logs"].P99MS,
		r.SignalLatency["traces"].P99MS,
		float64(r.MaxRSSBytes)/(1<<20),
		float64(r.MaxMainRSSBytes)/(1<<20),
		len(r.PeakProcessRSS),
		r.RequestsFailed,
		r.Readback.Verified,
	)
	if !r.GatePassed {
		fmt.Fprintln(os.Stderr, "regression gate failed: "+strings.Join(r.GateFailures, "; "))
		os.Exit(1)
	}
}

func run(
	binary, binDir string,
	duration time.Duration,
	concurrency, batchSize, poolSize int,
	compression string,
	readbackTimeout time.Duration,
) (result, error) {
	runID, runSalt, err := newRunID()
	if err != nil {
		return result{}, err
	}
	pool, err := newPayloadPool(runID, runSalt, batchSize, poolSize)
	if err != nil {
		return result{}, err
	}
	dataDir, err := os.MkdirTemp("", "lazy-observe-bench-")
	if err != nil {
		return result{}, err
	}
	defer func() { _ = os.RemoveAll(dataDir) }()

	metaPath := filepath.Join(dataDir, "meta", "lazy-observe.db")
	ingestToken, readToken, projectSlug, err := setupBenchMetadata(metaPath)
	if err != nil {
		return result{}, err
	}
	addr, err := freeAddress()
	if err != nil {
		return result{}, err
	}
	portOffset, err := findAvailableComponentPortOffset()
	if err != nil {
		return result{}, err
	}
	serverLog, err := os.CreateTemp("", "lazy-observe-bench-server-*.log")
	if err != nil {
		return result{}, err
	}
	defer func() { _ = os.Remove(serverLog.Name()) }()
	defer func() { _ = serverLog.Close() }()

	cmdArgs := []string{
		"server",
		"--listen", addr,
		"--storage-bin-dir", binDir,
		"--data-dir", dataDir,
	}
	if portOffset > 0 {
		cmdArgs = append(cmdArgs, "--component-port-offset", strconv.Itoa(portOffset))
	}
	cmd := exec.Command(binary, cmdArgs...)
	cmd.Env = append(os.Environ(), "LO_META_DB_URL=sqlite://"+metaPath)
	cmd.Stdout, cmd.Stderr = serverLog, serverLog
	if err := cmd.Start(); err != nil {
		return result{}, fmt.Errorf("start lazy-observe: %w", err)
	}
	stopped := false
	stopServer := func() error {
		if stopped || cmd.Process == nil {
			return nil
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return cmd.Wait()
	}
	defer func() {
		_ = stopServer()
	}()

	baseURL := "http://" + addr
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConns: concurrency * 4, MaxIdleConnsPerHost: concurrency * 4},
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	if err := waitReady(client, baseURL, 60*time.Second); err != nil {
		_ = stopServer()
		content, _ := os.ReadFile(serverLog.Name())
		return result{}, fmt.Errorf("server readiness: %w\n%s", err, content)
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var metricCount, logCount, spanCount, failures atomic.Uint64
	var metricServiceMask atomic.Uint64
	var requestSeq atomic.Uint64
	allLatencies := make([]time.Duration, 0, 1<<16)
	signalLatencies := map[signalKind][]time.Duration{
		signalMetrics: make([]time.Duration, 0, 1<<14),
		signalLogs:    make([]time.Duration, 0, 1<<14),
		signalTraces:  make([]time.Duration, 0, 1<<14),
	}
	var latencyMu sync.Mutex

	var traceSampleMu sync.Mutex
	var firstTraceID, midTraceID, lastTraceID, sampleTraceService string

	var rssMu sync.Mutex
	var maxTreeRSS uint64
	var maxMainRSS uint64
	maxComponentRSS := make(map[string]uint64)
	var peakProcesses []processRSSSample
	recordRSS := func() {
		snapshot, rssErr := sampleProcessTreeRSS(cmd.Process.Pid)
		if rssErr != nil {
			return
		}
		rssMu.Lock()
		defer rssMu.Unlock()
		if snapshot.TotalRSSBytes > maxTreeRSS {
			maxTreeRSS = snapshot.TotalRSSBytes
			peakProcesses = snapshot.Processes
		}
		if snapshot.RootRSSBytes > maxMainRSS {
			maxMainRSS = snapshot.RootRSSBytes
		}
		for component, rss := range snapshot.ComponentRSSBytes {
			if rss > maxComponentRSS[component] {
				maxComponentRSS[component] = rss
			}
		}
	}

	var workers sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		workerID := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			var localSeq uint64
			var workBuf []byte
			var gzBuf bytes.Buffer
			var gzWriter *gzip.Writer
			for ctx.Err() == nil {
				sig := signalKind((localSeq + uint64(workerID)) % 3)
				localSeq++
				globalSeq := requestSeq.Add(1)
				nowNano := uint64(time.Now().UnixNano())

				var traceID, traceService string
				var metricSvcIndex int
				switch sig {
				case signalMetrics:
					workBuf, metricSvcIndex = pool.materializeMetric(workBuf, workerID, globalSeq, nowNano)
				case signalLogs:
					workBuf = pool.materializeLog(workBuf, workerID, globalSeq, nowNano)
				case signalTraces:
					workBuf, traceID, traceService = pool.materializeTrace(workBuf, workerID, globalSeq, nowNano)
				}

				reqBody, contentEncoding, encErr := encodeOTLPBody(compression, workBuf, &gzBuf, &gzWriter)
				if encErr != nil {
					failures.Add(1)
					continue
				}

				requestStart := time.Now()
				requestErr := postOTLP(ctx, client, baseURL+sig.path(), ingestToken, reqBody, contentEncoding, sig)
				if ctx.Err() != nil {
					return
				}
				elapsedReq := time.Since(requestStart)
				latencyMu.Lock()
				allLatencies = append(allLatencies, elapsedReq)
				signalLatencies[sig] = append(signalLatencies[sig], elapsedReq)
				latencyMu.Unlock()

				if requestErr != nil {
					failures.Add(1)
					continue
				}
				switch sig {
				case signalMetrics:
					metricCount.Add(uint64(batchSize))
					metricServiceMask.Or(uint64(1) << uint(metricSvcIndex))
				case signalLogs:
					logCount.Add(uint64(batchSize))
				case signalTraces:
					spanCount.Add(uint64(batchSize))
					traceSampleMu.Lock()
					if firstTraceID == "" {
						firstTraceID = traceID
						sampleTraceService = traceService
					} else if midTraceID == "" && time.Since(start) >= duration/2 {
						midTraceID = traceID
					}
					lastTraceID = traceID
					traceSampleMu.Unlock()
				}
			}
		}()
	}

	var rssWg sync.WaitGroup
	rssWg.Add(1)
	go func() {
		defer rssWg.Done()
		ticker := time.NewTicker(rssSampleInterval)
		defer ticker.Stop()
		for {
			recordRSS()
			select {
			case <-ctx.Done():
				recordRSS()
				return
			case <-ticker.C:
			}
		}
	}()

	workers.Wait()
	elapsed := time.Since(start).Seconds()
	rssWg.Wait()

	sampleTraceIDs := collectUniqueStrings(firstTraceID, midTraceID, lastTraceID)
	expectedSeries := int64(bits.OnesCount64(metricServiceMask.Load()) * batchSize)
	expectedMinLogs := int64(logCount.Load())
	readback := verifyReadback(context.Background(), client, readbackConfig{
		baseURL:               baseURL,
		readToken:             readToken,
		projectSlug:           projectSlug,
		runID:                 runID,
		expectedSeries:        expectedSeries,
		expectedMinLogs:       expectedMinLogs,
		expectedSpansPerTrace: batchSize,
		sampleTraceIDs:        sampleTraceIDs,
		traceService:          sampleTraceService,
		windowStart:           start,
		timeout:               readbackTimeout,
	})

	if err := stopServer(); err != nil {
		return result{}, fmt.Errorf("stop lazy-observe: %w", err)
	}

	overall := summarizeDurations(allLatencies)
	perSignal := map[string]latencyPercentiles{
		"metrics": summarizeDurations(signalLatencies[signalMetrics]),
		"logs":    summarizeDurations(signalLatencies[signalLogs]),
		"traces":  summarizeDurations(signalLatencies[signalTraces]),
	}

	return result{
		Version:              2,
		RunID:                runID,
		DurationSeconds:      elapsed,
		Concurrency:          concurrency,
		BatchSize:            batchSize,
		PayloadPoolSize:      poolSize,
		Compression:          compression,
		MetricSamples:        metricCount.Load(),
		LogRecords:           logCount.Load(),
		TraceSpans:           spanCount.Load(),
		SamplesPerSecond:     float64(metricCount.Load()) / elapsed,
		LogsPerSecond:        float64(logCount.Load()) / elapsed,
		SpansPerSecond:       float64(spanCount.Load()) / elapsed,
		P50LatencyMS:         overall.P50MS,
		P95LatencyMS:         overall.P95MS,
		P99LatencyMS:         overall.P99MS,
		SignalLatency:        perSignal,
		MaxRSSBytes:          maxTreeRSS,
		MaxMainRSSBytes:      maxMainRSS,
		MaxComponentRSSBytes: maxComponentRSS,
		PeakProcessRSS:       peakProcesses,
		RSSIncludesChildren:  true,
		RequestsFailed:       failures.Load(),
		Readback:             readback,
	}, nil
}

func setupBenchMetadata(metaPath string) (string, string, string, error) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, metaPath)
	if err != nil {
		return "", "", "", err
	}
	defer store.Close()

	project, err := store.CreateProject(ctx, "Benchmark", "benchmark")
	if err != nil {
		return "", "", "", fmt.Errorf("create benchmark project: %w", err)
	}
	ingestToken, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		return "", "", "", fmt.Errorf("create benchmark ingest token: %w", err)
	}
	passBytes := make([]byte, 18)
	if _, err := rand.Read(passBytes); err != nil {
		return "", "", "", fmt.Errorf("generate benchmark user password: %w", err)
	}
	reader, err := store.CreateUser(ctx, "bench-reader", hex.EncodeToString(passBytes), "admin")
	if err != nil {
		return "", "", "", fmt.Errorf("create benchmark reader user: %w", err)
	}
	_, readToken, err := store.CreateUserAPIToken(ctx, reader.ID, "ingest-bench-readback")
	if err != nil {
		return "", "", "", fmt.Errorf("create benchmark read token: %w", err)
	}
	return ingestToken, readToken, project.Slug, nil
}

func normalizeCompression(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none":
		return "none", nil
	case "gzip":
		return "gzip", nil
	default:
		return "", fmt.Errorf("invalid compression %q: must be none or gzip", raw)
	}
}

func encodeOTLPBody(
	compression string,
	raw []byte,
	gzBuf *bytes.Buffer,
	gzWriter **gzip.Writer,
) ([]byte, string, error) {
	if compression != "gzip" {
		return raw, "", nil
	}
	gzBuf.Reset()
	if *gzWriter == nil {
		*gzWriter = gzip.NewWriter(gzBuf)
	} else {
		(*gzWriter).Reset(gzBuf)
	}
	if _, err := (*gzWriter).Write(raw); err != nil {
		return nil, "", err
	}
	if err := (*gzWriter).Close(); err != nil {
		return nil, "", err
	}
	return gzBuf.Bytes(), "gzip", nil
}

func postOTLP(
	ctx context.Context,
	client *http.Client,
	target, token string,
	body []byte,
	contentEncoding string,
	sig signalKind,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	respBody, readErr := readBounded(resp.Body, otlpResponseMaxBytes)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return validateOTLPResponse(respBody, sig)
}

func validateOTLPResponse(body []byte, sig signalKind) error {
	switch sig {
	case signalMetrics:
		var response metricspb.ExportMetricsServiceResponse
		if err := proto.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("invalid OTLP metrics response: %w", err)
		}
		if partial := response.GetPartialSuccess(); partial != nil &&
			(partial.GetRejectedDataPoints() != 0 || strings.TrimSpace(partial.GetErrorMessage()) != "") {
			return fmt.Errorf("OTLP metrics partial success rejected %d data points", partial.GetRejectedDataPoints())
		}
		return nil
	case signalLogs:
		var response logspb.ExportLogsServiceResponse
		if err := proto.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("invalid OTLP logs response: %w", err)
		}
		if partial := response.GetPartialSuccess(); partial != nil &&
			(partial.GetRejectedLogRecords() != 0 || strings.TrimSpace(partial.GetErrorMessage()) != "") {
			return fmt.Errorf("OTLP logs partial success rejected %d log records", partial.GetRejectedLogRecords())
		}
		return nil
	case signalTraces:
		var response tracepb.ExportTraceServiceResponse
		if err := proto.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("invalid OTLP traces response: %w", err)
		}
		if partial := response.GetPartialSuccess(); partial != nil &&
			(partial.GetRejectedSpans() != 0 || strings.TrimSpace(partial.GetErrorMessage()) != "") {
			return fmt.Errorf("OTLP traces partial success rejected %d spans", partial.GetRejectedSpans())
		}
		return nil
	default:
		return fmt.Errorf("unknown signal %d", sig)
	}
}

func applyStandaloneGate(current *result) {
	if current.RequestsFailed > 0 {
		current.GateFailures = append(
			current.GateFailures,
			fmt.Sprintf("failed requests %d > 0", current.RequestsFailed),
		)
	}
	if !current.Readback.Verified {
		msg := current.Readback.Error
		if msg == "" {
			msg = "read-back verification did not pass"
		}
		current.GateFailures = append(current.GateFailures, "readback verification failed: "+msg)
	}
	if current.MaxRSSBytes == 0 {
		current.GateFailures = append(current.GateFailures, "max RSS bytes was not measured (0)")
	}
	current.GatePassed = len(current.GateFailures) == 0
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
	applyStandaloneGate(current)

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
	if baseline.SpansPerSecond > 0 && current.SpansPerSecond < baseline.SpansPerSecond*0.6 {
		current.GateFailures = append(
			current.GateFailures,
			fmt.Sprintf("spans/s %.0f < baseline %.0f x 0.6", current.SpansPerSecond, baseline.SpansPerSecond),
		)
	}

	maxP99MS := resolveP99Budget(baseline)
	if maxP99MS <= 0 {
		current.GateFailures = append(current.GateFailures, "baseline missing p99 latency budget")
	} else {
		current.MaxP99LatencyMS = maxP99MS
		if current.P99LatencyMS > maxP99MS {
			current.GateFailures = append(
				current.GateFailures,
				fmt.Sprintf(
					"p99 latency %.2fms > budget %.2fms (baseline %.2fms)",
					current.P99LatencyMS,
					maxP99MS,
					baseline.P99LatencyMS,
				),
			)
		}
	}
	if len(baseline.MaxSignalP99MS) > 0 {
		current.MaxSignalP99MS = make(map[string]float64, len(baseline.MaxSignalP99MS))
		signals := make([]string, 0, len(baseline.MaxSignalP99MS))
		for sig := range baseline.MaxSignalP99MS {
			signals = append(signals, sig)
		}
		sort.Strings(signals)
		for _, sig := range signals {
			limit := baseline.MaxSignalP99MS[sig]
			current.MaxSignalP99MS[sig] = limit
			if limit <= 0 {
				continue
			}
			actual := current.SignalLatency[sig].P99MS
			if actual > limit {
				current.GateFailures = append(
					current.GateFailures,
					fmt.Sprintf("%s p99 latency %.2fms > budget %.2fms", sig, actual, limit),
				)
			}
		}
	}

	maxRSSLimit := resolveRSSLimit(baseline)
	if maxRSSLimit > 0 {
		current.MaxRSSLimitBytes = maxRSSLimit
		if current.MaxRSSBytes > maxRSSLimit {
			current.GateFailures = append(
				current.GateFailures,
				fmt.Sprintf("RSS %d > limit %d (baseline %d)", current.MaxRSSBytes, maxRSSLimit, baseline.MaxRSSBytes),
			)
		}
	}
	current.GatePassed = len(current.GateFailures) == 0
	return nil
}

func resolveP99Budget(baseline result) float64 {
	if baseline.MaxP99LatencyMS > 0 {
		return baseline.MaxP99LatencyMS
	}
	if baseline.P99LatencyMS > 0 {
		return math.Max(baseline.P99LatencyMS*p99LatencyMultiplier, minP99BudgetMS)
	}
	return 0
}

func resolveRSSLimit(baseline result) uint64 {
	if baseline.MaxRSSLimitBytes > 0 {
		return baseline.MaxRSSLimitBytes
	}
	if baseline.MaxRSSBytes > 0 {
		return baseline.MaxRSSBytes * 3 / 2
	}
	return 0
}

func summarizeDurations(values []time.Duration) latencyPercentiles {
	if len(values) == 0 {
		return latencyPercentiles{}
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return latencyPercentiles{
		P50MS: durationToMS(nearestRankDuration(sorted, 0.50)),
		P95MS: durationToMS(nearestRankDuration(sorted, 0.95)),
		P99MS: durationToMS(nearestRankDuration(sorted, 0.99)),
	}
}

func nearestRankDuration(sorted []time.Duration, quantile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func durationToMS(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / float64(time.Millisecond)
}

func collectUniqueStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		if len(out) >= maxSampleTraceIDs {
			break
		}
	}
	return out
}

func currentGitCommit() string {
	if envCommit := strings.TrimSpace(os.Getenv("LO_PERF_COMMIT")); envCommit != "" {
		return envCommit
	}
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func freeAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	return addr, listener.Close()
}

func findAvailableComponentPortOffset() (int, error) {
	basePorts := []int{18428, 19428, 20428, 22428}
	for offset := 0; offset <= 10000; offset += 100 {
		if componentPortsFree(basePorts, offset) {
			return offset, nil
		}
	}
	return 0, errors.New("could not find free port offset for embedded storage components")
}

func componentPortsFree(basePorts []int, offset int) bool {
	listeners := make([]net.Listener, 0, len(basePorts))
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	for _, port := range basePorts {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+offset))
		if err != nil {
			return false
		}
		listeners = append(listeners, ln)
	}
	return true
}

func waitReady(client *http.Client, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out waiting for /readyz")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ingest-bench:", err)
	os.Exit(2)
}
