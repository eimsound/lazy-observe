package otlpgrpc_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/otlpgrpc"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logsdata "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracedata "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type discardRoundTripper struct{}

func (discardRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    r,
	}, nil
}

type legacyTraceServer struct {
	tracepb.UnimplementedTraceServiceServer
	backend    storagepkg.SignalBackend
	httpClient *http.Client
}

func (s *legacyTraceServer) Export(
	ctx context.Context,
	req *tracepb.ExportTraceServiceRequest,
) (*tracepb.ExportTraceServiceResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty")
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.backend.TracesIngestBase()+"/insert/opentelemetry/v1/traces",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	_ = s.backend.ApplyTenantHeaders(httpReq.Header, 42)
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return &tracepb.ExportTraceServiceResponse{}, nil
}

type legacyLogsServer struct {
	logspb.UnimplementedLogsServiceServer
	backend    storagepkg.SignalBackend
	httpClient *http.Client
}

func (s *legacyLogsServer) Export(
	ctx context.Context,
	req *logspb.ExportLogsServiceRequest,
) (*logspb.ExportLogsServiceResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty")
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.backend.LogsIngestBase()+"/insert/opentelemetry/v1/logs",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	_ = s.backend.ApplyTenantHeaders(httpReq.Header, 42)
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return &logspb.ExportLogsServiceResponse{}, nil
}

func benchmarkTracePayload(spansPerResource int) []byte {
	spans := make([]*tracedata.Span, 0, spansPerResource)
	for i := range spansPerResource {
		spans = append(spans, &tracedata.Span{
			TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(i)},
			SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, byte(i)},
			Name:              fmt.Sprintf("HTTP GET /api/v1/orders/%d", i),
			StartTimeUnixNano: 1700000000000000000 + uint64(i)*1000,
			EndTimeUnixNano:   1700000000005000000 + uint64(i)*1000,
			Attributes: []*commonpb.KeyValue{
				{
					Key:   "http.method",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}},
				},
				{
					Key:   "http.route",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "/api/v1/orders/:id"}},
				},
				{Key: "http.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}},
			},
		})
	}
	req := &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracedata.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{
						Key:   "service.name",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
					},
					{
						Key:   "deployment.environment",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "production"}},
					},
				},
			},
			ScopeSpans: []*tracedata.ScopeSpans{{Spans: spans}},
		}},
	}
	wire, err := proto.Marshal(req)
	if err != nil {
		panic(err)
	}
	return wire
}

func benchmarkLogsPayload(recordsPerResource int) []byte {
	records := make([]*logsdata.LogRecord, 0, recordsPerResource)
	for i := range recordsPerResource {
		records = append(records, &logsdata.LogRecord{
			TimeUnixNano: 1700000000000000000 + uint64(i)*1000,
			SeverityText: "INFO",
			Body: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{
					StringValue: fmt.Sprintf("processed order checkout request_id=req-%06d status=ok", i),
				},
			},
			Attributes: []*commonpb.KeyValue{
				{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
				},
				{
					Key:   "tenant.tier",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "enterprise"}},
				},
			},
		})
	}
	req := &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*logsdata.ResourceLogs{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{
						Key:   "service.name",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
					},
				},
			},
			ScopeLogs: []*logsdata.ScopeLogs{{LogRecords: records}},
		}},
	}
	wire, err := proto.Marshal(req)
	if err != nil {
		panic(err)
	}
	return wire
}

func dialBufconnClient(b *testing.B, lis *bufconn.Listener) *grpc.ClientConn {
	b.Helper()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatalf("grpc.NewClient(bufconn): %v", err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	return conn
}

func startPassthroughBenchServer(b *testing.B) *grpc.ClientConn {
	b.Helper()
	httpClient := &http.Client{Transport: discardRoundTripper{}}
	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		"http://bench.invalid",
		"http://bench.invalid",
		"http://bench.invalid",
		"http://bench.invalid",
		httpClient,
	)
	if err != nil {
		b.Fatal(err)
	}
	srv, err := otlpgrpc.NewServer(otlpgrpc.Config{
		Auth:       &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_bench": {ID: 42, Slug: "bench"}}},
		Storage:    backend,
		HTTPClient: httpClient,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		b.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	b.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		_ = lis.Close()
	})
	return dialBufconnClient(b, lis)
}

func startLegacyBenchServer(b *testing.B) *grpc.ClientConn {
	b.Helper()
	httpClient := &http.Client{Transport: discardRoundTripper{}}
	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		"http://bench.invalid",
		"http://bench.invalid",
		"http://bench.invalid",
		"http://bench.invalid",
		httpClient,
	)
	if err != nil {
		b.Fatal(err)
	}
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_bench": {ID: 42, Slug: "bench"}}}
	gsrv := grpc.NewServer(grpc.UnaryInterceptor(func(
		ctx context.Context,
		req any,
		_ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("authorization")) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing auth")
		}
		if _, err := auth.AuthenticateProjectToken(ctx, "lo_bench"); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}))
	tracepb.RegisterTraceServiceServer(gsrv, &legacyTraceServer{backend: backend, httpClient: httpClient})
	logspb.RegisterLogsServiceServer(gsrv, &legacyLogsServer{backend: backend, httpClient: httpClient})

	lis := bufconn.Listen(1 << 20)
	go func() { _ = gsrv.Serve(lis) }()
	b.Cleanup(func() {
		gsrv.Stop()
		_ = lis.Close()
	})
	return dialBufconnClient(b, lis)
}

func BenchmarkOTLPGRPCExportPassthrough(b *testing.B) {
	traceWire := benchmarkTracePayload(50)
	logsWire := benchmarkLogsPayload(100)
	rawCodecOpt := grpc.ForceCodecV2(newRawClientCodec())
	ctx := withAuthHeader(context.Background(), "Bearer lo_bench")

	b.Run("Traces/Passthrough", func(b *testing.B) {
		conn := startPassthroughBenchServer(b)
		b.ReportAllocs()
		b.SetBytes(int64(len(traceWire)))
		b.ResetTimer()
		for b.Loop() {
			var resp tracepb.ExportTraceServiceResponse
			if err := conn.Invoke(
				ctx,
				tracepb.TraceService_Export_FullMethodName,
				rawWireBytes(traceWire),
				&resp,
				rawCodecOpt,
			); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
	})

	b.Run("Traces/LegacyUnmarshalRemarshal", func(b *testing.B) {
		conn := startLegacyBenchServer(b)
		b.ReportAllocs()
		b.SetBytes(int64(len(traceWire)))
		b.ResetTimer()
		for b.Loop() {
			var resp tracepb.ExportTraceServiceResponse
			if err := conn.Invoke(
				ctx,
				tracepb.TraceService_Export_FullMethodName,
				rawWireBytes(traceWire),
				&resp,
				rawCodecOpt,
			); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
	})

	b.Run("Logs/Passthrough", func(b *testing.B) {
		conn := startPassthroughBenchServer(b)
		b.ReportAllocs()
		b.SetBytes(int64(len(logsWire)))
		b.ResetTimer()
		for b.Loop() {
			var resp logspb.ExportLogsServiceResponse
			if err := conn.Invoke(
				ctx,
				logspb.LogsService_Export_FullMethodName,
				rawWireBytes(logsWire),
				&resp,
				rawCodecOpt,
			); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
	})

	b.Run("Logs/LegacyUnmarshalRemarshal", func(b *testing.B) {
		conn := startLegacyBenchServer(b)
		b.ReportAllocs()
		b.SetBytes(int64(len(logsWire)))
		b.ResetTimer()
		for b.Loop() {
			var resp logspb.ExportLogsServiceResponse
			if err := conn.Invoke(
				ctx,
				logspb.LogsService_Export_FullMethodName,
				rawWireBytes(logsWire),
				&resp,
				rawCodecOpt,
			); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
	})
}
