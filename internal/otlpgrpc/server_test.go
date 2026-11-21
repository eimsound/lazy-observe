package otlpgrpc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/gateway"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/otlpgrpc"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logsdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracedata "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type stubAuth struct {
	mu     sync.RWMutex
	tokens map[string]meta.ProjectInfo
	err    error
}

func (a *stubAuth) AuthenticateProjectToken(_ context.Context, token string) (meta.ProjectInfo, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.err != nil {
		return meta.ProjectInfo{}, a.err
	}
	project, ok := a.tokens[token]
	if !ok {
		return meta.ProjectInfo{}, meta.ErrInvalidProjectAPIToken
	}
	return project, nil
}

type capturedRequest struct {
	path        string
	rawQuery    string
	contentType string
	accountID   string
	projectID   string
	body        []byte
}

func startTestServer(
	t *testing.T,
	cfg otlpgrpc.Config,
	dialOpts ...grpc.DialOption,
) (*otlpgrpc.Server, *grpc.ClientConn, <-chan error) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv, err := otlpgrpc.NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := <-serveErr; err != nil {
			t.Errorf("otlpgrpc.Server.Serve returned error: %v", err)
		}
	})

	if len(dialOpts) == 0 {
		dialOpts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(ln.Addr().String(), dialOpts...)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return srv, conn, serveErr
}

func withAuthHeader(ctx context.Context, value string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", value))
}

func sampleTraceRequest(spanName string) *tracepb.ExportTraceServiceRequest {
	return &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracedata.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
				}},
			},
			ScopeSpans: []*tracedata.ScopeSpans{{
				Spans: []*tracedata.Span{{
					TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
					SpanId:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
					Name:    spanName,
				}},
			}},
		}},
	}
}

func sampleMetricsRequest(metricName string) *metricspb.ExportMetricsServiceRequest {
	return &metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsdata.ResourceMetrics{{
			ScopeMetrics: []*metricsdata.ScopeMetrics{{
				Metrics: []*metricsdata.Metric{{
					Name: metricName,
					Data: &metricsdata.Metric_Gauge{
						Gauge: &metricsdata.Gauge{
							DataPoints: []*metricsdata.NumberDataPoint{{
								Value: &metricsdata.NumberDataPoint_AsDouble{AsDouble: 42.5},
							}},
						},
					},
				}},
			}},
		}},
	}
}

func sampleLogsRequest(bodyText string) *logspb.ExportLogsServiceRequest {
	return &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*logsdata.ResourceLogs{{
			ScopeLogs: []*logsdata.ScopeLogs{{
				LogRecords: []*logsdata.LogRecord{{
					Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: bodyText}},
				}},
			}},
		}},
	}
}

func TestOTLPGRPCExportTracesMetricsLogsEndToEnd(t *testing.T) {
	var mu sync.Mutex
	captured := make(map[string]capturedRequest)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		captured[r.URL.Path] = capturedRequest{
			path:        r.URL.Path,
			rawQuery:    r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			accountID:   r.Header.Get("AccountID"),
			projectID:   r.Header.Get("ProjectID"),
			body:        body,
		}
		mu.Unlock()

		// Return a protobuf ExportTraceServiceResponse with partial_success on traces
		// and 204 No Content on metrics/logs to cover both response shapes.
		if strings.HasSuffix(r.URL.Path, "/traces") {
			raw, _ := proto.Marshal(&tracepb.ExportTraceServiceResponse{
				PartialSuccess: &tracepb.ExportTracePartialSuccess{
					RejectedSpans: 1,
					ErrorMessage:  "dropped invalid span",
				},
			})
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(raw)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	backend, err := storagepkg.NewWithOptions(storagepkg.BackendOptions{
		MetricsKind:      storagepkg.MetricsVM,
		MetricsQueryURL:  upstream.URL,
		MetricsIngestURL: upstream.URL,
		Metrics: storagepkg.MetricsOptions{
			Cluster:         true,
			TenantAccountID: "7",
		},
		Logs:   storagepkg.SignalEndpoint{URL: upstream.URL},
		Traces: storagepkg.SignalEndpoint{URL: upstream.URL},
	}, upstream.Client())
	if err != nil {
		t.Fatalf("storage.NewWithOptions: %v", err)
	}

	auth := &stubAuth{
		tokens: map[string]meta.ProjectInfo{
			"lo_alpha_valid": {ID: 42, Slug: "alpha"},
		},
	}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:       auth,
		Storage:    backend,
		HTTPClient: upstream.Client(),
	})

	traceClient := tracepb.NewTraceServiceClient(conn)
	metricsClient := metricspb.NewMetricsServiceClient(conn)
	logsClient := logspb.NewLogsServiceClient(conn)

	// Traces with standard "Bearer" + gzip compression.
	traceResp, err := traceClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_alpha_valid"),
		sampleTraceRequest("GET /checkout"),
		grpc.UseCompressor(gzip.Name),
	)
	if err != nil {
		t.Fatalf("TraceService.Export failed: %v", err)
	}
	if traceResp.GetPartialSuccess().GetRejectedSpans() != 1 ||
		traceResp.GetPartialSuccess().GetErrorMessage() != "dropped invalid span" {
		t.Fatalf("unexpected trace partial_success=%+v", traceResp.GetPartialSuccess())
	}

	// Metrics with lowercase "bearer".
	if _, err := metricsClient.Export(
		withAuthHeader(context.Background(), "bearer lo_alpha_valid"),
		sampleMetricsRequest("http_requests_total"),
	); err != nil {
		t.Fatalf("MetricsService.Export failed: %v", err)
	}

	// Logs with uppercase "BEARER".
	if _, err := logsClient.Export(
		withAuthHeader(context.Background(), "BEARER lo_alpha_valid"),
		sampleLogsRequest("order created"),
	); err != nil {
		t.Fatalf("LogsService.Export failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	traceReq, ok := captured["/insert/opentelemetry/v1/traces"]
	if !ok {
		t.Fatalf("missing traces upstream request; captured paths=%v", captured)
	}
	if traceReq.contentType != "application/x-protobuf" || traceReq.accountID != "7" || traceReq.projectID != "42" {
		t.Fatalf("unexpected traces headers: %+v", traceReq)
	}
	var gotTrace tracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(traceReq.body, &gotTrace); err != nil {
		t.Fatalf("unmarshal forwarded trace: %v", err)
	}
	if got := gotTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Name; got != "GET /checkout" {
		t.Fatalf("forwarded span name=%q want %q", got, "GET /checkout")
	}

	metricsReq, ok := captured["/insert/7/opentelemetry/v1/metrics"]
	if !ok {
		t.Fatalf("missing clustered metrics upstream request; captured paths=%v", captured)
	}
	if metricsReq.contentType != "application/x-protobuf" || metricsReq.rawQuery != "extra_label=project_id%3Dalpha" {
		t.Fatalf("unexpected metrics request: contentType=%q rawQuery=%q", metricsReq.contentType, metricsReq.rawQuery)
	}
	var gotMetrics metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(metricsReq.body, &gotMetrics); err != nil {
		t.Fatalf("unmarshal forwarded metrics: %v", err)
	}
	if got := gotMetrics.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Name; got != "http_requests_total" {
		t.Fatalf("forwarded metric name=%q want %q", got, "http_requests_total")
	}

	logsReq, ok := captured["/insert/opentelemetry/v1/logs"]
	if !ok {
		t.Fatalf("missing logs upstream request; captured paths=%v", captured)
	}
	if logsReq.contentType != "application/x-protobuf" || logsReq.accountID != "7" || logsReq.projectID != "42" {
		t.Fatalf("unexpected logs headers: %+v", logsReq)
	}
	var gotLogs logspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(logsReq.body, &gotLogs); err != nil {
		t.Fatalf("unmarshal forwarded logs: %v", err)
	}
	if got := gotLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Body.GetStringValue(); got != "order created" {
		t.Fatalf("forwarded log body=%q want %q", got, "order created")
	}
}

func TestOTLPGRPCAuthenticationAndTokenKinds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}

	auth := &stubAuth{
		tokens: map[string]meta.ProjectInfo{
			"lo_good": {ID: 1, Slug: "default"},
		},
	}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:       auth,
		Storage:    backend,
		HTTPClient: upstream.Client(),
	})
	traceClient := tracepb.NewTraceServiceClient(conn)

	cases := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{
			name:     "missing metadata",
			ctx:      context.Background(),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "wrong auth scheme",
			ctx:      withAuthHeader(context.Background(), "Basic dXNlcjpwYXNz"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "empty bearer token",
			ctx:      withAuthHeader(context.Background(), "Bearer   "),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "token with whitespace",
			ctx:      withAuthHeader(context.Background(), "Bearer lo_good extra"),
			wantCode: codes.Unauthenticated,
		},
		{
			name: "ambiguous duplicate authorization headers",
			ctx: metadata.NewOutgoingContext(
				context.Background(),
				metadata.Pairs("authorization", "Bearer lo_good", "authorization", "Bearer lo_other"),
			),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "unknown prefix token",
			ctx:      withAuthHeader(context.Background(), "Bearer secret_123"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "invalid project token",
			ctx:      withAuthHeader(context.Background(), "Bearer lo_unknown"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "user API token rejected as PermissionDenied",
			ctx:      withAuthHeader(context.Background(), "Bearer lou_user_secret"),
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "source map upload token rejected as PermissionDenied",
			ctx:      withAuthHeader(context.Background(), "Bearer "+meta.SourceMapUploadTokenPrefix+"upload_secret"),
			wantCode: codes.PermissionDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := traceClient.Export(tc.ctx, sampleTraceRequest("auth-check"))
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code=%s want=%s (err=%v)", status.Code(err), tc.wantCode, err)
			}
		})
	}

	// Auth store outage must map to Unavailable without leaking internal DB details.
	auth.mu.Lock()
	auth.err = errors.New("postgres://admin:supersecret@db.internal:5432/meta connection refused")
	auth.mu.Unlock()
	_, err = traceClient.Export(withAuthHeader(context.Background(), "Bearer lo_good"), sampleTraceRequest("db-down"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("auth outage code=%s want=Unavailable (err=%v)", status.Code(err), err)
	}
	if strings.Contains(err.Error(), "supersecret") || strings.Contains(err.Error(), "db.internal") {
		t.Fatalf("auth outage error leaked internal store details: %v", err)
	}
}

func TestOTLPGRPCWithSharedGatewayAuthenticatorAndRevocation(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "Orders", "orders")
	if err != nil {
		t.Fatal(err)
	}
	_, rawToken, err := store.CreateNamedProjectAPIToken(ctx, project.ID, "grpc-exporter")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}

	sharedAuth := gateway.NewAuthenticator(store)
	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:       sharedAuth,
		Storage:    backend,
		HTTPClient: upstream.Client(),
	})
	logsClient := logspb.NewLogsServiceClient(conn)

	if _, err := logsClient.Export(
		withAuthHeader(ctx, "Bearer "+rawToken),
		sampleLogsRequest("before revoke"),
	); err != nil {
		t.Fatalf("export with active token failed: %v", err)
	}

	// Revoking the token advances store.Revision(), invalidating the shared cache immediately.
	if err := store.RevokeProjectAPIToken(ctx, rawToken); err != nil {
		t.Fatal(err)
	}
	_, err = logsClient.Export(withAuthHeader(ctx, "Bearer "+rawToken), sampleLogsRequest("after revoke"))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("post-revoke code=%s want=Unauthenticated (err=%v)", status.Code(err), err)
	}
}

func TestOTLPGRPCMessageSizeConcurrencyAndDisabledMetrics(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/insert/opentelemetry/v1/logs" {
			started <- struct{}{}
			<-gate
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	// MetricsIngestURL intentionally empty to verify Unimplemented mapping.
	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		upstream.URL,
		"",
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}

	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_valid": {ID: 3, Slug: "gamma"}}}
	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:           auth,
		Storage:        backend,
		HTTPClient:     upstream.Client(),
		MaxRecvMsgSize: 2048,
		MaxInFlight:    1,
	})

	// 1. Oversized message rejected with ResourceExhausted.
	traceClient := tracepb.NewTraceServiceClient(conn)
	_, err = traceClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_valid"),
		sampleTraceRequest(strings.Repeat("x", 4096)),
	)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized request code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
	}

	// 2. Disabled metrics ingest maps to Unimplemented.
	metricsClient := metricspb.NewMetricsServiceClient(conn)
	_, err = metricsClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_valid"),
		sampleMetricsRequest("cpu_usage"),
	)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("disabled metrics code=%s want=Unimplemented (err=%v)", status.Code(err), err)
	}

	// 3. Concurrency cap rejects the second concurrent export with ResourceExhausted.
	logsClient := logspb.NewLogsServiceClient(conn)
	firstDone := make(chan error, 1)
	go func() {
		_, exportErr := logsClient.Export(
			withAuthHeader(context.Background(), "Bearer lo_valid"),
			sampleLogsRequest("in-flight"),
		)
		firstDone <- exportErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first export did not reach upstream")
	}

	_, err = logsClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_valid"),
		sampleLogsRequest("overflow"),
	)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("concurrency overflow code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
	}
	close(gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("first export failed: %v", err)
	}
}

func TestOTLPGRPCUpstreamStatusMappingAndErrorSanitization(t *testing.T) {
	var mu sync.Mutex
	upstreamStatus := http.StatusInternalServerError
	upstreamBody := "internal error: dial tcp 10.244.1.9:9428 victoria-logs secret-host"
	var upstreamDelay time.Duration

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := upstreamStatus
		body := upstreamBody
		delay := upstreamDelay
		mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(st)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_valid": {ID: 5, Slug: "delta"}}}
	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:            auth,
		Storage:         backend,
		HTTPClient:      upstream.Client(),
		UpstreamTimeout: 150 * time.Millisecond,
	})
	traceClient := tracepb.NewTraceServiceClient(conn)

	cases := []struct {
		name       string
		httpStatus int
		delay      time.Duration
		wantCode   codes.Code
	}{
		{name: "500 -> Unavailable", httpStatus: http.StatusInternalServerError, wantCode: codes.Unavailable},
		{name: "502 -> Unavailable", httpStatus: http.StatusBadGateway, wantCode: codes.Unavailable},
		{name: "503 -> Unavailable", httpStatus: http.StatusServiceUnavailable, wantCode: codes.Unavailable},
		{name: "504 -> DeadlineExceeded", httpStatus: http.StatusGatewayTimeout, wantCode: codes.DeadlineExceeded},
		{name: "400 -> InvalidArgument", httpStatus: http.StatusBadRequest, wantCode: codes.InvalidArgument},
		{
			name:       "413 -> ResourceExhausted",
			httpStatus: http.StatusRequestEntityTooLarge,
			wantCode:   codes.ResourceExhausted,
		},
		{name: "429 -> ResourceExhausted", httpStatus: http.StatusTooManyRequests, wantCode: codes.ResourceExhausted},
		{name: "404 -> Unimplemented", httpStatus: http.StatusNotFound, wantCode: codes.Unimplemented},
		{
			name:       "upstream timeout -> DeadlineExceeded",
			httpStatus: http.StatusOK,
			delay:      400 * time.Millisecond,
			wantCode:   codes.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			upstreamStatus = tc.httpStatus
			upstreamDelay = tc.delay
			mu.Unlock()

			_, err := traceClient.Export(
				withAuthHeader(context.Background(), "Bearer lo_valid"),
				sampleTraceRequest("err-check"),
			)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code=%s want=%s (err=%v)", status.Code(err), tc.wantCode, err)
			}
			msg := status.Convert(err).Message()
			if strings.Contains(msg, "10.244.1.9") ||
				strings.Contains(msg, "secret-host") ||
				strings.Contains(msg, upstream.URL) {
				t.Fatalf("gRPC error leaked upstream details: %q", msg)
			}
		})
	}

	// Unreachable upstream must map to Unavailable without exposing upstream host/port.
	upstream.Close()
	_, err = traceClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_valid"),
		sampleTraceRequest("unreachable"),
	)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("unreachable upstream code=%s want=Unavailable (err=%v)", status.Code(err), err)
	}
	if strings.Contains(status.Convert(err).Message(), "127.0.0.1") {
		t.Fatalf("unreachable upstream error leaked address: %v", err)
	}
}

func TestOTLPGRPCNativeTLSEndToEnd(t *testing.T) {
	serverTLS, clientTLS := selfSignedLoopbackTLS(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_tls": {ID: 9, Slug: "tls-proj"}}}
	_, conn, _ := startTestServer(
		t,
		otlpgrpc.Config{
			Auth:       auth,
			Storage:    backend,
			HTTPClient: upstream.Client(),
			TLSConfig:  serverTLS,
		},
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)

	traceClient := tracepb.NewTraceServiceClient(conn)
	if _, err := traceClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_tls"),
		sampleTraceRequest("tls-span"),
	); err != nil {
		t.Fatalf("TLS TraceService.Export failed: %v", err)
	}
}

func TestOTLPGRPCGracefulShutdownAndForcedTimeoutFallback(t *testing.T) {
	t.Run("graceful stop waits for in-flight RPC", func(t *testing.T) {
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- struct{}{}
			<-release
			w.WriteHeader(http.StatusNoContent)
		}))
		defer upstream.Close()

		backend, err := storagepkg.New(
			storagepkg.MetricsVM,
			upstream.URL,
			upstream.URL,
			upstream.URL,
			upstream.URL,
			upstream.Client(),
		)
		if err != nil {
			t.Fatal(err)
		}
		auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_shut": {ID: 10, Slug: "shut"}}}
		srv, conn, _ := startTestServer(t, otlpgrpc.Config{
			Auth:       auth,
			Storage:    backend,
			HTTPClient: upstream.Client(),
		})
		logsClient := logspb.NewLogsServiceClient(conn)

		rpcErr := make(chan error, 1)
		go func() {
			_, err := logsClient.Export(
				withAuthHeader(context.Background(), "Bearer lo_shut"),
				sampleLogsRequest("graceful"),
			)
			rpcErr <- err
		}()
		<-started

		shutdownErr := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			shutdownErr <- srv.Shutdown(ctx)
		}()

		time.Sleep(30 * time.Millisecond)
		close(release)

		if err := <-rpcErr; err != nil {
			t.Fatalf("in-flight RPC failed during graceful shutdown: %v", err)
		}
		if err := <-shutdownErr; err != nil {
			t.Fatalf("graceful Shutdown failed: %v", err)
		}
		// Calling Shutdown a second time must be idempotent.
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Fatalf("second Shutdown returned error: %v", err)
		}
	})

	t.Run("expired context forces immediate stop", func(t *testing.T) {
		started := make(chan struct{}, 1)
		release := make(chan struct{})

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer upstream.Close()
		defer close(release)

		backend, err := storagepkg.New(
			storagepkg.MetricsVM,
			upstream.URL,
			upstream.URL,
			upstream.URL,
			upstream.URL,
			upstream.Client(),
		)
		if err != nil {
			t.Fatal(err)
		}
		auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_force": {ID: 11, Slug: "force"}}}
		srv, conn, _ := startTestServer(t, otlpgrpc.Config{
			Auth:       auth,
			Storage:    backend,
			HTTPClient: upstream.Client(),
		})
		logsClient := logspb.NewLogsServiceClient(conn)

		go func() {
			_, _ = logsClient.Export(
				withAuthHeader(context.Background(), "Bearer lo_force"),
				sampleLogsRequest("stuck"),
			)
		}()
		<-started

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("forced Shutdown err=%v want context.DeadlineExceeded", err)
		}
	})
}

func selfSignedLoopbackTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	serverTLS := &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}},
	}
	clientTLS := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	}
	return serverTLS, clientTLS
}
