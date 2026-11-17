// Command e2e-otlp-payload emits a small OTLP protobuf request for shell E2E tests.
package main

import (
	"flag"
	"fmt"
	"os"
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

func main() {
	kind := flag.String("kind", "", "payload kind: metrics or logs")
	name := flag.String("name", "", "metric name or log body")
	value := flag.Float64("value", 0, "metric value")
	count := flag.Int("count", 1, "number of metric points or log records (1-200000)")
	flag.Parse()
	if *name == "" {
		fatal("-name is required")
	}
	if *count < 1 || *count > 200000 {
		fatal("-count must be between 1 and 200000")
	}

	var message proto.Message
	switch *kind {
	case "metrics":
		message = metricsPayload(*name, *value, *count)
	case "logs":
		message = logsPayload(*name, *count)
	default:
		fatal("-kind must be metrics or logs")
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		fatal("marshal OTLP payload: %v", err)
	}
	if _, err := os.Stdout.Write(payload); err != nil {
		fatal("write OTLP payload: %v", err)
	}
}

func metricsPayload(name string, value float64, count int) *metricspb.ExportMetricsServiceRequest {
	points := make([]*metricsdata.NumberDataPoint, count)
	for index := range points {
		points[index] = &metricsdata.NumberDataPoint{
			TimeUnixNano: uint64(time.Now().UnixNano()),
			Attributes: []*commonpb.KeyValue{
				{
					Key:   "fixture.sequence",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strconv.Itoa(index)}},
				},
			},
			Value: &metricsdata.NumberDataPoint_AsDouble{AsDouble: value},
		}
	}
	return &metricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricsdata.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "lazy-observe-ha-e2e"}},
			},
		}},
		ScopeMetrics: []*metricsdata.ScopeMetrics{{Metrics: []*metricsdata.Metric{{
			Name: name,
			Data: &metricsdata.Metric_Gauge{Gauge: &metricsdata.Gauge{DataPoints: points}},
		}}}},
	}}}
}

func logsPayload(body string, count int) *logspb.ExportLogsServiceRequest {
	records := make([]*logdata.LogRecord, count)
	for index := range records {
		records[index] = &logdata.LogRecord{
			TimeUnixNano: uint64(time.Now().UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
			Attributes: []*commonpb.KeyValue{
				{
					Key:   "fixture.sequence",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(index)}},
				},
			},
		}
	}
	return &logspb.ExportLogsServiceRequest{ResourceLogs: []*logdata.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "lazy-observe-ha-e2e"}},
			},
		}},
		ScopeLogs: []*logdata.ScopeLogs{{LogRecords: records}},
	}}}
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "e2e-otlp-payload: "+format+"\n", args...)
	os.Exit(2)
}
