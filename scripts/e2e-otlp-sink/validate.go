package main

import (
	"fmt"
	"strconv"
	"strings"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

const metricsMarkerAttribute = "scale_gate_run_id"
const metricsSeriesAttribute = "series"
const logsSequenceAttribute = "sequence"
const logsMessagePrefix = "lazy-observe horizontal scaling gate "

func validateOTLPBatch(kind signalKind, body []byte, expectedBatch int) error {
	if expectedBatch <= 0 {
		return fmt.Errorf("expected batch must be positive")
	}
	switch kind {
	case signalMetrics:
		request := &metricspb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			return fmt.Errorf("decode metrics protobuf")
		}
		return validateMetricsBatch(request, expectedBatch)
	case signalLogs:
		request := &logspb.ExportLogsServiceRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			return fmt.Errorf("decode logs protobuf")
		}
		return validateLogsBatch(request, expectedBatch)
	default:
		return fmt.Errorf("unsupported signal kind")
	}
}

type batchState struct {
	expected  int
	total     int
	marker    string
	markerSet bool
	sequences []bool
}

func newBatchState(expected int) *batchState {
	return &batchState{expected: expected, sequences: make([]bool, expected)}
}

func (state *batchState) observe(marker, sequence string) error {
	if strings.TrimSpace(marker) == "" {
		return fmt.Errorf("missing marker")
	}
	if !state.markerSet {
		state.marker, state.markerSet = marker, true
	} else if marker != state.marker {
		return fmt.Errorf("mismatched marker")
	}
	index, err := strconv.Atoi(sequence)
	if err != nil || index < 0 || index >= state.expected || strconv.Itoa(index) != sequence {
		return fmt.Errorf("sequence %q is outside the expected batch", sequence)
	}
	if state.sequences[index] {
		return fmt.Errorf("duplicate sequence %d", index)
	}
	state.sequences[index] = true
	state.total++
	return nil
}

func (state *batchState) finish() error {
	if state.total != state.expected {
		return fmt.Errorf("batch contains %d items; expected %d", state.total, state.expected)
	}
	for index, present := range state.sequences {
		if !present {
			return fmt.Errorf("batch is missing sequence %d", index)
		}
	}
	return nil
}

func validateMetricsBatch(request *metricspb.ExportMetricsServiceRequest, expected int) error {
	state := newBatchState(expected)
	for _, resource := range request.GetResourceMetrics() {
		for _, scope := range resource.GetScopeMetrics() {
			for _, metric := range scope.GetMetrics() {
				switch data := metric.GetData().(type) {
				case *metricsdata.Metric_Gauge:
					for _, point := range data.Gauge.GetDataPoints() {
						if err := observeMetricPoint(state, point.GetAttributes()); err != nil {
							return err
						}
					}
				case *metricsdata.Metric_Sum:
					for _, point := range data.Sum.GetDataPoints() {
						if err := observeMetricPoint(state, point.GetAttributes()); err != nil {
							return err
						}
					}
				case *metricsdata.Metric_Histogram:
					for _, point := range data.Histogram.GetDataPoints() {
						if err := observeMetricPoint(state, point.GetAttributes()); err != nil {
							return err
						}
					}
				case *metricsdata.Metric_ExponentialHistogram:
					for _, point := range data.ExponentialHistogram.GetDataPoints() {
						if err := observeMetricPoint(state, point.GetAttributes()); err != nil {
							return err
						}
					}
				case *metricsdata.Metric_Summary:
					for _, point := range data.Summary.GetDataPoints() {
						if err := observeMetricPoint(state, point.GetAttributes()); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return state.finish()
}

func observeMetricPoint(state *batchState, attributes []*commonpb.KeyValue) error {
	marker, err := requiredStringAttribute(attributes, metricsMarkerAttribute)
	if err != nil {
		return err
	}
	series, err := requiredStringAttribute(attributes, metricsSeriesAttribute)
	if err != nil {
		return err
	}
	return state.observe(marker, series)
}

func validateLogsBatch(request *logspb.ExportLogsServiceRequest, expected int) error {
	state := newBatchState(expected)
	for _, resource := range request.GetResourceLogs() {
		for _, scope := range resource.GetScopeLogs() {
			for _, record := range scope.GetLogRecords() {
				body, ok := stringValue(record.GetBody())
				if !ok || !strings.HasPrefix(body, logsMessagePrefix) {
					return fmt.Errorf("log body does not contain the scale marker")
				}
				sequence, err := requiredStringAttribute(record.GetAttributes(), logsSequenceAttribute)
				if err != nil {
					return err
				}
				if err := state.observe(strings.TrimPrefix(body, logsMessagePrefix), sequence); err != nil {
					return err
				}
			}
		}
	}
	return state.finish()
}

func requiredStringAttribute(attributes []*commonpb.KeyValue, key string) (string, error) {
	value := ""
	found := false
	for _, attribute := range attributes {
		if attribute == nil || attribute.GetKey() != key {
			continue
		}
		if found {
			return "", fmt.Errorf("duplicate %s attribute", key)
		}
		var ok bool
		value, ok = stringValue(attribute.GetValue())
		if !ok {
			return "", fmt.Errorf("%s attribute must be a string", key)
		}
		found = true
	}
	if !found {
		return "", fmt.Errorf("missing %s attribute", key)
	}
	return value, nil
}

func stringValue(value *commonpb.AnyValue) (string, bool) {
	if value == nil {
		return "", false
	}
	text, ok := value.GetValue().(*commonpb.AnyValue_StringValue)
	if !ok {
		return "", false
	}
	return text.StringValue, true
}
