package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracedata "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type signalKind int

const (
	signalMetrics signalKind = iota
	signalLogs
	signalTraces
)

func (s signalKind) String() string {
	switch s {
	case signalMetrics:
		return "metrics"
	case signalLogs:
		return "logs"
	case signalTraces:
		return "traces"
	default:
		return "unknown"
	}
}

func (s signalKind) path() string {
	switch s {
	case signalMetrics:
		return "/v1/metrics"
	case signalLogs:
		return "/v1/logs"
	case signalTraces:
		return "/v1/traces"
	default:
		return ""
	}
}

const (
	benchMetricName        = "lazy_observe_bench_samples"
	benchRunIDAttribute    = "bench_run_id"
	defaultPayloadPoolSize = 64
)

var (
	benchServices = []string{
		"bench-checkout-api",
		"bench-orders-api",
		"bench-billing-api",
		"bench-search-api",
	}
	benchRegions = []string{"us-east", "eu-west"}
	benchRoutes  = []string{
		"/api/v1/checkout",
		"/api/v1/orders",
		"/api/v1/payments",
		"/api/v1/search",
		"/api/v1/inventory",
		"/api/v1/sessions",
	}
	benchMethods      = []string{"GET", "POST", "PUT"}
	benchStatuses     = []string{"200", "201", "202", "400", "404", "503"}
	benchSeverities   = []string{"INFO", "INFO", "INFO", "WARN", "ERROR"}
	benchLogTemplates = []string{
		"checkout workflow completed",
		"order state transition persisted",
		"payment webhook signature validated",
		"search index shard responded",
		"inventory lock refreshed",
		"session cache fallback executed",
	}
	benchOperations = []string{
		"POST /api/v1/checkout",
		"GET /api/v1/orders",
		"POST /api/v1/payments",
		"GET /api/v1/search",
		"POST /api/v1/inventory/reserve",
		"GET /api/v1/sessions/validate",
	}
)

type metricTemplate struct {
	raw          []byte
	timeOffsets  []int
	valueOffsets []int
}

type logTemplate struct {
	raw            []byte
	timeOffsets    []int
	traceIDOffsets []int
	spanIDOffsets  []int
}

type traceTemplate struct {
	raw             []byte
	service         string
	traceIDOffsets  []int
	spanIDOffsets   []int
	parentIDOffsets []int
	startOffsets    []int
	endOffsets      []int
}

type payloadPool struct {
	runID     string
	runSalt   uint64
	batchSize int
	metrics   []metricTemplate
	logs      []logTemplate
	traces    []traceTemplate
}

func newRunID() (string, uint64, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, fmt.Errorf("generate benchmark run ID: %w", err)
	}
	return "bench_" + hex.EncodeToString(raw[:8]), binary.LittleEndian.Uint64(raw[8:]), nil
}

func newPayloadPool(runID string, runSalt uint64, batchSize, poolSize int) (*payloadPool, error) {
	if runID == "" {
		return nil, errors.New("runID must not be empty")
	}
	if batchSize <= 0 {
		return nil, errors.New("batchSize must be positive")
	}
	if poolSize <= 0 {
		return nil, errors.New("poolSize must be positive")
	}
	pool := &payloadPool{
		runID:     runID,
		runSalt:   runSalt,
		batchSize: batchSize,
		metrics:   make([]metricTemplate, poolSize),
		logs:      make([]logTemplate, poolSize),
		traces:    make([]traceTemplate, poolSize),
	}
	for slot := 0; slot < poolSize; slot++ {
		mt, err := buildMetricTemplate(runID, batchSize, slot)
		if err != nil {
			return nil, fmt.Errorf("build metric template %d: %w", slot, err)
		}
		lt, err := buildLogTemplate(runID, batchSize, slot)
		if err != nil {
			return nil, fmt.Errorf("build log template %d: %w", slot, err)
		}
		tt, err := buildTraceTemplate(runID, batchSize, slot)
		if err != nil {
			return nil, fmt.Errorf("build trace template %d: %w", slot, err)
		}
		pool.metrics[slot] = mt
		pool.logs[slot] = lt
		pool.traces[slot] = tt
	}
	return pool, nil
}

func buildMetricTemplate(runID string, batchSize, slot int) (metricTemplate, error) {
	// Keep metric label cardinality strictly bounded:
	// 4 (service, region) pairs x batchSize series = 4 * batchSize total active series.
	svcIndex := slot % len(benchServices)
	service := benchServices[svcIndex]
	region := benchRegions[svcIndex%len(benchRegions)]

	timeSentinels := make([][8]byte, batchSize)
	valueSentinels := make([][8]byte, batchSize)
	points := make([]*metricsdata.NumberDataPoint, batchSize)
	for i := 0; i < batchSize; i++ {
		timeVal := uint64(0x5a71000000000000) | uint64(slot<<24) | uint64(i+1)
		binary.LittleEndian.PutUint64(timeSentinels[i][:], timeVal)

		// Normalized IEEE-754 float64 bits (exponent 0x430) so proto.Marshal writes them verbatim.
		valBits := uint64(0x4300000000000000) | uint64(slot<<24) | uint64(i+1)
		binary.LittleEndian.PutUint64(valueSentinels[i][:], valBits)

		points[i] = &metricsdata.NumberDataPoint{
			TimeUnixNano: timeVal,
			Attributes: []*commonpb.KeyValue{
				stringAttr("series", strconv.Itoa(i)),
				stringAttr("region", region),
				stringAttr(benchRunIDAttribute, runID),
			},
			Value: &metricsdata.NumberDataPoint_AsDouble{AsDouble: math.Float64frombits(valBits)},
		}
	}
	req := &metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsdata.ResourceMetrics{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					stringAttr("service.name", service),
					stringAttr("deployment.environment", "bench"),
					stringAttr(benchRunIDAttribute, runID),
				},
			},
			ScopeMetrics: []*metricsdata.ScopeMetrics{{
				Metrics: []*metricsdata.Metric{{
					Name: benchMetricName,
					Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{DataPoints: points}},
				}},
			}},
		}},
	}
	raw, err := proto.Marshal(req)
	if err != nil {
		return metricTemplate{}, err
	}
	timeOffsets := make([]int, batchSize)
	valueOffsets := make([]int, batchSize)
	for i := 0; i < batchSize; i++ {
		timeOffsets[i], err = findUniqueOffset(raw, timeSentinels[i][:])
		if err != nil {
			return metricTemplate{}, fmt.Errorf("locate metric time offset %d: %w", i, err)
		}
		valueOffsets[i], err = findUniqueOffset(raw, valueSentinels[i][:])
		if err != nil {
			return metricTemplate{}, fmt.Errorf("locate metric value offset %d: %w", i, err)
		}
	}
	return metricTemplate{raw: raw, timeOffsets: timeOffsets, valueOffsets: valueOffsets}, nil
}

func buildLogTemplate(runID string, batchSize, slot int) (logTemplate, error) {
	service := benchServices[slot%len(benchServices)]
	region := benchRegions[(slot/len(benchServices))%len(benchRegions)]

	timeSentinels := make([][8]byte, batchSize)
	traceSentinels := make([][16]byte, batchSize)
	spanSentinels := make([][8]byte, batchSize)
	records := make([]*logdata.LogRecord, batchSize)
	for i := 0; i < batchSize; i++ {
		timeVal := uint64(0x5b72000000000000) | uint64(slot<<24) | uint64(i+1)
		binary.LittleEndian.PutUint64(timeSentinels[i][:], timeVal)
		traceSentinels[i] = uniqueSentinel16(0x6c, slot, i)
		spanSentinels[i] = uniqueSentinel8(0x7d, slot, i)

		route := benchRoutes[(slot+i)%len(benchRoutes)]
		method := benchMethods[(slot+i)%len(benchMethods)]
		status := benchStatuses[(slot*3+i)%len(benchStatuses)]
		severity := benchSeverities[(slot+i)%len(benchSeverities)]
		msgTemplate := benchLogTemplates[(slot+i)%len(benchLogTemplates)]
		body := fmt.Sprintf(
			"%s run_id=%s slot=%d seq=%d method=%s route=%s status=%s",
			msgTemplate,
			runID,
			slot,
			i,
			method,
			route,
			status,
		)
		records[i] = &logdata.LogRecord{
			TimeUnixNano: timeVal,
			SeverityText: severity,
			Body:         stringValue(body),
			TraceId:      append([]byte(nil), traceSentinels[i][:]...),
			SpanId:       append([]byte(nil), spanSentinels[i][:]...),
			Attributes: []*commonpb.KeyValue{
				stringAttr(benchRunIDAttribute, runID),
				stringAttr("sequence", strconv.Itoa(i)),
				stringAttr("payload_slot", strconv.Itoa(slot)),
				stringAttr("region", region),
				stringAttr("http.method", method),
				stringAttr("http.route", route),
				stringAttr("http.status_code", status),
			},
		}
	}
	req := &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*logdata.ResourceLogs{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					stringAttr("service.name", service),
					stringAttr("deployment.environment", "bench"),
					stringAttr(benchRunIDAttribute, runID),
				},
			},
			ScopeLogs: []*logdata.ScopeLogs{{LogRecords: records}},
		}},
	}
	raw, err := proto.Marshal(req)
	if err != nil {
		return logTemplate{}, err
	}
	timeOffsets := make([]int, batchSize)
	traceIDOffsets := make([]int, batchSize)
	spanIDOffsets := make([]int, batchSize)
	for i := 0; i < batchSize; i++ {
		timeOffsets[i], err = findUniqueOffset(raw, timeSentinels[i][:])
		if err != nil {
			return logTemplate{}, fmt.Errorf("locate log time offset %d: %w", i, err)
		}
		traceIDOffsets[i], err = findUniqueOffset(raw, traceSentinels[i][:])
		if err != nil {
			return logTemplate{}, fmt.Errorf("locate log trace_id offset %d: %w", i, err)
		}
		spanIDOffsets[i], err = findUniqueOffset(raw, spanSentinels[i][:])
		if err != nil {
			return logTemplate{}, fmt.Errorf("locate log span_id offset %d: %w", i, err)
		}
	}
	return logTemplate{
		raw:            raw,
		timeOffsets:    timeOffsets,
		traceIDOffsets: traceIDOffsets,
		spanIDOffsets:  spanIDOffsets,
	}, nil
}

func buildTraceTemplate(runID string, batchSize, slot int) (traceTemplate, error) {
	service := benchServices[slot%len(benchServices)]
	region := benchRegions[(slot/len(benchServices))%len(benchRegions)]

	traceSentinels := make([][16]byte, batchSize)
	spanSentinels := make([][8]byte, batchSize)
	parentSentinels := make([][8]byte, max(batchSize-1, 0))
	startSentinels := make([][8]byte, batchSize)
	endSentinels := make([][8]byte, batchSize)
	spans := make([]*tracedata.Span, batchSize)
	for i := 0; i < batchSize; i++ {
		traceSentinels[i] = uniqueSentinel16(0x8e, slot, i)
		spanSentinels[i] = uniqueSentinel8(0x9f, slot, i)
		startVal := uint64(0x5c73000000000000) | uint64(slot<<24) | uint64(i+1)
		endVal := uint64(0x5d74000000000000) | uint64(slot<<24) | uint64(i+1)
		binary.LittleEndian.PutUint64(startSentinels[i][:], startVal)
		binary.LittleEndian.PutUint64(endSentinels[i][:], endVal)

		var parentID []byte
		kind := tracedata.Span_SPAN_KIND_SERVER
		if i > 0 {
			parentSentinels[i-1] = uniqueSentinel8(0xae, slot, i)
			parentID = append([]byte(nil), parentSentinels[i-1][:]...)
			kind = tracedata.Span_SPAN_KIND_INTERNAL
		}
		op := benchOperations[(slot+i)%len(benchOperations)]
		route := benchRoutes[(slot+i)%len(benchRoutes)]
		method := benchMethods[(slot+i)%len(benchMethods)]
		status := benchStatuses[(slot*5+i)%len(benchStatuses)]
		spans[i] = &tracedata.Span{
			TraceId:           append([]byte(nil), traceSentinels[i][:]...),
			SpanId:            append([]byte(nil), spanSentinels[i][:]...),
			ParentSpanId:      parentID,
			Name:              op,
			Kind:              kind,
			StartTimeUnixNano: startVal,
			EndTimeUnixNano:   endVal,
			Attributes: []*commonpb.KeyValue{
				stringAttr(benchRunIDAttribute, runID),
				stringAttr("span.index", strconv.Itoa(i)),
				stringAttr("payload_slot", strconv.Itoa(slot)),
				stringAttr("region", region),
				stringAttr("http.method", method),
				stringAttr("http.route", route),
				stringAttr("http.status_code", status),
			},
		}
	}
	req := &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracedata.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					stringAttr("service.name", service),
					stringAttr("deployment.environment", "bench"),
					stringAttr(benchRunIDAttribute, runID),
				},
			},
			ScopeSpans: []*tracedata.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "lazy-observe-ingest-bench"},
				Spans: spans,
			}},
		}},
	}
	raw, err := proto.Marshal(req)
	if err != nil {
		return traceTemplate{}, err
	}
	traceIDOffsets := make([]int, batchSize)
	spanIDOffsets := make([]int, batchSize)
	parentIDOffsets := make([]int, max(batchSize-1, 0))
	startOffsets := make([]int, batchSize)
	endOffsets := make([]int, batchSize)
	for i := 0; i < batchSize; i++ {
		traceIDOffsets[i], err = findUniqueOffset(raw, traceSentinels[i][:])
		if err != nil {
			return traceTemplate{}, fmt.Errorf("locate trace trace_id offset %d: %w", i, err)
		}
		spanIDOffsets[i], err = findUniqueOffset(raw, spanSentinels[i][:])
		if err != nil {
			return traceTemplate{}, fmt.Errorf("locate trace span_id offset %d: %w", i, err)
		}
		if i > 0 {
			parentIDOffsets[i-1], err = findUniqueOffset(raw, parentSentinels[i-1][:])
			if err != nil {
				return traceTemplate{}, fmt.Errorf("locate trace parent_span_id offset %d: %w", i, err)
			}
		}
		startOffsets[i], err = findUniqueOffset(raw, startSentinels[i][:])
		if err != nil {
			return traceTemplate{}, fmt.Errorf("locate trace start_time offset %d: %w", i, err)
		}
		endOffsets[i], err = findUniqueOffset(raw, endSentinels[i][:])
		if err != nil {
			return traceTemplate{}, fmt.Errorf("locate trace end_time offset %d: %w", i, err)
		}
	}
	return traceTemplate{
		raw:             raw,
		service:         service,
		traceIDOffsets:  traceIDOffsets,
		spanIDOffsets:   spanIDOffsets,
		parentIDOffsets: parentIDOffsets,
		startOffsets:    startOffsets,
		endOffsets:      endOffsets,
	}, nil
}

func (p *payloadPool) materializeMetric(dst []byte, workerID int, seq uint64, nowNano uint64) []byte {
	slot := int((seq + uint64(workerID)*7) % uint64(len(p.metrics)))
	tpl := &p.metrics[slot]
	buf := resizeSlice(dst, len(tpl.raw))
	copy(buf, tpl.raw)
	for i, off := range tpl.timeOffsets {
		binary.LittleEndian.PutUint64(buf[off:off+8], nowNano+uint64(i)*1000)
	}
	for i, off := range tpl.valueOffsets {
		val := float64((seq+uint64(i)+uint64(workerID)*13)%10000) + 0.25
		binary.LittleEndian.PutUint64(buf[off:off+8], math.Float64bits(val))
	}
	return buf
}

func (p *payloadPool) materializeLog(dst []byte, workerID int, seq uint64, nowNano uint64) []byte {
	slot := int((seq + uint64(workerID)*11) % uint64(len(p.logs)))
	tpl := &p.logs[slot]
	buf := resizeSlice(dst, len(tpl.raw))
	copy(buf, tpl.raw)
	for i, off := range tpl.timeOffsets {
		binary.LittleEndian.PutUint64(buf[off:off+8], nowNano+uint64(i)*1000)
	}
	for i := range tpl.traceIDOffsets {
		traceID := deriveTraceID(p.runSalt, 1, workerID, seq, i)
		spanID := deriveSpanID(p.runSalt, 1, workerID, seq, i)
		copy(buf[tpl.traceIDOffsets[i]:tpl.traceIDOffsets[i]+16], traceID[:])
		copy(buf[tpl.spanIDOffsets[i]:tpl.spanIDOffsets[i]+8], spanID[:])
	}
	return buf
}

func (p *payloadPool) materializeTrace(dst []byte, workerID int, seq uint64, nowNano uint64) ([]byte, string, string) {
	slot := int((seq + uint64(workerID)*13) % uint64(len(p.traces)))
	tpl := &p.traces[slot]
	buf := resizeSlice(dst, len(tpl.raw))
	copy(buf, tpl.raw)
	traceID := deriveTraceID(p.runSalt, 2, workerID, seq, 0)
	rootSpanID := deriveSpanID(p.runSalt, 2, workerID, seq, 0)
	for i := range tpl.traceIDOffsets {
		copy(buf[tpl.traceIDOffsets[i]:tpl.traceIDOffsets[i]+16], traceID[:])
		var spanID [8]byte
		if i == 0 {
			spanID = rootSpanID
		} else {
			spanID = deriveSpanID(p.runSalt, 2, workerID, seq, i)
			copy(buf[tpl.parentIDOffsets[i-1]:tpl.parentIDOffsets[i-1]+8], rootSpanID[:])
		}
		copy(buf[tpl.spanIDOffsets[i]:tpl.spanIDOffsets[i]+8], spanID[:])
		start := nowNano + uint64(i)*10_000
		end := start + 250_000 + uint64((seq+uint64(i))%500)*1000
		binary.LittleEndian.PutUint64(buf[tpl.startOffsets[i]:tpl.startOffsets[i]+8], start)
		binary.LittleEndian.PutUint64(buf[tpl.endOffsets[i]:tpl.endOffsets[i]+8], end)
	}
	return buf, hex.EncodeToString(traceID[:]), tpl.service
}

func resizeSlice(buf []byte, n int) []byte {
	if cap(buf) < n {
		return make([]byte, n)
	}
	return buf[:n]
}

func findUniqueOffset(haystack, needle []byte) (int, error) {
	first := bytes.Index(haystack, needle)
	if first < 0 {
		return -1, errors.New("sentinel not found in serialized protobuf")
	}
	if second := bytes.Index(haystack[first+1:], needle); second >= 0 {
		return -1, errors.New("sentinel appeared more than once in serialized protobuf")
	}
	return first, nil
}

func uniqueSentinel16(tag byte, slot, index int) [16]byte {
	var out [16]byte
	out[0] = tag
	out[1] = 0xa5
	out[2] = 0x5a
	out[3] = 0xc3
	binary.LittleEndian.PutUint32(out[4:8], uint32(slot+1))
	binary.LittleEndian.PutUint32(out[8:12], uint32(index+1))
	binary.LittleEndian.PutUint32(out[12:16], uint32(slot*131+index*17+0x12345))
	return out
}

func uniqueSentinel8(tag byte, slot, index int) [8]byte {
	var out [8]byte
	out[0] = tag
	out[1] = 0x3c
	binary.LittleEndian.PutUint16(out[2:4], uint16(slot+1))
	binary.LittleEndian.PutUint32(out[4:8], uint32(index+1)|(uint32(tag)<<24))
	return out
}

func deriveTraceID(salt, domain uint64, workerID int, seq uint64, index int) [16]byte {
	var out [16]byte
	hi := splitmix64(salt ^ (domain << 56) ^ (uint64(workerID+1) << 40) ^ seq ^ (uint64(index+1) << 16))
	lo := splitmix64(hi ^ 0x9e3779b97f4a7c15 ^ uint64(index+1))
	if hi == 0 && lo == 0 {
		lo = 1
	}
	binary.LittleEndian.PutUint64(out[0:8], hi)
	binary.LittleEndian.PutUint64(out[8:16], lo)
	return out
}

func deriveSpanID(salt, domain uint64, workerID int, seq uint64, index int) [8]byte {
	var out [8]byte
	v := splitmix64(salt ^ (domain << 48) ^ (uint64(workerID+1) << 32) ^ (seq * 0xbf58476d1ce4e5b9) ^ uint64(index+1))
	if v == 0 {
		v = 1
	}
	binary.LittleEndian.PutUint64(out[:], v)
	return out
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: stringValue(value)}
}

func stringValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}
