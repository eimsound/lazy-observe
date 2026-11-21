// Package selfmetrics provides a zero-dependency, lock-free, fixed-cardinality
// Prometheus text format 0.0.4 metrics registry for the lazy-observe gateway
// and OTLP/gRPC server.
package selfmetrics

import (
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ContentType is the Prometheus text exposition format 0.0.4 media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Surface is the bounded HTTP routing surface label enum.
type Surface uint8

const (
	SurfaceAdmin Surface = iota
	SurfaceIngest
	SurfaceMetrics
	SurfacePublic
	SurfaceQuery
	surfaceCount
)

var surfaceLabels = [surfaceCount]string{
	SurfaceAdmin:   "admin",
	SurfaceIngest:  "ingest",
	SurfaceMetrics: "metrics",
	SurfacePublic:  "public",
	SurfaceQuery:   "query",
}

// String returns the canonical low-cardinality label value for s.
func (s Surface) String() string {
	if s < surfaceCount {
		return surfaceLabels[s]
	}
	return "public"
}

type statusClass uint8

const (
	status2xx statusClass = iota
	status3xx
	status4xx
	status5xx
	statusClassCount
)

var statusClassLabels = [statusClassCount]string{
	status2xx: "2xx",
	status3xx: "3xx",
	status4xx: "4xx",
	status5xx: "5xx",
}

func classifyHTTPStatus(code int) statusClass {
	switch {
	case code >= 500:
		return status5xx
	case code >= 400:
		return status4xx
	case code >= 300:
		return status3xx
	default:
		return status2xx
	}
}

// Protocol is the bounded telemetry ingest transport protocol label enum.
type Protocol uint8

const (
	ProtocolGRPC Protocol = iota
	ProtocolHTTP
	protocolCount
)

var protocolLabels = [protocolCount]string{
	ProtocolGRPC: "grpc",
	ProtocolHTTP: "http",
}

// Signal is the bounded telemetry ingest signal label enum.
type Signal uint8

const (
	SignalDiagnostics Signal = iota
	SignalErrors
	SignalLogs
	SignalMetrics
	SignalProfiles
	SignalReplay
	SignalTraces
	SignalVitals
	signalCount
)

var signalLabels = [signalCount]string{
	SignalDiagnostics: "diagnostics",
	SignalErrors:      "errors",
	SignalLogs:        "logs",
	SignalMetrics:     "metrics",
	SignalProfiles:    "profiles",
	SignalReplay:      "replay",
	SignalTraces:      "traces",
	SignalVitals:      "vitals",
}

// Outcome is the bounded telemetry ingest outcome label enum.
type Outcome uint8

const (
	OutcomeAccepted Outcome = iota
	OutcomeRateLimited
	OutcomeRejected
	OutcomeUpstreamError
	outcomeCount
)

var outcomeLabels = [outcomeCount]string{
	OutcomeAccepted:      "accepted",
	OutcomeRateLimited:   "rate_limited",
	OutcomeRejected:      "rejected",
	OutcomeUpstreamError: "upstream_error",
}

var validProtocolSignals = [protocolCount][]Signal{
	ProtocolGRPC: {SignalLogs, SignalMetrics, SignalTraces},
	ProtocolHTTP: {
		SignalDiagnostics,
		SignalErrors,
		SignalLogs,
		SignalMetrics,
		SignalProfiles,
		SignalReplay,
		SignalTraces,
		SignalVitals,
	},
}

var durationBucketBounds = [...]float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

var durationBucketLabels = [...]string{
	"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10",
}

type atomicHistogram struct {
	buckets  [len(durationBucketBounds)]atomic.Uint64
	count    atomic.Uint64
	sumNanos atomic.Uint64
}

func (h *atomicHistogram) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	secs := d.Seconds()
	for i, bound := range durationBucketBounds {
		if secs <= bound {
			h.buckets[i].Add(1)
			break
		}
	}
	h.sumNanos.Add(uint64(d.Nanoseconds()))
	h.count.Add(1)
}

// Config configures a Registry.
type Config struct {
	Version   string
	Commit    string
	StartedAt time.Time
	Now       func() time.Time
}

// Registry stores all self-observability metrics using fixed-size atomic arrays
// and renders them deterministically in Prometheus text exposition format 0.0.4.
type Registry struct {
	version   string
	commit    string
	startedAt time.Time
	now       func() time.Time

	httpRequests     [surfaceCount][statusClassCount]atomic.Uint64
	httpDurations    [surfaceCount]atomicHistogram
	ingestRequests   [protocolCount][signalCount][outcomeCount]atomic.Uint64
	ingestRateLimits [protocolCount][signalCount]atomic.Uint64
	grpcInFlight     atomic.Int64
}

// New constructs a Registry with static build metadata and clock settings.
func New(cfg Config) *Registry {
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = "dev"
	}
	commit := strings.TrimSpace(cfg.Commit)
	if commit == "" {
		commit = "none"
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	startedAt := cfg.StartedAt
	if startedAt.IsZero() {
		startedAt = nowFn()
	}
	return &Registry{
		version:   version,
		commit:    commit,
		startedAt: startedAt,
		now:       nowFn,
	}
}

// ObserveHTTP records one completed HTTP request's status class and duration.
func (r *Registry) ObserveHTTP(surface Surface, statusCode int, duration time.Duration) {
	if r == nil {
		return
	}
	if surface >= surfaceCount {
		surface = SurfacePublic
	}
	class := classifyHTTPStatus(statusCode)
	r.httpRequests[surface][class].Add(1)
	r.httpDurations[surface].observe(duration)
}

// ObserveIngest records one telemetry ingest attempt outcome. When outcome is
// OutcomeRateLimited, it also increments lazy_observe_ingest_rate_limited_total.
func (r *Registry) ObserveIngest(protocol Protocol, signal Signal, outcome Outcome) {
	if r == nil || protocol >= protocolCount || signal >= signalCount || outcome >= outcomeCount {
		return
	}
	r.ingestRequests[protocol][signal][outcome].Add(1)
	if outcome == OutcomeRateLimited {
		r.ingestRateLimits[protocol][signal].Add(1)
	}
}

// IncGRPCInFlight increments the in-flight OTLP/gRPC request gauge.
func (r *Registry) IncGRPCInFlight() {
	if r == nil {
		return
	}
	r.grpcInFlight.Add(1)
}

// DecGRPCInFlight decrements the in-flight OTLP/gRPC request gauge.
func (r *Registry) DecGRPCInFlight() {
	if r == nil {
		return
	}
	r.grpcInFlight.Add(-1)
}

// WritePrometheus writes all registered metrics in deterministic, sorted
// Prometheus Text Exposition Format 0.0.4.
func (r *Registry) WritePrometheus(w io.Writer) error {
	if r == nil {
		return nil
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	var b strings.Builder
	b.Grow(8192)

	// Go runtime metrics (alphabetical).
	writeMetricHeader(&b, "go_gc_completed_cycles_total", "Total number of completed GC cycles.", "counter")
	b.WriteString("go_gc_completed_cycles_total ")
	b.WriteString(strconv.FormatUint(uint64(ms.NumGC), 10))
	b.WriteByte('\n')

	writeMetricHeader(&b, "go_goroutines", "Number of goroutines that currently exist.", "gauge")
	b.WriteString("go_goroutines ")
	b.WriteString(strconv.Itoa(runtime.NumGoroutine()))
	b.WriteByte('\n')

	writeMetricHeader(&b, "go_memstats_alloc_bytes", "Number of bytes allocated in heap and currently in use.", "gauge")
	b.WriteString("go_memstats_alloc_bytes ")
	b.WriteString(strconv.FormatUint(ms.Alloc, 10))
	b.WriteByte('\n')

	writeMetricHeader(&b, "go_memstats_heap_inuse_bytes", "Number of heap bytes that are in use.", "gauge")
	b.WriteString("go_memstats_heap_inuse_bytes ")
	b.WriteString(strconv.FormatUint(ms.HeapInuse, 10))
	b.WriteByte('\n')

	writeMetricHeader(&b, "go_memstats_sys_bytes", "Number of bytes obtained from system.", "gauge")
	b.WriteString("go_memstats_sys_bytes ")
	b.WriteString(strconv.FormatUint(ms.Sys, 10))
	b.WriteByte('\n')

	// lazy_observe_build_info
	writeMetricHeader(&b, "lazy_observe_build_info", "Build information for the lazy-observe server.", "gauge")
	fmt.Fprintf(
		&b,
		"lazy_observe_build_info{commit=%q,version=%q} 1\n",
		escapeLabelValue(r.commit),
		escapeLabelValue(r.version),
	)

	// lazy_observe_http_request_duration_seconds
	writeMetricHeader(
		&b,
		"lazy_observe_http_request_duration_seconds",
		"HTTP request duration in seconds by gateway surface.",
		"histogram",
	)
	for s := Surface(0); s < surfaceCount; s++ {
		label := surfaceLabels[s]
		h := &r.httpDurations[s]
		var cum uint64
		for i, le := range durationBucketLabels {
			cum += h.buckets[i].Load()
			b.WriteString(`lazy_observe_http_request_duration_seconds_bucket{surface="`)
			b.WriteString(label)
			b.WriteString(`",le="`)
			b.WriteString(le)
			b.WriteString(`"} `)
			b.WriteString(strconv.FormatUint(cum, 10))
			b.WriteByte('\n')
		}
		totalCount := h.count.Load()
		if totalCount < cum {
			totalCount = cum
		}
		b.WriteString(`lazy_observe_http_request_duration_seconds_bucket{surface="`)
		b.WriteString(label)
		b.WriteString(`",le="+Inf"} `)
		b.WriteString(strconv.FormatUint(totalCount, 10))
		b.WriteByte('\n')

		sumSeconds := float64(h.sumNanos.Load()) / float64(time.Second)
		b.WriteString(`lazy_observe_http_request_duration_seconds_sum{surface="`)
		b.WriteString(label)
		b.WriteString(`"} `)
		b.WriteString(strconv.FormatFloat(sumSeconds, 'g', -1, 64))
		b.WriteByte('\n')

		b.WriteString(`lazy_observe_http_request_duration_seconds_count{surface="`)
		b.WriteString(label)
		b.WriteString(`"} `)
		b.WriteString(strconv.FormatUint(totalCount, 10))
		b.WriteByte('\n')
	}

	// lazy_observe_http_requests_total
	writeMetricHeader(
		&b,
		"lazy_observe_http_requests_total",
		"Total number of HTTP requests handled by the gateway.",
		"counter",
	)
	for s := Surface(0); s < surfaceCount; s++ {
		for sc := statusClass(0); sc < statusClassCount; sc++ {
			b.WriteString(`lazy_observe_http_requests_total{status_class="`)
			b.WriteString(statusClassLabels[sc])
			b.WriteString(`",surface="`)
			b.WriteString(surfaceLabels[s])
			b.WriteString(`"} `)
			b.WriteString(strconv.FormatUint(r.httpRequests[s][sc].Load(), 10))
			b.WriteByte('\n')
		}
	}

	// lazy_observe_ingest_rate_limited_total
	writeMetricHeader(
		&b,
		"lazy_observe_ingest_rate_limited_total",
		"Total number of telemetry ingest requests rejected by rate limiting.",
		"counter",
	)
	for p := Protocol(0); p < protocolCount; p++ {
		for _, sig := range validProtocolSignals[p] {
			b.WriteString(`lazy_observe_ingest_rate_limited_total{protocol="`)
			b.WriteString(protocolLabels[p])
			b.WriteString(`",signal="`)
			b.WriteString(signalLabels[sig])
			b.WriteString(`"} `)
			b.WriteString(strconv.FormatUint(r.ingestRateLimits[p][sig].Load(), 10))
			b.WriteByte('\n')
		}
	}

	// lazy_observe_ingest_requests_total
	writeMetricHeader(
		&b,
		"lazy_observe_ingest_requests_total",
		"Total number of telemetry ingest requests by protocol, signal, and outcome.",
		"counter",
	)
	for p := Protocol(0); p < protocolCount; p++ {
		for _, sig := range validProtocolSignals[p] {
			for o := Outcome(0); o < outcomeCount; o++ {
				b.WriteString(`lazy_observe_ingest_requests_total{outcome="`)
				b.WriteString(outcomeLabels[o])
				b.WriteString(`",protocol="`)
				b.WriteString(protocolLabels[p])
				b.WriteString(`",signal="`)
				b.WriteString(signalLabels[sig])
				b.WriteString(`"} `)
				b.WriteString(strconv.FormatUint(r.ingestRequests[p][sig][o].Load(), 10))
				b.WriteByte('\n')
			}
		}
	}

	// lazy_observe_otlp_grpc_in_flight_requests
	writeMetricHeader(
		&b,
		"lazy_observe_otlp_grpc_in_flight_requests",
		"Number of OTLP/gRPC ingest requests currently in flight.",
		"gauge",
	)
	inFlight := r.grpcInFlight.Load()
	if inFlight < 0 {
		inFlight = 0
	}
	b.WriteString("lazy_observe_otlp_grpc_in_flight_requests ")
	b.WriteString(strconv.FormatInt(inFlight, 10))
	b.WriteByte('\n')

	// lazy_observe_process_uptime_seconds
	writeMetricHeader(
		&b,
		"lazy_observe_process_uptime_seconds",
		"Elapsed process uptime in seconds.",
		"gauge",
	)
	uptime := r.now().Sub(r.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	b.WriteString("lazy_observe_process_uptime_seconds ")
	b.WriteString(strconv.FormatFloat(uptime, 'g', -1, 64))
	b.WriteByte('\n')

	_, err := io.WriteString(w, b.String())
	return err
}

func writeMetricHeader(b *strings.Builder, name, help, metricType string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(metricType)
	b.WriteByte('\n')
}

// escapeLabelValue removes control characters other than those escaped by %q
// and guarantees single-line label values.
func escapeLabelValue(raw string) string {
	cleaned := strings.ReplaceAll(raw, "\r", "")
	cleaned = strings.ReplaceAll(cleaned, "\n", "")
	return cleaned
}
