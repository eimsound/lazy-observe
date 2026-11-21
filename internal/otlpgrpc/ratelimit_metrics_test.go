package otlpgrpc_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/account"
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
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOTLPGRPCRateLimitReturnsResourceExhaustedWithRetryInfoAndUpdatesMetrics(t *testing.T) {
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

	var clockMu sync.Mutex
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	limiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             2,
		Now:               nowFn,
	})
	metricsReg := selfmetrics.New(selfmetrics.Config{
		Version:   "v0.1.0-grpc",
		Commit:    "abcdef0",
		StartedAt: now,
		Now:       nowFn,
	})
	auth := &stubAuth{
		tokens: map[string]meta.ProjectInfo{
			"lo_proj_a": {ID: 1, Slug: "proj-a"},
			"lo_proj_b": {ID: 2, Slug: "proj-b"},
		},
	}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:          auth,
		Storage:       backend,
		HTTPClient:    upstream.Client(),
		IngestLimiter: limiter,
		SelfMetrics:   metricsReg,
	})

	tracesClient := tracepb.NewTraceServiceClient(conn)
	metricsClient := metricspb.NewMetricsServiceClient(conn)
	logsClient := logspb.NewLogsServiceClient(conn)

	ctxA := withAuthHeader(context.Background(), "Bearer lo_proj_a")
	ctxB := withAuthHeader(context.Background(), "Bearer lo_proj_b")

	// Consume project A's burst (2).
	if _, err := tracesClient.Export(ctxA, sampleTraceRequest("span-1")); err != nil {
		t.Fatalf("burst 1 failed: %v", err)
	}
	if _, err := metricsClient.Export(ctxA, sampleMetricsRequest("m-1")); err != nil {
		t.Fatalf("burst 2 failed: %v", err)
	}
	hitsBeforeThrottle := upstreamHits.Load()

	// Next export for project A must be rejected with ResourceExhausted + RetryInfo before hitting upstream.
	_, err = logsClient.Export(ctxA, sampleLogsRequest("throttled"))
	if err == nil {
		t.Fatal("expected rate-limit error for project A")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("status=%v want codes.ResourceExhausted", err)
	}
	var retryDelay time.Duration
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.RetryInfo); ok && info.GetRetryDelay() != nil {
			retryDelay = info.GetRetryDelay().AsDuration()
		}
	}
	if retryDelay < time.Second {
		t.Fatalf("RetryInfo delay=%s want >= 1s", retryDelay)
	}
	if upstreamHits.Load() != hitsBeforeThrottle {
		t.Fatalf("rate-limited gRPC call reached upstream: before=%d after=%d", hitsBeforeThrottle, upstreamHits.Load())
	}

	// Project B must remain isolated and succeed immediately.
	if _, err := logsClient.Export(ctxB, sampleLogsRequest("isolated")); err != nil {
		t.Fatalf("project B failed: %v", err)
	}

	// Advance clock by 1s -> project A recovers.
	advance(time.Second)
	if _, err := logsClient.Export(ctxA, sampleLogsRequest("recovered")); err != nil {
		t.Fatalf("project A recovery failed: %v", err)
	}

	// Verify Prometheus metrics recorded gRPC outcomes and rate limits.
	var buf bytes.Buffer
	if err := metricsReg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("invalid prometheus output: %v", err)
	}
	var grpcLogsRL float64
	for _, m := range families["lazy_observe_ingest_rate_limited_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["protocol"] == "grpc" && lbls["signal"] == "logs" {
			grpcLogsRL = m.GetCounter().GetValue()
		}
	}
	if grpcLogsRL != 1 {
		t.Fatalf("grpc logs rate_limited_total=%v want 1", grpcLogsRL)
	}
}

func TestOTLPGRPCByteRateLimitReturnsResourceExhaustedWithRetryInfo(t *testing.T) {
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
	req := sampleLogsRequest(strings.Repeat("x", 200))
	limiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 100,
		Burst:             100,
		BytesPerSecond:    300,
		ByteBurst:         300,
		Now:               func() time.Time { return now },
	})
	auth := &stubAuth{
		tokens: map[string]meta.ProjectInfo{
			"lo_byte_proj": {ID: 9, Slug: "byte-proj"},
		},
	}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:          auth,
		Storage:       backend,
		HTTPClient:    upstream.Client(),
		IngestLimiter: limiter,
	})

	ctx := withAuthHeader(context.Background(), "Bearer lo_byte_proj")
	client := logspb.NewLogsServiceClient(conn)

	// First ~220B payload fits within 300B burst.
	if _, err := client.Export(ctx, req); err != nil {
		t.Fatalf("first payload failed: %v", err)
	}
	// Second ~220B payload exceeds remaining ~80B byte budget -> ResourceExhausted + RetryInfo.
	_, err = client.Export(ctx, req)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("second payload err=%v want ResourceExhausted", err)
	}
	var hasRetryInfo bool
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.RetryInfo); ok && info.GetRetryDelay().AsDuration() >= time.Second {
			hasRetryInfo = true
		}
	}
	if !hasRetryInfo {
		t.Fatalf("missing RetryInfo >= 1s on byte-rate rejection: details=%+v", st.Details())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstreamHits=%d want 1", upstreamHits.Load())
	}
}

func TestOTLPGRPCInFlightGaugeTracksActiveRequests(t *testing.T) {
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
	metricsReg := selfmetrics.New(selfmetrics.Config{})
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_ok": {ID: 1, Slug: "default"}}}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:        auth,
		Storage:     backend,
		HTTPClient:  upstream.Client(),
		SelfMetrics: metricsReg,
	})

	readInFlight := func() float64 {
		t.Helper()
		var buf bytes.Buffer
		if err := metricsReg.WritePrometheus(&buf); err != nil {
			t.Fatal(err)
		}
		parser := expfmt.NewTextParser(model.UTF8Validation)
		families, err := parser.TextToMetricFamilies(strings.NewReader(buf.String()))
		if err != nil {
			t.Fatal(err)
		}
		return families["lazy_observe_otlp_grpc_in_flight_requests"].GetMetric()[0].GetGauge().GetValue()
	}

	done := make(chan error, 1)
	go func() {
		ctx := withAuthHeader(context.Background(), "Bearer lo_ok")
		_, callErr := tracepb.NewTraceServiceClient(conn).Export(ctx, sampleTraceRequest("blocking"))
		done <- callErr
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("gRPC export did not reach upstream")
	}

	if got := readInFlight(); got != 1 {
		close(release)
		t.Fatalf("in_flight while blocked=%v want 1", got)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("blocked export failed: %v", err)
	}
	// Wait briefly for deferred DecGRPCInFlight on the server goroutine.
	deadline := time.After(time.Second)
	for readInFlight() != 0 {
		select {
		case <-deadline:
			t.Fatalf("in_flight did not return to 0, got %v", readInFlight())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type noopAccount struct {
	account.Behavior
}

type staticTokenStore struct {
	projects map[string]meta.ProjectInfo
}

func (s *staticTokenStore) AuthenticateProjectAPIToken(
	_ context.Context,
	token string,
) (meta.ProjectInfo, error) {
	if p, ok := s.projects[token]; ok {
		return p, nil
	}
	return meta.ProjectInfo{}, meta.ErrInvalidProjectAPIToken
}

func (s *staticTokenStore) Revision() uint64 { return 1 }

func TestSharedIngestLimiterAndSelfMetricsAcrossHTTPAndOTLPGRPC(t *testing.T) {
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

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sharedLimiter := ingestlimit.New(ingestlimit.Config{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Now:               func() time.Time { return now },
	})
	sharedMetrics := selfmetrics.New(selfmetrics.Config{
		Version:   "v0.1.0-shared",
		Commit:    "cafe123",
		StartedAt: now,
		Now:       func() time.Time { return now },
	})
	tokens := &staticTokenStore{
		projects: map[string]meta.ProjectInfo{
			"lo_shared": {ID: 42, Slug: "shared-proj"},
		},
	}

	gw := gateway.New(gateway.Config{
		Account:       noopAccount{},
		Tokens:        tokens,
		Storage:       backend,
		DisableWebUI:  true,
		IngestLimiter: sharedLimiter,
		SelfMetrics:   sharedMetrics,
		MetricsEndpoint: gateway.MetricsEndpointConfig{
			Enabled:              true,
			AllowUnauthenticated: true,
		},
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:          gw.IngestAuthenticator(),
		Storage:       backend,
		HTTPClient:    upstream.Client(),
		IngestLimiter: sharedLimiter,
		SelfMetrics:   sharedMetrics,
	})

	// 1. Spend the single burst token on HTTP /v1/traces.
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(`{}`))
	httpReq.Header.Set("Authorization", "Bearer lo_shared")
	httpResp, err := gw.App().Test(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTP status=%d want 204", httpResp.StatusCode)
	}

	// 2. Immediately call OTLP/gRPC with the same project token -> must be throttled by the shared bucket!
	ctx := withAuthHeader(context.Background(), "Bearer lo_shared")
	_, grpcErr := tracepb.NewTraceServiceClient(conn).Export(ctx, sampleTraceRequest("shared-limit"))
	if status.Code(grpcErr) != codes.ResourceExhausted {
		t.Fatalf("gRPC err=%v want codes.ResourceExhausted", grpcErr)
	}

	// 3. Scrape HTTP /metrics and verify BOTH protocol="http" (accepted=1) and protocol="grpc" (rate_limited=1).
	metricsResp, err := gw.App().Test(httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status=%d", metricsResp.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("invalid prometheus text: %v", err)
	}

	var httpAccepted, grpcRateLimited float64
	for _, m := range families["lazy_observe_ingest_requests_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["protocol"] == "http" && lbls["signal"] == "traces" && lbls["outcome"] == "accepted" {
			httpAccepted = m.GetCounter().GetValue()
		}
		if lbls["protocol"] == "grpc" && lbls["signal"] == "traces" && lbls["outcome"] == "rate_limited" {
			grpcRateLimited = m.GetCounter().GetValue()
		}
	}
	if httpAccepted != 1 || grpcRateLimited != 1 {
		t.Fatalf("httpAccepted=%v grpcRateLimited=%v want 1,1", httpAccepted, grpcRateLimited)
	}
}

type panickingGRPCRoundTripper struct{}

func (panickingGRPCRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	panic("simulated OTLP/gRPC handler panic")
}

func TestOTLPGRPCPanicRecordsUpstreamErrorMetric(t *testing.T) {
	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		"http://metrics.invalid",
		"http://metrics.invalid",
		"http://logs.invalid",
		"http://traces.invalid",
		&http.Client{Transport: panickingGRPCRoundTripper{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	metricsReg := selfmetrics.New(selfmetrics.Config{})
	auth := &stubAuth{tokens: map[string]meta.ProjectInfo{"lo_ok": {ID: 1, Slug: "default"}}}

	_, conn, _ := startTestServer(t, otlpgrpc.Config{
		Auth:        auth,
		Storage:     backend,
		HTTPClient:  &http.Client{Transport: panickingGRPCRoundTripper{}},
		SelfMetrics: metricsReg,
	})

	ctx := withAuthHeader(context.Background(), "Bearer lo_ok")
	_, callErr := tracepb.NewTraceServiceClient(conn).Export(ctx, sampleTraceRequest("panic-span"))
	if status.Code(callErr) != codes.Internal {
		t.Fatalf("panic gRPC err=%v want codes.Internal", callErr)
	}

	var buf bytes.Buffer
	if err := metricsReg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("parse prometheus output: %v", err)
	}

	var grpcErrCount float64
	for _, m := range families["lazy_observe_ingest_requests_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["protocol"] == "grpc" && lbls["signal"] == "traces" && lbls["outcome"] == "upstream_error" {
			grpcErrCount = m.GetCounter().GetValue()
		}
	}
	if grpcErrCount != 1 {
		t.Fatalf("grpc traces upstream_error=%v want 1", grpcErrCount)
	}
}
