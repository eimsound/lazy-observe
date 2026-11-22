package otlpgrpc

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	otlpProtobufContentType = "application/x-protobuf"
	maxResponseBodyBytes    = 1 << 20
	drainBodyLimitBytes     = 64 << 10
	maxErrorLogBodyBytes    = 4 << 10

	defaultRetryDelay  = time.Second
	maxRetryAfterDelay = 5 * time.Minute

	upstreamTracesPath  = "/insert/opentelemetry/v1/traces"
	upstreamMetricsPath = "/opentelemetry/v1/metrics"
	upstreamLogsPath    = "/insert/opentelemetry/v1/logs"
)

type rawExportServer interface {
	handleExportStream(stream grpc.ServerStream, spec exportSignalSpec) error
}

type exportSignalSpec struct {
	name          string
	fullMethod    string
	metricSignal  selfmetrics.Signal
	resolveTarget func(s *Server, project meta.ProjectInfo) (targetURL string, applyTenantHeaders bool, err error)
	newResponse   func() proto.Message
}

var (
	tracesExportSpec = exportSignalSpec{
		name:         "traces",
		fullMethod:   tracepb.TraceService_Export_FullMethodName,
		metricSignal: selfmetrics.SignalTraces,
		resolveTarget: func(s *Server, project meta.ProjectInfo) (string, bool, error) {
			if project.ID <= 0 {
				return "", false, status.Error(codes.Internal, "project context is missing")
			}
			return buildTargetURL(s.storage.TracesIngestBase(), upstreamTracesPath, nil), true, nil
		},
		newResponse: func() proto.Message {
			return &tracepb.ExportTraceServiceResponse{}
		},
	}

	metricsExportSpec = exportSignalSpec{
		name:         "metrics",
		fullMethod:   metricspb.MetricsService_Export_FullMethodName,
		metricSignal: selfmetrics.SignalMetrics,
		resolveTarget: func(s *Server, project meta.ProjectInfo) (string, bool, error) {
			if !s.storage.MetricsIngestEnabled() {
				return "", false, status.Error(codes.Unimplemented, "metrics ingest is disabled")
			}
			if project.ID <= 0 || strings.TrimSpace(project.Slug) == "" {
				return "", false, status.Error(codes.Internal, "project context is missing")
			}
			extraLabel, err := s.storage.MetricsIngestExtraLabel(project.Slug)
			if err != nil {
				return "", false, status.Error(codes.Internal, "project context is missing")
			}
			query := url.Values{"extra_label": []string{extraLabel}}
			return buildTargetURL(s.storage.MetricsIngestBase(), upstreamMetricsPath, query), false, nil
		},
		newResponse: func() proto.Message {
			return &metricspb.ExportMetricsServiceResponse{}
		},
	}

	logsExportSpec = exportSignalSpec{
		name:         "logs",
		fullMethod:   logspb.LogsService_Export_FullMethodName,
		metricSignal: selfmetrics.SignalLogs,
		resolveTarget: func(s *Server, project meta.ProjectInfo) (string, bool, error) {
			if project.ID <= 0 {
				return "", false, status.Error(codes.Internal, "project context is missing")
			}
			return buildTargetURL(s.storage.LogsIngestBase(), upstreamLogsPath, nil), true, nil
		},
		newResponse: func() proto.Message {
			return &logspb.ExportLogsServiceResponse{}
		},
	}

	traceRawServiceDesc = grpc.ServiceDesc{
		ServiceName: "opentelemetry.proto.collector.trace.v1.TraceService",
		HandlerType: (*rawExportServer)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Export",
			Handler:       _TraceService_RawExport_Handler,
			ClientStreams: false,
			ServerStreams: false,
		}},
		Metadata: "opentelemetry/proto/collector/trace/v1/trace_service.proto",
	}

	metricsRawServiceDesc = grpc.ServiceDesc{
		ServiceName: "opentelemetry.proto.collector.metrics.v1.MetricsService",
		HandlerType: (*rawExportServer)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Export",
			Handler:       _MetricsService_RawExport_Handler,
			ClientStreams: false,
			ServerStreams: false,
		}},
		Metadata: "opentelemetry/proto/collector/metrics/v1/metrics_service.proto",
	}

	logsRawServiceDesc = grpc.ServiceDesc{
		ServiceName: "opentelemetry.proto.collector.logs.v1.LogsService",
		HandlerType: (*rawExportServer)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Export",
			Handler:       _LogsService_RawExport_Handler,
			ClientStreams: false,
			ServerStreams: false,
		}},
		Metadata: "opentelemetry/proto/collector/logs/v1/logs_service.proto",
	}
)

func _TraceService_RawExport_Handler(srv any, stream grpc.ServerStream) error {
	return srv.(rawExportServer).handleExportStream(stream, tracesExportSpec)
}

func _MetricsService_RawExport_Handler(srv any, stream grpc.ServerStream) error {
	return srv.(rawExportServer).handleExportStream(stream, metricsExportSpec)
}

func _LogsService_RawExport_Handler(srv any, stream grpc.ServerStream) error {
	return srv.(rawExportServer).handleExportStream(stream, logsExportSpec)
}

func (s *Server) forwardRawProtobuf(
	ctx context.Context,
	signal string,
	targetURL string,
	project meta.ProjectInfo,
	applyTenantHeaders bool,
	rawReq *rawPayload,
	respMsg proto.Message,
) error {
	if rawReq == nil {
		return status.Error(codes.InvalidArgument, "empty OTLP export request")
	}
	payloadLen := rawReq.Len()
	if s.maxRecvMsgSize > 0 && payloadLen > s.maxRecvMsgSize {
		return status.Error(codes.ResourceExhausted, "OTLP request exceeds size limit")
	}

	reqCtx, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()
	if s.shutdownCtx != nil {
		stopAfter := context.AfterFunc(s.shutdownCtx, cancel)
		defer stopAfter()
	}

	var body io.Reader = http.NoBody
	if payloadLen > 0 {
		bodyReader := newRawBufferReader(rawReq.buf)
		defer bodyReader.Close()
		body = bodyReader
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, body)
	if err != nil {
		s.logger.Error("otlp grpc build upstream request failed", "signal", signal, "error", err)
		return status.Error(codes.Internal, "failed to construct upstream request")
	}
	httpReq.ContentLength = int64(payloadLen)
	httpReq.Header.Set("Content-Type", otlpProtobufContentType)
	if applyTenantHeaders {
		if err := s.storage.ApplyTenantHeaders(httpReq.Header, project.ID); err != nil {
			return status.Error(codes.Internal, "project context is missing")
		}
	}

	httpResp, err := s.httpClient.Do(httpReq)
	if err != nil {
		code, msg := classifyTransportError(ctx, reqCtx, s.shutdownCtx, err)
		s.logger.Warn(
			"otlp grpc upstream export failed",
			"signal", signal,
			"project_id", project.ID,
			"grpc_code", code.String(),
			"error", err,
		)
		return status.Error(code, msg)
	}
	defer httpResp.Body.Close()
	defer drainBody(httpResp.Body)

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		errSnippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorLogBodyBytes))
		code, msg := mapUpstreamHTTPStatus(httpResp.StatusCode)
		s.logger.Warn(
			"otlp grpc upstream returned non-2xx status",
			"signal", signal,
			"project_id", project.ID,
			"http_status", httpResp.StatusCode,
			"grpc_code", code.String(),
			"body_excerpt", strings.TrimSpace(string(errSnippet)),
		)
		if httpResp.StatusCode == http.StatusTooManyRequests {
			return throttledStatusError(msg, parseRetryAfter(httpResp.Header.Get("Retry-After")))
		}
		return status.Error(code, msg)
	}

	if httpResp.StatusCode == http.StatusNoContent || httpResp.ContentLength == 0 || respMsg == nil {
		return nil
	}
	respBody, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBodyBytes))
	if readErr != nil || len(respBody) == 0 {
		return nil
	}
	// Best-effort protobuf response decoding preserves partial_success details
	// when the upstream returns an OTLP Export*ServiceResponse, while remaining
	// tolerant of Victoria* endpoints that return an empty or plain-text 200.
	if err := proto.Unmarshal(respBody, respMsg); err != nil {
		proto.Reset(respMsg)
	}
	return nil
}

func buildTargetURL(base, upstreamPath string, query url.Values) string {
	target := strings.TrimRight(base, "/") + upstreamPath
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target
}

func drainBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainBodyLimitBytes))
}

func throttledStatusError(msg string, delay time.Duration) error {
	if delay <= 0 {
		delay = defaultRetryDelay
	}
	st := status.New(codes.ResourceExhausted, msg)
	detailed, err := st.WithDetails(&errdetails.RetryInfo{
		RetryDelay: durationpb.New(delay),
	})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

func hasRetryInfoDetail(st *status.Status) bool {
	if st == nil {
		return false
	}
	for _, detail := range st.Details() {
		if _, ok := detail.(*errdetails.RetryInfo); ok {
			return true
		}
	}
	return false
}

func parseRetryAfter(raw string) time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultRetryDelay
	}
	secs, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || secs <= 0 {
		return defaultRetryDelay
	}
	maxSecs := int64(maxRetryAfterDelay / time.Second)
	if secs > maxSecs {
		return maxRetryAfterDelay
	}
	return time.Duration(secs) * time.Second
}

func classifyTransportError(parentCtx, reqCtx, shutdownCtx context.Context, err error) (codes.Code, string) {
	if shutdownCtx != nil && shutdownCtx.Err() != nil {
		return codes.Unavailable, "server is shutting down"
	}
	if parentCtx != nil && errors.Is(parentCtx.Err(), context.Canceled) {
		return codes.Canceled, "request canceled"
	}
	if reqCtx != nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return codes.DeadlineExceeded, "upstream storage timed out"
	}
	if errors.Is(err, context.Canceled) {
		return codes.Canceled, "request canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return codes.DeadlineExceeded, "upstream storage timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return codes.DeadlineExceeded, "upstream storage timed out"
	}
	return codes.Unavailable, "upstream storage unavailable"
}

func mapUpstreamHTTPStatus(statusCode int) (codes.Code, string) {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return codes.InvalidArgument, "invalid OTLP payload"
	case http.StatusUnauthorized:
		return codes.Unauthenticated, "upstream rejected credentials"
	case http.StatusForbidden:
		return codes.PermissionDenied, "upstream rejected request"
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return codes.Unimplemented, "upstream OTLP endpoint unsupported"
	case http.StatusRequestEntityTooLarge:
		return codes.ResourceExhausted, "OTLP payload exceeds upstream limit"
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted, "upstream rate limit exceeded"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return codes.DeadlineExceeded, "upstream storage timed out"
	default:
		if statusCode >= http.StatusMultipleChoices && statusCode < http.StatusBadRequest {
			return codes.Unavailable, "upstream storage redirect blocked"
		}
		if statusCode >= http.StatusInternalServerError {
			return codes.Unavailable, "upstream storage unavailable"
		}
		return codes.InvalidArgument, "upstream rejected OTLP request"
	}
}
