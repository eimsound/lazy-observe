package otlpgrpc_test

import (
	"bytes"
	stdgzip "compress/gzip"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/gateway"
	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/otlpgrpc"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logsdata "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsdata "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracedata "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
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

	// 1. Oversized message rejected with permanent ResourceExhausted (no RetryInfo).
	traceClient := tracepb.NewTraceServiceClient(conn)
	_, err = traceClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_valid"),
		sampleTraceRequest(strings.Repeat("x", 4096)),
	)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized request code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
	}
	if _, hasRetryInfo := retryInfoDelay(err); hasRetryInfo {
		t.Fatalf("oversized request must not attach RetryInfo: %v", err)
	}

	// 1b. Gzip bomb (< 2048B compressed, > 2048B decompressed) rejected without RetryInfo.
	gzipBombReq := sampleTraceRequest(strings.Repeat("a", 16384))
	rawBomb, marshalErr := proto.Marshal(gzipBombReq)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var compressed bytes.Buffer
	gzw := stdgzip.NewWriter(&compressed)
	if _, err := gzw.Write(rawBomb); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed.Len() >= 2048 || len(rawBomb) <= 2048 {
		t.Fatalf("test fixture precondition failed: compressed=%d raw=%d", compressed.Len(), len(rawBomb))
	}
	_, err = traceClient.Export(
		withAuthHeader(context.Background(), "Bearer lo_valid"),
		gzipBombReq,
		grpc.UseCompressor(gzip.Name),
	)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gzip bomb code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
	}
	if _, hasRetryInfo := retryInfoDelay(err); hasRetryInfo {
		t.Fatalf("gzip bomb must not attach RetryInfo: %v", err)
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

	// 3. Concurrency cap rejects the second concurrent export with retryable ResourceExhausted + RetryInfo.
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
	if delay, ok := retryInfoDelay(err); !ok || delay != time.Second {
		t.Fatalf("concurrency overflow RetryInfo=(%s, %v), want (1s, true)", delay, ok)
	}
	close(gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("first export failed: %v", err)
	}
}

func TestOTLPGRPCUpstreamStatusMappingAndErrorSanitization(t *testing.T) {
	var redirectHits atomic.Int64
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()

	var mu sync.Mutex
	upstreamStatus := http.StatusInternalServerError
	upstreamBody := "internal error: dial tcp 10.244.1.9:9428 victoria-logs secret-host"
	upstreamRetryAfter := ""
	upstreamLocation := ""
	var upstreamDelay time.Duration

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := upstreamStatus
		body := upstreamBody
		retryAfter := upstreamRetryAfter
		location := upstreamLocation
		delay := upstreamDelay
		mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if location != "" {
			w.Header().Set("Location", location)
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
		name           string
		httpStatus     int
		retryAfter     string
		location       string
		delay          time.Duration
		wantCode       codes.Code
		wantRetryInfo  bool
		wantRetryDelay time.Duration
	}{
		{name: "500 -> Unavailable", httpStatus: http.StatusInternalServerError, wantCode: codes.Unavailable},
		{name: "502 -> Unavailable", httpStatus: http.StatusBadGateway, wantCode: codes.Unavailable},
		{name: "503 -> Unavailable", httpStatus: http.StatusServiceUnavailable, wantCode: codes.Unavailable},
		{name: "504 -> DeadlineExceeded", httpStatus: http.StatusGatewayTimeout, wantCode: codes.DeadlineExceeded},
		{
			name:       "302 redirect -> Unavailable without following",
			httpStatus: http.StatusFound,
			location:   redirectTarget.URL + "/secret-redirect",
			wantCode:   codes.Unavailable,
		},
		{name: "400 -> InvalidArgument", httpStatus: http.StatusBadRequest, wantCode: codes.InvalidArgument},
		{
			name:          "413 -> permanent ResourceExhausted (no RetryInfo)",
			httpStatus:    http.StatusRequestEntityTooLarge,
			wantCode:      codes.ResourceExhausted,
			wantRetryInfo: false,
		},
		{
			name:           "429 without Retry-After -> ResourceExhausted with 1s RetryInfo",
			httpStatus:     http.StatusTooManyRequests,
			wantCode:       codes.ResourceExhausted,
			wantRetryInfo:  true,
			wantRetryDelay: time.Second,
		},
		{
			name:           "429 with Retry-After: 5 -> ResourceExhausted with 5s RetryInfo",
			httpStatus:     http.StatusTooManyRequests,
			retryAfter:     "5",
			wantCode:       codes.ResourceExhausted,
			wantRetryInfo:  true,
			wantRetryDelay: 5 * time.Second,
		},
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
			upstreamRetryAfter = tc.retryAfter
			upstreamLocation = tc.location
			upstreamDelay = tc.delay
			mu.Unlock()

			_, err := traceClient.Export(
				withAuthHeader(context.Background(), "Bearer lo_valid"),
				sampleTraceRequest("err-check"),
			)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code=%s want=%s (err=%v)", status.Code(err), tc.wantCode, err)
			}
			delay, hasRetryInfo := retryInfoDelay(err)
			if hasRetryInfo != tc.wantRetryInfo {
				t.Fatalf("hasRetryInfo=%v want=%v (err=%v)", hasRetryInfo, tc.wantRetryInfo, err)
			}
			if tc.wantRetryInfo && delay != tc.wantRetryDelay {
				t.Fatalf("RetryInfo delay=%s want=%s", delay, tc.wantRetryDelay)
			}
			msg := status.Convert(err).Message()
			if strings.Contains(msg, "10.244.1.9") ||
				strings.Contains(msg, "secret-host") ||
				strings.Contains(msg, "secret-redirect") ||
				strings.Contains(msg, upstream.URL) ||
				strings.Contains(msg, redirectTarget.URL) {
				t.Fatalf("gRPC error leaked upstream details: %q", msg)
			}
		})
	}
	if hits := redirectHits.Load(); hits != 0 {
		t.Fatalf("upstream client followed 302 redirect (%d hits)", hits)
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

		rpcErr := make(chan error, 1)
		go func() {
			_, callErr := logsClient.Export(
				withAuthHeader(context.Background(), "Bearer lo_force"),
				sampleLogsRequest("stuck"),
			)
			rpcErr <- callErr
		}()
		<-started

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("forced Shutdown err=%v want context.DeadlineExceeded", err)
		}
		if err := <-rpcErr; status.Code(err) != codes.Unavailable {
			t.Fatalf("forced stop RPC code=%s want=Unavailable (err=%v)", status.Code(err), err)
		}
	})
}

func retryInfoDelay(err error) (time.Duration, bool) {
	st := status.Convert(err)
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.RetryInfo); ok {
			return info.GetRetryDelay().AsDuration(), true
		}
	}
	return 0, false
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

type rawWireBytes []byte

type rawClientCodec struct {
	proto encoding.CodecV2
}

func newRawClientCodec() encoding.CodecV2 {
	return &rawClientCodec{proto: encoding.GetCodecV2("proto")}
}

func (c *rawClientCodec) Marshal(v any) (mem.BufferSlice, error) {
	if raw, ok := v.(rawWireBytes); ok {
		return mem.BufferSlice{mem.SliceBuffer(raw)}, nil
	}
	return c.proto.Marshal(v)
}

func (c *rawClientCodec) Unmarshal(data mem.BufferSlice, v any) error {
	return c.proto.Unmarshal(data, v)
}

func (c *rawClientCodec) Name() string {
	return "proto"
}

type countingAuth struct {
	calls atomic.Int64
	inner *stubAuth
}

func (a *countingAuth) AuthenticateProjectToken(ctx context.Context, token string) (meta.ProjectInfo, error) {
	a.calls.Add(1)
	return a.inner.AuthenticateProjectToken(ctx, token)
}

func TestOTLPGRPCAuthAndConcurrencyRunBeforeRecvMsg(t *testing.T) {
	t.Run("unauthenticated and wrong-token oversized/malformed requests fail before RecvMsg", func(t *testing.T) {
		var upstreamHits atomic.Int64
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			upstreamHits.Add(1)
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

		auth := &countingAuth{
			inner: &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_valid": {ID: 1, Slug: "alpha"}}},
		}
		_, conn, _ := startTestServer(t, otlpgrpc.Config{
			Auth:           auth,
			Storage:        backend,
			HTTPClient:     upstream.Client(),
			MaxRecvMsgSize: 1024,
		})

		traceClient := tracepb.NewTraceServiceClient(conn)

		// Oversized unauthenticated request must return Unauthenticated, not ResourceExhausted.
		_, err = traceClient.Export(context.Background(), sampleTraceRequest(strings.Repeat("x", 4096)))
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("oversized unauthenticated code=%s want=Unauthenticated (err=%v)", status.Code(err), err)
		}

		// Gzip bomb unauthenticated request must return Unauthenticated, not ResourceExhausted.
		_, err = traceClient.Export(
			context.Background(),
			sampleTraceRequest(strings.Repeat("z", 16384)),
			grpc.UseCompressor(gzip.Name),
		)
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("gzip bomb unauthenticated code=%s want=Unauthenticated (err=%v)", status.Code(err), err)
		}

		// Malformed wire frame without auth must return Unauthenticated, not InvalidArgument.
		var resp tracepb.ExportTraceServiceResponse
		err = conn.Invoke(
			context.Background(),
			tracepb.TraceService_Export_FullMethodName,
			rawWireBytes([]byte{0x80, 0x08, 0x01}),
			&resp,
			grpc.ForceCodecV2(newRawClientCodec()),
		)
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("malformed unauthenticated code=%s want=Unauthenticated (err=%v)", status.Code(err), err)
		}

		// Wrong token kind (lou_) with oversized request must return PermissionDenied, not ResourceExhausted.
		_, err = traceClient.Export(
			withAuthHeader(context.Background(), "Bearer lou_user_token"),
			sampleTraceRequest(strings.Repeat("u", 4096)),
		)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("user token oversized code=%s want=PermissionDenied (err=%v)", status.Code(err), err)
		}

		if upstreamHits.Load() != 0 {
			t.Fatalf("upstreamHits=%d want 0", upstreamHits.Load())
		}
	})

	t.Run("concurrency cap runs before auth and RecvMsg", func(t *testing.T) {
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

		auth := &countingAuth{
			inner: &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_valid": {ID: 1, Slug: "alpha"}}},
		}
		_, conn, _ := startTestServer(t, otlpgrpc.Config{
			Auth:           auth,
			Storage:        backend,
			HTTPClient:     upstream.Client(),
			MaxRecvMsgSize: 1024,
			MaxInFlight:    1,
		})

		traceClient := tracepb.NewTraceServiceClient(conn)
		firstDone := make(chan error, 1)
		go func() {
			_, callErr := traceClient.Export(
				withAuthHeader(context.Background(), "Bearer lo_valid"),
				sampleTraceRequest("hold-slot"),
			)
			firstDone <- callErr
		}()

		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("first export did not reach upstream")
		}

		callsBefore := auth.calls.Load()

		// Overflow with missing auth + oversized payload must hit concurrency first.
		_, err = traceClient.Export(context.Background(), sampleTraceRequest(strings.Repeat("x", 4096)))
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("overflow code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
		}
		if delay, ok := retryInfoDelay(err); !ok || delay != time.Second {
			t.Fatalf("overflow RetryInfo=(%s, %v) want (1s, true)", delay, ok)
		}

		// Overflow with valid token + malformed wire bytes must also stop at concurrency.
		var resp tracepb.ExportTraceServiceResponse
		err = conn.Invoke(
			withAuthHeader(context.Background(), "Bearer lo_valid"),
			tracepb.TraceService_Export_FullMethodName,
			rawWireBytes([]byte{0x80}),
			&resp,
			grpc.ForceCodecV2(newRawClientCodec()),
		)
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("overflow raw code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
		}
		if auth.calls.Load() != callsBefore {
			t.Fatalf("auth calls increased during concurrency saturation: before=%d after=%d",
				callsBefore, auth.calls.Load())
		}

		close(release)
		if err := <-firstDone; err != nil {
			t.Fatalf("first export failed: %v", err)
		}
	})
}

func TestOTLPGRPCMalformedWireFramesRejectedWithInvalidArgument(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
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

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	limiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Now:               func() time.Time { return now },
	})
	metricsReg := selfmetrics.New(selfmetrics.Config{Now: func() time.Time { return now }})
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_valid": {ID: 7, Slug: "proj7"}}}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:          auth,
		Storage:       backend,
		HTTPClient:    upstream.Client(),
		IngestLimiter: limiter,
		SelfMetrics:   metricsReg,
	})

	ctx := withAuthHeader(context.Background(), "Bearer lo_valid")
	rawCodecOpt := grpc.ForceCodecV2(newRawClientCodec())

	badFrames := []struct {
		name string
		raw  []byte
	}{
		{name: "truncated tag", raw: []byte{0x80}},
		{name: "truncated length", raw: []byte{0x0a, 0x10, 0x01, 0x02}},
		{name: "field 1 varint", raw: []byte{0x08, 0x01}},
		{name: "field 1 fixed32", raw: []byte{0x0d, 1, 2, 3, 4}},
		{name: "plaintext json", raw: []byte(`{"resourceSpans":[]}`)},
	}
	methods := []string{
		tracepb.TraceService_Export_FullMethodName,
		metricspb.MetricsService_Export_FullMethodName,
		logspb.LogsService_Export_FullMethodName,
	}

	for _, method := range methods {
		for _, bf := range badFrames {
			var resp tracepb.ExportTraceServiceResponse
			callErr := conn.Invoke(ctx, method, rawWireBytes(bf.raw), &resp, rawCodecOpt)
			if status.Code(callErr) != codes.InvalidArgument {
				t.Fatalf("%s (%s) code=%s want=InvalidArgument (err=%v)",
					method, bf.name, status.Code(callErr), callErr)
			}
		}
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("malformed frames hit upstream %d times", upstreamHits.Load())
	}

	// Single burst token must still be untouched after malformed requests!
	if _, err := tracepb.NewTraceServiceClient(conn).Export(ctx, sampleTraceRequest("valid-after-bad")); err != nil {
		t.Fatalf("valid export after malformed frames failed (rate limit leaked?): %v", err)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstreamHits=%d want 1", upstreamHits.Load())
	}

	var buf bytes.Buffer
	if err := metricsReg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}

	rejected := map[string]float64{}
	accepted := map[string]float64{}
	for _, m := range families["lazy_observe_ingest_requests_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["protocol"] != "grpc" {
			continue
		}
		switch lbls["outcome"] {
		case "rejected":
			rejected[lbls["signal"]] = m.GetCounter().GetValue()
		case "accepted":
			accepted[lbls["signal"]] = m.GetCounter().GetValue()
		}
	}
	wantRejected := float64(len(badFrames))
	if rejected["traces"] != wantRejected || rejected["metrics"] != wantRejected || rejected["logs"] != wantRejected {
		t.Fatalf("rejected counts=%v want %v per signal", rejected, wantRejected)
	}
	if accepted["traces"] != 1 {
		t.Fatalf("accepted traces=%v want 1", accepted["traces"])
	}
}

func TestOTLPGRPCRawPassthroughExactBytesGzipAndUnknownService(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
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
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_raw": {ID: 12, Slug: "raw"}}}
	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:       auth,
		Storage:    backend,
		HTTPClient: upstream.Client(),
	})

	// Place unknown field 99 BEFORE field 1. Re-marshaling with proto.Marshal would
	// reorder fields by number, whereas raw passthrough preserves exact wire bytes.
	baseReq, err := proto.Marshal(sampleTraceRequest(strings.Repeat("raw-span-", 300)))
	if err != nil {
		t.Fatal(err)
	}
	wireWithLeadingUnknown := protowire.AppendTag(nil, 99, protowire.BytesType)
	wireWithLeadingUnknown = protowire.AppendString(wireWithLeadingUnknown, "passthrough-order-sentinel")
	wireWithLeadingUnknown = append(wireWithLeadingUnknown, baseReq...)

	ctx := withAuthHeader(context.Background(), "Bearer lo_raw")
	rawCodecOpt := grpc.ForceCodecV2(newRawClientCodec())

	var resp tracepb.ExportTraceServiceResponse
	if err := conn.Invoke(
		ctx,
		tracepb.TraceService_Export_FullMethodName,
		rawWireBytes(wireWithLeadingUnknown),
		&resp,
		rawCodecOpt,
	); err != nil {
		t.Fatalf("raw invoke failed: %v", err)
	}

	// Also exercise gzip + pooled buffer (>1024B) passthrough.
	if err := conn.Invoke(
		ctx,
		tracepb.TraceService_Export_FullMethodName,
		rawWireBytes(wireWithLeadingUnknown),
		&resp,
		rawCodecOpt,
		grpc.UseCompressor(gzip.Name),
	); err != nil {
		t.Fatalf("gzip raw invoke failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d want 2", len(bodies))
	}
	for i, got := range bodies {
		if !bytes.Equal(got, wireWithLeadingUnknown) {
			t.Fatalf("body[%d] not byte-for-byte identical: got len=%d want len=%d",
				i, len(got), len(wireWithLeadingUnknown))
		}
	}

	// Unregistered service must return Unimplemented (no UnknownServiceHandler).
	err = conn.Invoke(
		ctx,
		"/opentelemetry.proto.collector.custom.v1.CustomService/Export",
		rawWireBytes(wireWithLeadingUnknown),
		&resp,
		rawCodecOpt,
	)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unknown service code=%s want=Unimplemented (err=%v)", status.Code(err), err)
	}
}

func TestOTLPGRPCPlaintextHTTP200ResponseCompatibility(t *testing.T) {
	var mu sync.Mutex
	var upstreamRespBody []byte
	var upstreamContentType string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		body := append([]byte(nil), upstreamRespBody...)
		ct := upstreamContentType
		mu.Unlock()
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
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
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_resp": {ID: 15, Slug: "resp"}}}
	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:       auth,
		Storage:    backend,
		HTTPClient: upstream.Client(),
	})

	ctx := withAuthHeader(context.Background(), "Bearer lo_resp")
	traceClient := tracepb.NewTraceServiceClient(conn)
	metricsClient := metricspb.NewMetricsServiceClient(conn)
	logsClient := logspb.NewLogsServiceClient(conn)

	// 1. Plaintext HTTP 200 ("OK\n") and non-protobuf binary must succeed cleanly.
	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "plaintext OK", contentType: "text/plain; charset=utf-8", body: []byte("OK\n")},
		{name: "invalid protobuf response", contentType: "application/x-protobuf", body: []byte{0x0a, 0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			upstreamContentType = tc.contentType
			upstreamRespBody = tc.body
			mu.Unlock()

			trResp, err := traceClient.Export(ctx, sampleTraceRequest("plain-200"))
			if err != nil {
				t.Fatalf("TraceService.Export failed: %v", err)
			}
			if trResp.GetPartialSuccess() != nil {
				t.Fatalf("unexpected trace PartialSuccess: %+v", trResp.GetPartialSuccess())
			}

			mResp, err := metricsClient.Export(ctx, sampleMetricsRequest("plain-200"))
			if err != nil {
				t.Fatalf("MetricsService.Export failed: %v", err)
			}
			if mResp.GetPartialSuccess() != nil {
				t.Fatalf("unexpected metrics PartialSuccess: %+v", mResp.GetPartialSuccess())
			}

			lResp, err := logsClient.Export(ctx, sampleLogsRequest("plain-200"))
			if err != nil {
				t.Fatalf("LogsService.Export failed: %v", err)
			}
			if lResp.GetPartialSuccess() != nil {
				t.Fatalf("unexpected logs PartialSuccess: %+v", lResp.GetPartialSuccess())
			}
		})
	}

	// 2. Valid protobuf PartialSuccess responses are preserved across all three signals.
	t.Run("protobuf PartialSuccess preserved", func(t *testing.T) {
		mu.Lock()
		upstreamContentType = "application/x-protobuf"
		upstreamRespBody, _ = proto.Marshal(&tracepb.ExportTraceServiceResponse{
			PartialSuccess: &tracepb.ExportTracePartialSuccess{
				RejectedSpans: 2,
				ErrorMessage:  "partial trace",
			},
		})
		mu.Unlock()
		trResp, err := traceClient.Export(ctx, sampleTraceRequest("ps-trace"))
		if err != nil {
			t.Fatalf("TraceService.Export failed: %v", err)
		}
		if trResp.GetPartialSuccess().GetRejectedSpans() != 2 ||
			trResp.GetPartialSuccess().GetErrorMessage() != "partial trace" {
			t.Fatalf("unexpected trace PartialSuccess: %+v", trResp.GetPartialSuccess())
		}

		mu.Lock()
		upstreamRespBody, _ = proto.Marshal(&metricspb.ExportMetricsServiceResponse{
			PartialSuccess: &metricspb.ExportMetricsPartialSuccess{
				RejectedDataPoints: 5,
				ErrorMessage:       "partial metric",
			},
		})
		mu.Unlock()
		mResp, err := metricsClient.Export(ctx, sampleMetricsRequest("ps-metric"))
		if err != nil {
			t.Fatalf("MetricsService.Export failed: %v", err)
		}
		if mResp.GetPartialSuccess().GetRejectedDataPoints() != 5 ||
			mResp.GetPartialSuccess().GetErrorMessage() != "partial metric" {
			t.Fatalf("unexpected metrics PartialSuccess: %+v", mResp.GetPartialSuccess())
		}

		mu.Lock()
		upstreamRespBody, _ = proto.Marshal(&logspb.ExportLogsServiceResponse{
			PartialSuccess: &logspb.ExportLogsPartialSuccess{
				RejectedLogRecords: 4,
				ErrorMessage:       "partial log",
			},
		})
		mu.Unlock()
		lResp, err := logsClient.Export(ctx, sampleLogsRequest("ps-log"))
		if err != nil {
			t.Fatalf("LogsService.Export failed: %v", err)
		}
		if lResp.GetPartialSuccess().GetRejectedLogRecords() != 4 ||
			lResp.GetPartialSuccess().GetErrorMessage() != "partial log" {
			t.Fatalf("unexpected logs PartialSuccess: %+v", lResp.GetPartialSuccess())
		}
	})
}
