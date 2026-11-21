package otlpgrpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	otlpProtobufContentType = "application/x-protobuf"
	maxResponseBodyBytes    = 1 << 20
	drainBodyLimitBytes     = 64 << 10
	maxErrorLogBodyBytes    = 4 << 10

	upstreamTracesPath  = "/insert/opentelemetry/v1/traces"
	upstreamMetricsPath = "/opentelemetry/v1/metrics"
	upstreamLogsPath    = "/insert/opentelemetry/v1/logs"
)

type traceService struct {
	tracepb.UnimplementedTraceServiceServer
	server *Server
}

func (svc *traceService) Export(
	ctx context.Context,
	req *tracepb.ExportTraceServiceRequest,
) (*tracepb.ExportTraceServiceResponse, error) {
	resp := &tracepb.ExportTraceServiceResponse{}
	if err := svc.server.exportTenantSignal(
		ctx,
		"traces",
		svc.server.storage.TracesIngestBase(),
		upstreamTracesPath,
		req,
		resp,
	); err != nil {
		return nil, err
	}
	return resp, nil
}

type metricsService struct {
	metricspb.UnimplementedMetricsServiceServer
	server *Server
}

func (svc *metricsService) Export(
	ctx context.Context,
	req *metricspb.ExportMetricsServiceRequest,
) (*metricspb.ExportMetricsServiceResponse, error) {
	if !svc.server.storage.MetricsIngestEnabled() {
		return nil, status.Error(codes.Unimplemented, "metrics ingest is disabled")
	}
	project, ok := projectFromContext(ctx)
	if !ok || project.ID <= 0 || strings.TrimSpace(project.Slug) == "" {
		return nil, status.Error(codes.Internal, "project context is missing")
	}
	extraLabel, err := svc.server.storage.MetricsIngestExtraLabel(project.Slug)
	if err != nil {
		return nil, status.Error(codes.Internal, "project context is missing")
	}
	query := url.Values{"extra_label": []string{extraLabel}}
	target := buildTargetURL(svc.server.storage.MetricsIngestBase(), upstreamMetricsPath, query)

	resp := &metricspb.ExportMetricsServiceResponse{}
	if err := svc.server.forwardProtobuf(ctx, "metrics", target, project, false, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

type logsService struct {
	logspb.UnimplementedLogsServiceServer
	server *Server
}

func (svc *logsService) Export(
	ctx context.Context,
	req *logspb.ExportLogsServiceRequest,
) (*logspb.ExportLogsServiceResponse, error) {
	resp := &logspb.ExportLogsServiceResponse{}
	if err := svc.server.exportTenantSignal(
		ctx,
		"logs",
		svc.server.storage.LogsIngestBase(),
		upstreamLogsPath,
		req,
		resp,
	); err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *Server) exportTenantSignal(
	ctx context.Context,
	signal string,
	baseURL string,
	upstreamPath string,
	reqMsg proto.Message,
	respMsg proto.Message,
) error {
	project, ok := projectFromContext(ctx)
	if !ok || project.ID <= 0 {
		return status.Error(codes.Internal, "project context is missing")
	}
	target := buildTargetURL(baseURL, upstreamPath, nil)
	return s.forwardProtobuf(ctx, signal, target, project, true, reqMsg, respMsg)
}

func (s *Server) forwardProtobuf(
	ctx context.Context,
	signal string,
	targetURL string,
	project meta.ProjectInfo,
	applyTenantHeaders bool,
	reqMsg proto.Message,
	respMsg proto.Message,
) error {
	if reqMsg == nil {
		return status.Error(codes.InvalidArgument, "empty OTLP export request")
	}
	payload, err := proto.Marshal(reqMsg)
	if err != nil {
		return status.Error(codes.InvalidArgument, "failed to encode OTLP request")
	}
	if len(payload) > s.maxRecvMsgSize {
		return status.Error(codes.ResourceExhausted, "OTLP request exceeds size limit")
	}

	reqCtx, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()
	if s.shutdownCtx != nil {
		stopAfter := context.AfterFunc(s.shutdownCtx, cancel)
		defer stopAfter()
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		s.logger.Error("otlp grpc build upstream request failed", "signal", signal, "error", err)
		return status.Error(codes.Internal, "failed to construct upstream request")
	}
	httpReq.Header.Set("Content-Type", otlpProtobufContentType)
	if applyTenantHeaders {
		if err := s.storage.ApplyTenantHeaders(httpReq.Header, project.ID); err != nil {
			return status.Error(codes.Internal, "project context is missing")
		}
	}

	httpResp, err := s.httpClient.Do(httpReq)
	if err != nil {
		code, msg := classifyTransportError(ctx, reqCtx, err)
		s.logger.Warn(
			"otlp grpc upstream export failed",
			"signal", signal,
			"project_id", project.ID,
			"grpc_code", code.String(),
			"error", err,
		)
		return status.Error(code, msg)
	}
	defer drainAndCloseBody(httpResp.Body)

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
	_ = proto.Unmarshal(respBody, respMsg)
	return nil
}

func buildTargetURL(base, upstreamPath string, query url.Values) string {
	target := strings.TrimRight(base, "/") + upstreamPath
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target
}

func drainAndCloseBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainBodyLimitBytes))
	_ = body.Close()
}

func classifyTransportError(parentCtx, reqCtx context.Context, err error) (codes.Code, string) {
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
		if statusCode >= http.StatusInternalServerError {
			return codes.Unavailable, "upstream storage unavailable"
		}
		return codes.InvalidArgument, "upstream rejected OTLP request"
	}
}
