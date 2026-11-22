// Package otlpgrpc serves standard OTLP/gRPC ingest endpoints for traces,
// metrics, and logs, authenticating callers with project API tokens and
// forwarding protobuf payloads to the configured signal storage backend with
// fail-closed tenant isolation.
package otlpgrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip decompressor for standard OTel gRPC exporters.
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

const (
	// DefaultMaxRecvMsgSize matches gateway's maxDecodedRequestBody (52 MiB) so
	// HTTP and gRPC OTLP ingest enforce the same single-request memory bound.
	DefaultMaxRecvMsgSize = 52 << 20
	// DefaultMaxConcurrentStreams bounds concurrent HTTP/2 streams per gRPC connection.
	DefaultMaxConcurrentStreams uint32 = 256
	// DefaultMaxInFlight bounds concurrent unary OTLP exports across all connections.
	DefaultMaxInFlight = 256
	// DefaultUpstreamTimeout matches gateway's default unary upstream proxy timeout.
	DefaultUpstreamTimeout = 60 * time.Second
	// DefaultDrainTimeout bounds GracefulStop before forcing an immediate Stop.
	DefaultDrainTimeout = 15 * time.Second

	defaultMaxIdleConnections        = 256
	defaultMaxIdleConnectionsPerHost = 128
	defaultResponseHeaderTimeout     = 60 * time.Second
)

// TokenAuthenticator resolves a project API ingest token to its project.
// *gateway.Authenticator satisfies this interface directly.
type TokenAuthenticator interface {
	AuthenticateProjectToken(ctx context.Context, token string) (meta.ProjectInfo, error)
}

// Config configures an OTLP/gRPC ingest server.
type Config struct {
	Auth                 TokenAuthenticator
	Storage              storagepkg.SignalBackend
	HTTPClient           *http.Client
	TLSConfig            *tls.Config
	IngestLimiter        *ingestlimit.Limiter
	SelfMetrics          *selfmetrics.Registry
	MaxRecvMsgSize       int
	MaxConcurrentStreams uint32
	MaxInFlight          int
	UpstreamTimeout      time.Duration
	DrainTimeout         time.Duration
	Logger               *slog.Logger
}

// Server hosts the OTLP TraceService, MetricsService, and LogsService over gRPC.
type Server struct {
	grpcServer      *grpc.Server
	auth            TokenAuthenticator
	storage         storagepkg.SignalBackend
	httpClient      *http.Client
	ingestLimiter   *ingestlimit.Limiter
	selfMetrics     *selfmetrics.Registry
	maxRecvMsgSize  int
	upstreamTimeout time.Duration
	drainTimeout    time.Duration
	inFlightSem     chan struct{}
	logger          *slog.Logger
	shutdownCtx     context.Context
	shutdownCancel  context.CancelFunc

	stopOnce sync.Once
	stopErr  error
}

// NewServer constructs a configured OTLP/gRPC server without binding a socket.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Auth == nil {
		return nil, errors.New("otlpgrpc: token authenticator is required")
	}
	if cfg.Storage == nil {
		return nil, errors.New("otlpgrpc: signal storage backend is required")
	}
	if cfg.MaxRecvMsgSize <= 0 {
		cfg.MaxRecvMsgSize = DefaultMaxRecvMsgSize
	}
	if cfg.MaxConcurrentStreams == 0 {
		cfg.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = DefaultMaxInFlight
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = DefaultUpstreamTimeout
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	httpClient := buildUpstreamHTTPClient(cfg.HTTPClient)
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	s := &Server{
		auth:            cfg.Auth,
		storage:         cfg.Storage,
		httpClient:      httpClient,
		ingestLimiter:   cfg.IngestLimiter,
		selfMetrics:     cfg.SelfMetrics,
		maxRecvMsgSize:  cfg.MaxRecvMsgSize,
		upstreamTimeout: cfg.UpstreamTimeout,
		drainTimeout:    cfg.DrainTimeout,
		inFlightSem:     make(chan struct{}, cfg.MaxInFlight),
		logger:          cfg.Logger,
		shutdownCtx:     shutdownCtx,
		shutdownCancel:  shutdownCancel,
	}

	opts := []grpc.ServerOption{
		grpc.ForceServerCodecV2(newRawOrProtoCodec(mem.DefaultBufferPool())),
		grpc.MaxRecvMsgSize(cfg.MaxRecvMsgSize),
		grpc.MaxConcurrentStreams(cfg.MaxConcurrentStreams),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              2 * time.Minute,
			Timeout:           20 * time.Second,
		}),
	}
	if cfg.TLSConfig != nil {
		tlsCfg := cfg.TLSConfig.Clone()
		if tlsCfg.MinVersion == 0 {
			tlsCfg.MinVersion = tls.VersionTLS12
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	s.grpcServer = grpc.NewServer(opts...)
	s.grpcServer.RegisterService(&traceRawServiceDesc, s)
	s.grpcServer.RegisterService(&metricsRawServiceDesc, s)
	s.grpcServer.RegisterService(&logsRawServiceDesc, s)
	return s, nil
}

// Serve accepts incoming gRPC connections on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	if ln == nil {
		return errors.New("otlpgrpc: listener is required")
	}
	err := s.grpcServer.Serve(ln)
	if err == nil || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Shutdown gracefully stops accepting new RPCs and waits for active RPCs to
// finish until ctx expires, then falls back to an immediate Stop. It is safe
// and idempotent to call multiple times.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		shutdownCtx := ctx
		if shutdownCtx == nil {
			var cancel context.CancelFunc
			shutdownCtx, cancel = context.WithTimeout(context.Background(), s.drainTimeout)
			defer cancel()
		}
		done := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
			s.shutdownCancel()
			s.stopErr = nil
		case <-shutdownCtx.Done():
			s.logger.Warn("otlp grpc graceful stop timed out; forcing immediate stop", "error", shutdownCtx.Err())
			s.shutdownCancel()
			s.grpcServer.Stop()
			<-done
			s.stopErr = shutdownCtx.Err()
		}
	})
	return s.stopErr
}

// handleExportStream enforces the required per-RPC pipeline on a single-request
// custom StreamDesc: metrics -> recover -> concurrency -> auth -> RecvMsg ->
// top-level wire validation -> rate limit -> upstream forward -> SendMsg.
func (s *Server) handleExportStream(stream grpc.ServerStream, spec exportSignalSpec) (err error) {
	// 1. metrics (registered first so it observes panic recovery and RecvMsg errors).
	defer func() {
		if s.selfMetrics != nil {
			s.selfMetrics.ObserveIngest(selfmetrics.ProtocolGRPC, spec.metricSignal, classifyGRPCOutcome(err))
		}
	}()

	// 2. recover (registered second so panics become codes.Internal before metrics records).
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(
				"otlp grpc handler panic",
				"method", spec.fullMethod,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			err = status.Error(codes.Internal, "internal server error")
		}
	}()

	// 3. concurrency limit (before auth and RecvMsg).
	if s.inFlightSem != nil {
		select {
		case s.inFlightSem <- struct{}{}:
			if s.selfMetrics != nil {
				s.selfMetrics.IncGRPCInFlight()
			}
			defer func() {
				if s.selfMetrics != nil {
					s.selfMetrics.DecGRPCInFlight()
				}
				<-s.inFlightSem
			}()
		default:
			return throttledStatusError("too many concurrent OTLP gRPC requests", defaultRetryDelay)
		}
	}

	// 4. auth (before RecvMsg so unauthenticated calls do not decompress/buffer payloads).
	ctx := stream.Context()
	project, authErr := authenticateContext(ctx, s.auth, s.logger)
	if authErr != nil {
		return authErr
	}

	// 5. RecvMsg into rawPayload and deterministically release pooled memory.
	var rawReq rawPayload
	defer rawReq.Free()
	if recvErr := stream.RecvMsg(&rawReq); recvErr != nil {
		return recvErr
	}

	// 6. top-level wire check -> rate limit -> forwardRawProtobuf -> SendMsg.
	if valErr := validateTopLevelOTLPRequest(rawReq.Bytes()); valErr != nil {
		return valErr
	}
	if rlErr := s.checkIngestRateLimit(project.ID, int64(rawReq.Len())); rlErr != nil {
		return rlErr
	}
	targetURL, applyTenantHeaders, targetErr := spec.resolveTarget(s, project)
	if targetErr != nil {
		return targetErr
	}
	resp := spec.newResponse()
	if fwdErr := s.forwardRawProtobuf(
		ctx,
		spec.name,
		targetURL,
		project,
		applyTenantHeaders,
		&rawReq,
		resp,
	); fwdErr != nil {
		return fwdErr
	}
	if sendErr := stream.SendMsg(resp); sendErr != nil {
		return sendErr
	}
	return nil
}

func (s *Server) checkIngestRateLimit(projectID int, payloadBytes int64) error {
	if s.ingestLimiter == nil || !s.ingestLimiter.Enabled() {
		return nil
	}
	if projectID <= 0 {
		return status.Error(codes.Internal, "project context is missing")
	}
	decision := s.ingestLimiter.Allow(projectID, payloadBytes)
	if !decision.Allowed {
		return throttledStatusError("project ingest rate limit exceeded", decision.RetryAfter)
	}
	return nil
}

func classifyGRPCOutcome(err error) selfmetrics.Outcome {
	if err == nil {
		return selfmetrics.OutcomeAccepted
	}
	st, ok := status.FromError(err)
	if !ok {
		return selfmetrics.OutcomeUpstreamError
	}
	switch st.Code() {
	case codes.OK:
		return selfmetrics.OutcomeAccepted
	case codes.ResourceExhausted:
		if hasRetryInfoDetail(st) {
			return selfmetrics.OutcomeRateLimited
		}
		return selfmetrics.OutcomeRejected
	case codes.InvalidArgument,
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.Unimplemented,
		codes.Canceled,
		codes.FailedPrecondition,
		codes.NotFound,
		codes.AlreadyExists:
		return selfmetrics.OutcomeRejected
	default:
		return selfmetrics.OutcomeUpstreamError
	}
}

func buildUpstreamHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = defaultMaxIdleConnections
		transport.MaxIdleConnsPerHost = defaultMaxIdleConnectionsPerHost
		transport.ResponseHeaderTimeout = defaultResponseHeaderTimeout
		return &http.Client{
			Transport:     transport,
			CheckRedirect: noFollowRedirects,
		}
	}
	clientCopy := *base
	if clientCopy.CheckRedirect == nil {
		clientCopy.CheckRedirect = noFollowRedirects
	}
	return &clientCopy
}

func noFollowRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
