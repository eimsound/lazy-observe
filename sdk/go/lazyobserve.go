// Package lazyobserve provides explicitly installed, preconfigured OpenTelemetry
// instrumentation for lazy-observe.
package lazyobserve

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	rtmetrics "runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/eimsound/lazy-observe/sdk/go"

const (
	otelTraceQueueSize = 2_048
	otelTraceBatchSize = 512
	otelLogQueueSize   = 2_048
	otelLogBatchSize   = 512
)

const (
	// CompressionNone sends uncompressed OTLP/HTTP payloads (default).
	CompressionNone = "none"
	// CompressionGzip sends gzip-compressed OTLP/HTTP payloads.
	CompressionGzip = "gzip"
)

// Config controls SDK construction. An all-zero Metrics/Logs/Traces matrix
// means all signals enabled; otherwise the three fields are an explicit matrix.
type Config struct {
	DSN         string
	Metrics     bool
	Logs        bool
	Traces      bool
	SampleRate  float64
	ServiceName string
	// Compression configures OTLP/HTTP payload compression for metrics, logs,
	// and traces ("", "none", or "gzip"). When empty, standard
	// OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION and OTEL_EXPORTER_OTLP_COMPRESSION
	// environment variables apply, defaulting to "none".
	Compression        string
	TeeToStderr        *bool
	CaptureDBStatement bool
	// CaptureLLMContent records prompt/completion content up to 4 KiB. It is
	// disabled by default; callers are responsible for redacting sensitive data.
	CaptureLLMContent bool
	// Release and Environment identify the deployed build on error events.
	Release     string
	Environment string
	// Tags are copied during initialization and attached to every error event.
	Tags           map[string]string
	ExportInterval time.Duration
	// Profiling enables continuous CPU, heap, and goroutine pprof collection. Nil defaults to enabled.
	Profiling *bool
	// ProfilingCPU enables CPU pprof sampling when Profiling is enabled. Nil defaults to enabled.
	// Set to false to collect only heap and goroutine snapshots when another profiler owns CPU sampling.
	ProfilingCPU      *bool
	ProfilingInterval time.Duration
	Delivery          DeliveryConfig
}

// DeliveryConfig bounds custom error and profile delivery. Zero values select documented defaults.
type DeliveryConfig struct {
	ErrorQueueCapacity   int
	ProfileQueueCapacity int
	MaxAttempts          int
	RetryInitial         time.Duration
	RetryMax             time.Duration
}

type dbMetricRegistration struct {
	client  *Client
	id      uint64
	reg     metric.Registration
	stopCtx func() bool
	once    sync.Once
	err     error
}

func (r *dbMetricRegistration) unregister() error {
	r.once.Do(func() {
		var stop func() bool
		if r.client != nil {
			stop = r.client.detachDBRegistration(r)
		}
		if stop != nil {
			stop()
		}
		if r.reg != nil {
			r.err = r.reg.Unregister()
		}
	})
	return r.err
}

// Client is an isolated lazy-observe SDK instance. It never replaces global
// OpenTelemetry providers or installs process-wide hooks.
type Client struct {
	cfg                 Config
	initErr             error
	meter               metric.MeterProvider
	tracer              trace.TracerProvider
	metricSDK           *sdkmetric.MeterProvider
	traceSDK            *sdktrace.TracerProvider
	logSDK              *sdklog.LoggerProvider
	logger              *slog.Logger
	propagator          propagation.TextMapPropagator
	runtimeReg          metric.Registration
	dbRegsMu            sync.Mutex
	dbRegsClosed        bool
	dbRegNextID         uint64
	dbRegs              map[uint64]*dbMetricRegistration
	shutdown            sync.Once
	shutdownErr         error
	ingest              *ingestClient
	profileStop         context.CancelFunc
	profileDone         chan struct{}
	profileDelivery     *deliveryQueue[collectedProfile]
	cpuProfiler         *cpuProfiler // nil means systemCPUProfiler; tests inject fakes
	cpuConflictOnce     sync.Once
	profileInitialDelay func(time.Duration) time.Duration // nil uses jitterInitialProfilingDelay
	profileCycleDelay   func(time.Duration) time.Duration // nil uses jitterProfilingDelay
	errorTransport      *errorTransport
	droppedTelemetry    atomic.Uint64
	droppedCounter      metric.Int64Counter
}

// SDKDiagnostics is a process-local snapshot of bounded delivery losses.
type SDKDiagnostics struct {
	DroppedTelemetry uint64
}

// Init creates the SDK. On invalid configuration it returns a safe degraded
// instance; inspect Err to report the construction failure.
func Init(cfg Config) *Client {
	c, err := newClient(context.Background(), cfg)
	if err != nil {
		return degraded(cfg, err)
	}
	return c
}

// Err reports why Init returned a degraded instance.
func (c *Client) Err() error { return c.initErr }

func signalMatrix(cfg Config) (metrics, logs, traces bool) {
	if !cfg.Metrics && !cfg.Logs && !cfg.Traces {
		return true, true, true
	}
	return cfg.Metrics, cfg.Logs, cfg.Traces
}

func defaultServiceName() string {
	if len(os.Args) > 0 {
		if name := filepath.Base(os.Args[0]); name != "" && name != "." {
			return name
		}
	}
	return "unknown_service"
}

func normalizedConfig(cfg Config) (Config, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = defaultServiceName()
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 1
	}
	if cfg.SampleRate < 0 || cfg.SampleRate > 1 {
		return cfg, fmt.Errorf("sample rate must be between 0 and 1")
	}
	compression, err := normalizeCompression(cfg.Compression)
	if err != nil {
		return cfg, err
	}
	cfg.Compression = compression
	if cfg.ExportInterval <= 0 {
		cfg.ExportInterval = 5 * time.Second
	}
	if cfg.Profiling == nil {
		enabled := true
		cfg.Profiling = &enabled
	}
	if cfg.ProfilingCPU == nil {
		enabled := true
		cfg.ProfilingCPU = &enabled
	}
	if cfg.ProfilingInterval <= 0 {
		cfg.ProfilingInterval = 60 * time.Second
	}
	if err := normalizeDeliveryConfig(&cfg.Delivery); err != nil {
		return cfg, err
	}
	cfg.Tags = maps.Clone(cfg.Tags)
	return cfg, nil
}

func normalizeCompression(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case CompressionNone:
		return CompressionNone, nil
	case CompressionGzip:
		return CompressionGzip, nil
	default:
		return "", fmt.Errorf(
			"invalid OTLP compression %q: must be \"\", %q, or %q",
			raw,
			CompressionNone,
			CompressionGzip,
		)
	}
}

func resolveSignalCompression(explicit, signal string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	signalVar := "OTEL_EXPORTER_OTLP_" + signal + "_COMPRESSION"
	if raw := strings.TrimSpace(os.Getenv(signalVar)); raw != "" {
		normalized, err := normalizeCompression(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", signalVar, err)
		}
		return normalized, nil
	}
	if raw := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_COMPRESSION")); raw != "" {
		normalized, err := normalizeCompression(raw)
		if err != nil {
			return "", fmt.Errorf("OTEL_EXPORTER_OTLP_COMPRESSION: %w", err)
		}
		return normalized, nil
	}
	return CompressionNone, nil
}

func normalizeDeliveryConfig(cfg *DeliveryConfig) error {
	if cfg.ErrorQueueCapacity < 0 || cfg.ProfileQueueCapacity < 0 || cfg.MaxAttempts < 0 ||
		cfg.RetryInitial < 0 || cfg.RetryMax < 0 {
		return errors.New("delivery capacities, attempts, and retry durations cannot be negative")
	}
	if cfg.ErrorQueueCapacity == 0 {
		cfg.ErrorQueueCapacity = errorQueueSize
	}
	if cfg.ProfileQueueCapacity == 0 {
		cfg.ProfileQueueCapacity = profilingQueueSize
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = deliveryDefaultMaxAttempts
	}
	if cfg.RetryInitial == 0 {
		cfg.RetryInitial = deliveryDefaultRetryInitial
	}
	if cfg.RetryMax == 0 {
		cfg.RetryMax = deliveryDefaultRetryMax
	}
	if cfg.RetryMax < cfg.RetryInitial {
		return errors.New("delivery retry max cannot be shorter than retry initial")
	}
	return nil
}

func buildResource(base *resource.Resource, serviceName, project string) (*resource.Resource, error) {
	return resource.Merge(base, resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		attribute.String("lazy_observe.project", project),
	))
}

func newClient(ctx context.Context, cfg Config) (*Client, error) {
	parsed, err := parseDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	cfg, err = normalizedConfig(cfg)
	if err != nil {
		return nil, err
	}
	metricsOn, logsOn, tracesOn := signalMatrix(cfg)
	var metricsCompression, tracesCompression, logsCompression string
	if metricsOn {
		metricsCompression, err = resolveSignalCompression(cfg.Compression, "METRICS")
		if err != nil {
			return nil, err
		}
	}
	if tracesOn {
		tracesCompression, err = resolveSignalCompression(cfg.Compression, "TRACES")
		if err != nil {
			return nil, err
		}
	}
	if logsOn {
		logsCompression, err = resolveSignalCompression(cfg.Compression, "LOGS")
		if err != nil {
			return nil, err
		}
	}

	res, err := buildResource(resource.Default(), cfg.ServiceName, parsed.project)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	c := &Client{
		cfg:        cfg,
		propagator: propagation.TraceContext{},
		ingest:     newIngestClient(parsed.endpoint, parsed.token, nil),
	}
	headers := map[string]string{"Authorization": "Bearer " + parsed.token}

	if metricsOn {
		metricCompressionOpt := otlpmetrichttp.WithCompression(otlpmetrichttp.NoCompression)
		if metricsCompression == CompressionGzip {
			metricCompressionOpt = otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression)
		}
		exporter, exportErr := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(parsed.endpoint),
			otlpmetrichttp.WithURLPath("/v1/metrics"),
			otlpmetrichttp.WithHeaders(headers),
			metricCompressionOpt,
		)
		if exportErr != nil {
			return nil, fmt.Errorf("create metrics exporter: %w", exportErr)
		}
		reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.ExportInterval))
		c.metricSDK = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
		c.meter = c.metricSDK
		if err := c.registerRuntimeMetrics(); err != nil {
			_ = c.metricSDK.Shutdown(ctx)
			return nil, fmt.Errorf("register runtime metrics: %w", err)
		}
	} else {
		c.meter = metricnoop.NewMeterProvider()
	}

	if tracesOn {
		traceCompressionOpt := otlptracehttp.WithCompression(otlptracehttp.NoCompression)
		if tracesCompression == CompressionGzip {
			traceCompressionOpt = otlptracehttp.WithCompression(otlptracehttp.GzipCompression)
		}
		exporter, exportErr := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(parsed.endpoint),
			otlptracehttp.WithURLPath("/v1/traces"),
			otlptracehttp.WithHeaders(headers),
			traceCompressionOpt,
		)
		if exportErr != nil {
			_ = c.closeCreated(ctx)
			return nil, fmt.Errorf("create traces exporter: %w", exportErr)
		}
		c.traceSDK = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))),
			sdktrace.WithBatcher(
				exporter,
				sdktrace.WithMaxQueueSize(otelTraceQueueSize),
				sdktrace.WithMaxExportBatchSize(otelTraceBatchSize),
			),
		)
		c.tracer = c.traceSDK
	} else {
		c.tracer = tracenoop.NewTracerProvider()
	}

	var otelHandler slog.Handler
	if logsOn {
		logCompressionOpt := otlploghttp.WithCompression(otlploghttp.NoCompression)
		if logsCompression == CompressionGzip {
			logCompressionOpt = otlploghttp.WithCompression(otlploghttp.GzipCompression)
		}
		exporter, exportErr := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(parsed.endpoint),
			otlploghttp.WithURLPath("/v1/logs"),
			otlploghttp.WithHeaders(headers),
			logCompressionOpt,
		)
		if exportErr != nil {
			_ = c.closeCreated(ctx)
			return nil, fmt.Errorf("create logs exporter: %w", exportErr)
		}
		processor := sdklog.NewBatchProcessor(
			exporter,
			sdklog.WithExportInterval(cfg.ExportInterval),
			sdklog.WithMaxQueueSize(otelLogQueueSize),
			sdklog.WithExportMaxBatchSize(otelLogBatchSize),
		)
		c.logSDK = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(processor))
		otelHandler = otelslog.NewHandler(instrumentationName, otelslog.WithLoggerProvider(c.logSDK))
	}
	tee := cfg.TeeToStderr == nil || *cfg.TeeToStderr
	var stderr slog.Handler
	if tee {
		stderr = slog.NewTextHandler(os.Stderr, nil)
	}
	c.logger = slog.New(&correlatingHandler{service: cfg.ServiceName, otel: otelHandler, tee: stderr})
	droppedCounter, counterErr := c.meter.Meter(instrumentationName+"/delivery").Int64Counter(
		"lazy_observe_sdk_dropped_telemetry",
		metric.WithDescription("Telemetry items dropped by bounded SDK delivery."),
		metric.WithUnit("{item}"),
	)
	if counterErr != nil {
		_ = c.closeCreated(ctx)
		return nil, fmt.Errorf("register delivery metrics: %w", counterErr)
	}
	c.droppedCounter = droppedCounter
	c.errorTransport = newErrorTransport(c.ingest, c.logger, cfg.Delivery, c.recordTelemetryDrop)
	if *cfg.Profiling {
		c.profileDelivery = newProfileDelivery(c)
		profileCtx, cancel := context.WithCancel(context.Background())
		c.profileStop = cancel
		c.profileDone = make(chan struct{})
		go c.profilingLoop(profileCtx)
	}
	return c, nil
}

func degraded(cfg Config, err error) *Client {
	if cfg.ServiceName == "" {
		cfg.ServiceName = defaultServiceName()
	}
	var stderr slog.Handler
	if cfg.TeeToStderr == nil || *cfg.TeeToStderr {
		stderr = slog.NewTextHandler(os.Stderr, nil)
	}
	h := &correlatingHandler{service: cfg.ServiceName, tee: stderr}
	return &Client{
		cfg:        cfg,
		initErr:    err,
		meter:      metricnoop.NewMeterProvider(),
		tracer:     tracenoop.NewTracerProvider(),
		logger:     slog.New(h),
		propagator: propagation.TraceContext{},
	}
}

// Slog returns a logger that correlates records with the span in ctx.
func (c *Client) Slog() *slog.Logger { return c.logger }

// TracerProvider exposes this instance's provider without mutating OTel globals.
func (c *Client) TracerProvider() trace.TracerProvider { return c.tracer }

// MeterProvider exposes this instance's provider without mutating OTel globals.
func (c *Client) MeterProvider() metric.MeterProvider { return c.meter }

// Diagnostics returns process-local bounded delivery counters without network access.
func (c *Client) Diagnostics() SDKDiagnostics {
	return SDKDiagnostics{DroppedTelemetry: c.droppedTelemetry.Load()}
}

// WrapDB explicitly opens an instrumented database/sql DB and registers pool
// metrics that are unregistered when Shutdown runs. For short-lived or
// dynamically rotated DB pools, prefer WrapDBContext so pool metric
// registration is released as soon as the pool's context is cancelled.
func (c *Client) WrapDB(driverName, dataSourceName string) (*sql.DB, error) {
	return c.WrapDBContext(context.Background(), driverName, dataSourceName)
}

// WrapDBContext explicitly opens an instrumented database/sql DB and binds its
// pool metric registration lifecycle to ctx as well as Client.Shutdown. When
// ctx is cancelled or Shutdown runs, the metric.Registration is unregistered
// immediately so closed databases are not retained or scraped indefinitely.
func (c *Client) WrapDBContext(ctx context.Context, driverName, dataSourceName string) (*sql.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open instrumented database: %w", err)
	}
	opts := []otelsql.Option{
		otelsql.WithTracerProvider(c.tracer),
		otelsql.WithMeterProvider(c.meter),
		otelsql.WithAttributes(
			attribute.String("db.system.name", driverName),
			attribute.String("db.system", driverName),
		),
		otelsql.WithSpanOptions(otelsql.SpanOptions{DisableQuery: !c.cfg.CaptureDBStatement}),
	}
	if c.cfg.CaptureDBStatement {
		opts = append(
			opts,
			otelsql.WithAttributesGetter(
				func(_ context.Context, _ otelsql.Method, query string, _ []driver.NamedValue) []attribute.KeyValue {
					return []attribute.KeyValue{attribute.String("db.statement", query)}
				},
			),
		)
	}
	db, err := otelsql.Open(driverName, dataSourceName, opts...)
	if err != nil {
		return nil, fmt.Errorf("open instrumented database: %w", err)
	}
	c.dbRegsMu.Lock()
	closed := c.dbRegsClosed
	c.dbRegsMu.Unlock()
	if closed {
		return db, nil
	}
	reg, err := otelsql.RegisterDBStatsMetrics(db, opts...)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("register database metrics: %w", err)
	}
	entry := &dbMetricRegistration{client: c, reg: reg}
	c.dbRegsMu.Lock()
	if c.dbRegsClosed {
		c.dbRegsMu.Unlock()
		_ = reg.Unregister()
		return db, nil
	}
	if c.dbRegs == nil {
		c.dbRegs = make(map[uint64]*dbMetricRegistration)
	}
	c.dbRegNextID++
	entry.id = c.dbRegNextID
	c.dbRegs[entry.id] = entry
	if ctx.Done() != nil {
		entry.stopCtx = context.AfterFunc(ctx, func() {
			_ = entry.unregister()
		})
	}
	c.dbRegsMu.Unlock()
	return db, nil
}

func (c *Client) detachDBRegistration(entry *dbMetricRegistration) func() bool {
	c.dbRegsMu.Lock()
	defer c.dbRegsMu.Unlock()
	delete(c.dbRegs, entry.id)
	stop := entry.stopCtx
	entry.stopCtx = nil
	return stop
}

func (c *Client) activeDBRegistrations() int {
	c.dbRegsMu.Lock()
	defer c.dbRegsMu.Unlock()
	return len(c.dbRegs)
}

// Shutdown flushes all enabled signals and releases SDK resources.
// When ctx has no deadline, Shutdown bounds cleanup with a default timeout.
func (c *Client) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultDeliveryShutdownTimeout)
		defer cancel()
	}
	c.shutdown.Do(func() {
		c.shutdownErr = c.closeCreated(ctx)
	})
	return c.shutdownErr
}

func (c *Client) closeCreated(ctx context.Context) error {
	var errs []error
	// Signal profilingLoop to stop immediately so CPU profiling never outlives
	// shutdown if errorTransport drains consume the shutdown budget.
	if c.profileStop != nil {
		c.profileStop()
	}
	if c.errorTransport != nil {
		// Error events are short-lived and user-triggered. Drain them before a
		// profiling upload or an OTel exporter can consume the shutdown budget.
		errs = append(errs, c.errorTransport.close(ctx))
	}
	if c.profileDone != nil {
		select {
		case <-c.profileDone:
		case <-ctx.Done():
		}
	}
	if c.profileDelivery != nil {
		errs = append(errs, c.profileDelivery.close(ctx))
	}
	c.dbRegsMu.Lock()
	c.dbRegsClosed = true
	regs := make([]*dbMetricRegistration, 0, len(c.dbRegs))
	for _, entry := range c.dbRegs {
		regs = append(regs, entry)
	}
	c.dbRegs = nil
	c.dbRegsMu.Unlock()
	for _, entry := range regs {
		errs = append(errs, entry.unregister())
	}
	if c.runtimeReg != nil {
		errs = append(errs, c.runtimeReg.Unregister())
		c.runtimeReg = nil
	}
	if c.logSDK != nil {
		errs = append(errs, c.logSDK.Shutdown(ctx))
	}
	if c.traceSDK != nil {
		errs = append(errs, c.traceSDK.Shutdown(ctx))
	}
	if c.metricSDK != nil {
		errs = append(errs, c.metricSDK.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func (c *Client) recordTelemetryDrop(signal, reason string, count int) {
	if count <= 0 {
		return
	}
	c.droppedTelemetry.Add(uint64(count))
	if c.droppedCounter != nil {
		c.droppedCounter.Add(
			context.Background(),
			int64(count),
			metric.WithAttributes(
				attribute.String("signal", signal),
				attribute.String("reason", reason),
			),
		)
	}
}

func (c *Client) registerRuntimeMetrics() error {
	m := c.meter.Meter(instrumentationName + "/runtime")
	goroutines, err := m.Int64ObservableGauge(
		"process.runtime.go.goroutines",
		metric.WithDescription("Number of goroutines that currently exist."),
		metric.WithUnit("{goroutine}"),
	)
	if err != nil {
		return err
	}
	heap, err := m.Int64ObservableGauge(
		"process.runtime.go.memory.heap_alloc",
		metric.WithDescription("Bytes of allocated heap objects."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}
	gc, err := m.Int64ObservableCounter(
		"process.runtime.go.gc.count",
		metric.WithDescription("Completed garbage collection cycles."),
		metric.WithUnit("{cycle}"),
	)
	if err != nil {
		return err
	}
	reg, err := m.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		samples := [3]rtmetrics.Sample{
			{Name: "/sched/goroutines:goroutines"},
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/gc/cycles/total:gc-cycles"},
		}
		rtmetrics.Read(samples[:])
		if samples[0].Value.Kind() == rtmetrics.KindUint64 {
			observer.ObserveInt64(goroutines, int64(samples[0].Value.Uint64()))
		} else {
			observer.ObserveInt64(goroutines, int64(runtime.NumGoroutine()))
		}
		if samples[1].Value.Kind() == rtmetrics.KindUint64 {
			observer.ObserveInt64(heap, int64(samples[1].Value.Uint64()))
		}
		if samples[2].Value.Kind() == rtmetrics.KindUint64 {
			observer.ObserveInt64(gc, int64(samples[2].Value.Uint64()))
		}
		return nil
	}, goroutines, heap, gc)
	if err == nil {
		c.runtimeReg = reg
	}
	return err
}
