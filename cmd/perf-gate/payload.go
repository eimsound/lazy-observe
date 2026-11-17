package main

import (
	"strconv"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func metricsLoadPayload(batchSize int) ([]byte, error) {
	now := uint64(time.Now().UnixNano())
	points := make([]*metricsdata.NumberDataPoint, batchSize)
	for index := range points {
		points[index] = &metricsdata.NumberDataPoint{
			TimeUnixNano: now,
			Attributes: []*commonpb.KeyValue{{
				Key: "series", Value: stringAnyValue(strconv.Itoa(index)),
			}},
			Value: &metricsdata.NumberDataPoint_AsInt{AsInt: int64(index)},
		}
	}
	request := &metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricsdata.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: loadServiceAttributes()},
		ScopeMetrics: []*metricsdata.ScopeMetrics{{Metrics: []*metricsdata.Metric{{
			Name: "lazy_observe_scale_gate_samples",
			Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{DataPoints: points}},
		}}}},
	}}}
	return proto.Marshal(request)
}

func logsLoadPayload(batchSize int) ([]byte, error) {
	now := uint64(time.Now().UnixNano())
	records := make([]*logdata.LogRecord, batchSize)
	for index := range records {
		records[index] = &logdata.LogRecord{
			TimeUnixNano: now,
			SeverityText: "INFO",
			Body:         stringAnyValue("lazy-observe horizontal scaling gate log record"),
			Attributes: []*commonpb.KeyValue{{
				Key: "sequence", Value: stringAnyValue(strconv.Itoa(index)),
			}},
		}
	}
	request := &logspb.ExportLogsServiceRequest{ResourceLogs: []*logdata.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: loadServiceAttributes()},
		ScopeLogs: []*logdata.ScopeLogs{{LogRecords: records}},
	}}}
	return proto.Marshal(request)
}

func loadServiceAttributes() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{{Key: "service.name", Value: stringAnyValue("lazy-observe-scale-gate")}}
}

func stringAnyValue(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}
