package otlpgrpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eimsound/lazy-observe/internal/ingestlimit"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/selfmetrics"
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
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type trackingBufferPool struct {
	gets   atomic.Int64
	puts   atomic.Int64
	active atomic.Int64
}

func (p *trackingBufferPool) Get(length int) *[]byte {
	p.gets.Add(1)
	p.active.Add(1)
	buf := make([]byte, length)
	return &buf
}

func (p *trackingBufferPool) Put(*[]byte) {
	p.puts.Add(1)
	p.active.Add(-1)
}

func newPooledBuffer(pool *trackingBufferPool, payload []byte) mem.Buffer {
	buf := pool.Get(len(payload))
	copy(*buf, payload)
	return mem.NewBuffer(buf, pool)
}

func TestRawOrProtoCodecBufferLifecycleAndDelegation(t *testing.T) {
	t.Run("single pooled buffer retains ref past input free and returns on payload free", func(t *testing.T) {
		pool := &trackingBufferPool{}
		codec := newRawOrProtoCodec(pool)

		want := bytes.Repeat([]byte{0xab}, 4096)
		in := mem.BufferSlice{newPooledBuffer(pool, want)}
		if pool.active.Load() != 1 {
			t.Fatalf("active buffers after alloc=%d want 1", pool.active.Load())
		}

		var dst rawPayload
		if err := codec.Unmarshal(in, &dst); err != nil {
			t.Fatalf("Unmarshal(rawPayload): %v", err)
		}
		// Simulate grpc-go recv() freeing the incoming slice right after Unmarshal.
		in.Free()
		if pool.active.Load() != 1 {
			t.Fatalf("active buffers after input slice Free=%d want 1", pool.active.Load())
		}
		if !bytes.Equal(dst.Bytes(), want) {
			t.Fatalf("payload mismatch: len=%d want=%d", dst.Len(), len(want))
		}

		dst.Free()
		dst.Free() // Idempotent double-free must be safe.
		if pool.active.Load() != 0 {
			t.Fatalf("active buffers after rawPayload.Free=%d want 0", pool.active.Load())
		}
	})

	t.Run("multi-buffer slice materializes via pool and releases all buffers", func(t *testing.T) {
		pool := &trackingBufferPool{}
		codec := newRawOrProtoCodec(pool)

		part1 := bytes.Repeat([]byte{0x11}, 1500)
		part2 := bytes.Repeat([]byte{0x22}, 1500)
		want := append(append([]byte(nil), part1...), part2...)

		in := mem.BufferSlice{
			newPooledBuffer(pool, part1),
			newPooledBuffer(pool, part2),
		}
		var dst rawPayload
		if err := codec.Unmarshal(in, &dst); err != nil {
			t.Fatalf("Unmarshal(multi-buffer): %v", err)
		}
		in.Free()
		if pool.active.Load() != 1 {
			t.Fatalf("active buffers after input Free=%d want 1", pool.active.Load())
		}
		if !bytes.Equal(dst.Bytes(), want) {
			t.Fatalf("coalesced payload mismatch: len=%d want=%d", dst.Len(), len(want))
		}

		dst.Free()
		if pool.active.Load() != 0 {
			t.Fatalf("active buffers after dst.Free=%d want 0 (gets=%d puts=%d)",
				pool.active.Load(), pool.gets.Load(), pool.puts.Load())
		}
	})

	t.Run("empty slice unmarshal and repeated setBuffer do not leak", func(t *testing.T) {
		pool := &trackingBufferPool{}
		codec := newRawOrProtoCodec(pool)

		var dst rawPayload
		if err := codec.Unmarshal(nil, &dst); err != nil {
			t.Fatalf("Unmarshal(nil): %v", err)
		}
		if dst.Len() != 0 || dst.Bytes() != nil {
			t.Fatalf("empty unmarshal got len=%d bytes=%v", dst.Len(), dst.Bytes())
		}
		dst.Free()

		first := mem.BufferSlice{newPooledBuffer(pool, bytes.Repeat([]byte{0x01}, 2048))}
		if err := codec.Unmarshal(first, &dst); err != nil {
			t.Fatalf("first Unmarshal: %v", err)
		}
		first.Free()
		if pool.active.Load() != 1 {
			t.Fatalf("active after first=%d want 1", pool.active.Load())
		}

		wantSecond := bytes.Repeat([]byte{0x02}, 2048)
		second := mem.BufferSlice{newPooledBuffer(pool, wantSecond)}
		if err := codec.Unmarshal(second, &dst); err != nil {
			t.Fatalf("second Unmarshal: %v", err)
		}
		second.Free()
		if pool.active.Load() != 1 {
			t.Fatalf("active after overwrite=%d want 1 (prior buffer leaked)", pool.active.Load())
		}
		if !bytes.Equal(dst.Bytes(), wantSecond) {
			t.Fatalf("overwritten payload mismatch")
		}
		dst.Free()
		if pool.active.Load() != 0 {
			t.Fatalf("active after final Free=%d want 0", pool.active.Load())
		}
	})

	t.Run("delegates non-rawPayload types and rejects nil rawPayload pointer", func(t *testing.T) {
		codec := newRawOrProtoCodec(nil)
		if codec.Name() != "proto" {
			t.Fatalf("codec.Name()=%q want proto", codec.Name())
		}

		var nilRaw *rawPayload
		if err := codec.Unmarshal(nil, nilRaw); err == nil {
			t.Fatal("expected error for typed nil *rawPayload")
		}

		msg := &tracepb.ExportTraceServiceResponse{
			PartialSuccess: &tracepb.ExportTracePartialSuccess{
				RejectedSpans: 3,
				ErrorMessage:  "partial",
			},
		}
		out, err := codec.Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal(proto.Message): %v", err)
		}
		defer out.Free()

		var decoded tracepb.ExportTraceServiceResponse
		if err := codec.Unmarshal(out, &decoded); err != nil {
			t.Fatalf("Unmarshal(proto.Message): %v", err)
		}
		if decoded.GetPartialSuccess().GetRejectedSpans() != 3 ||
			decoded.GetPartialSuccess().GetErrorMessage() != "partial" {
			t.Fatalf("unexpected delegated round-trip response: %+v", decoded.GetPartialSuccess())
		}
	})
}

func TestRawBufferReaderLifecycleAndConcurrency(t *testing.T) {
	t.Run("chunked read to EOF and Close releases buffer ref", func(t *testing.T) {
		pool := &trackingBufferPool{}
		want := bytes.Repeat([]byte{0x5a}, 4096)
		buf := newPooledBuffer(pool, want)

		r := newRawBufferReader(buf)
		buf.Free() // Drop caller's ref; reader must retain its own ref.
		if pool.active.Load() != 1 {
			t.Fatalf("active after caller Free=%d want 1", pool.active.Load())
		}

		var got []byte
		chunk := make([]byte, 512)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				got = append(got, chunk[:n]...)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Read failed: %v", err)
			}
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("read bytes mismatch: len=%d want=%d", len(got), len(want))
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if pool.active.Load() != 0 {
			t.Fatalf("active after reader Close=%d want 0", pool.active.Load())
		}
	})

	t.Run("early Close rejects subsequent Read with ErrClosedPipe", func(t *testing.T) {
		pool := &trackingBufferPool{}
		buf := newPooledBuffer(pool, bytes.Repeat([]byte{0x7f}, 2048))
		r := newRawBufferReader(buf)
		buf.Free()

		head := make([]byte, 64)
		if n, err := r.Read(head); n != 64 || err != nil {
			t.Fatalf("initial Read=(%d, %v) want (64, nil)", n, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if pool.active.Load() != 0 {
			t.Fatalf("active after early Close=%d want 0", pool.active.Load())
		}
		if n, err := r.Read(head); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Read after Close=(%d, %v) want (0, io.ErrClosedPipe)", n, err)
		}
	})

	t.Run("empty buffer reader returns EOF before Close and ErrClosedPipe after Close", func(t *testing.T) {
		r := newRawBufferReader(nil)
		p := make([]byte, 8)
		if n, err := r.Read(p); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("empty Read=(%d, %v) want (0, io.EOF)", n, err)
		}
		_ = r.Close()
		if n, err := r.Read(p); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closed empty Read=(%d, %v) want (0, io.ErrClosedPipe)", n, err)
		}
	})

	t.Run("concurrent Read and Close is race-free", func(t *testing.T) {
		pool := &trackingBufferPool{}
		data := bytes.Repeat([]byte{0x33}, 8192)
		for range 32 {
			buf := newPooledBuffer(pool, data)
			r := newRawBufferReader(buf)
			buf.Free()

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				chunk := make([]byte, 128)
				for {
					_, err := r.Read(chunk)
					if err != nil {
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				_ = r.Close()
			}()
			wg.Wait()
		}
		if pool.active.Load() != 0 {
			t.Fatalf("active after concurrent stress=%d want 0", pool.active.Load())
		}
	})
}

func TestValidateTopLevelOTLPRequest(t *testing.T) {
	tracesPayload, err := proto.Marshal(&tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracedata.ResourceSpans{
			{ScopeSpans: []*tracedata.ScopeSpans{{Spans: []*tracedata.Span{{Name: "a"}}}}},
			{ScopeSpans: []*tracedata.ScopeSpans{{Spans: []*tracedata.Span{{Name: "b"}}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metricsPayload, err := proto.Marshal(&metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsdata.ResourceMetrics{
			{ScopeMetrics: []*metricsdata.ScopeMetrics{{Metrics: []*metricsdata.Metric{{Name: "m"}}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logsPayload, err := proto.Marshal(&logspb.ExportLogsServiceRequest{
		ResourceLogs: []*logsdata.ResourceLogs{
			{ScopeLogs: []*logsdata.ScopeLogs{{LogRecords: []*logsdata.LogRecord{{
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "l"}},
			}}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Unknown top-level field (varint field 99) before valid field 1.
	withUnknownField := protowire.AppendTag(nil, 99, protowire.VarintType)
	withUnknownField = protowire.AppendVarint(withUnknownField, 7)
	withUnknownField = append(withUnknownField, tracesPayload...)

	// O(Resource) non-recursion proof: field 1 BytesType carrying malformed inner bytes.
	nonRecursiveField1 := protowire.AppendTag(nil, 1, protowire.BytesType)
	nonRecursiveField1 = protowire.AppendBytes(nonRecursiveField1, []byte{0xff, 0xff, 0x80, 0x0f})

	validCases := []struct {
		name    string
		payload []byte
	}{
		{name: "nil payload", payload: nil},
		{name: "empty payload", payload: []byte{}},
		{name: "valid traces", payload: tracesPayload},
		{name: "valid metrics", payload: metricsPayload},
		{name: "valid logs", payload: logsPayload},
		{name: "valid with unknown top-level field", payload: withUnknownField},
		{name: "field 1 bytes not traversed recursively", payload: nonRecursiveField1},
	}
	for _, tc := range validCases {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			if err := validateTopLevelOTLPRequest(tc.payload); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	field1Varint := protowire.AppendTag(nil, 1, protowire.VarintType)
	field1Varint = protowire.AppendVarint(field1Varint, 1)

	field1Fixed32 := protowire.AppendTag(nil, 1, protowire.Fixed32Type)
	field1Fixed32 = protowire.AppendFixed32(field1Fixed32, 1)

	field1Fixed64 := protowire.AppendTag(nil, 1, protowire.Fixed64Type)
	field1Fixed64 = protowire.AppendFixed64(field1Fixed64, 1)

	field1Group := protowire.AppendTag(nil, 1, protowire.StartGroupType)
	field1Group = protowire.AppendTag(field1Group, 1, protowire.EndGroupType)

	invalidCases := []struct {
		name    string
		payload []byte
	}{
		{name: "truncated tag varint", payload: []byte{0x80}},
		{name: "truncated length-delimited field 1", payload: []byte{0x0a, 0x08, 0x01, 0x02}},
		{name: "field number zero", payload: []byte{0x00, 0x01}},
		{name: "reserved wire type 6", payload: []byte{0x0e}},
		{name: "reserved wire type 7", payload: []byte{0x0f}},
		{name: "unmatched end group", payload: []byte{0x0c}},
		{name: "field 1 varint", payload: field1Varint},
		{name: "field 1 fixed32", payload: field1Fixed32},
		{name: "field 1 fixed64", payload: field1Fixed64},
		{name: "field 1 group", payload: field1Group},
		{name: "json payload", payload: []byte(`{"resourceSpans":[]}`)},
		{name: "plaintext garbage", payload: []byte("hello otlp")},
	}
	for _, tc := range invalidCases {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			err := validateTopLevelOTLPRequest(tc.payload)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code=%s want=InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}

	// Zero-allocation guarantee across multi-Resource payloads.
	multiResource := append(append([]byte(nil), tracesPayload...), tracesPayload...)
	allocs := testing.AllocsPerRun(100, func() {
		if err := validateTopLevelOTLPRequest(multiResource); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("validateTopLevelOTLPRequest allocs=%v want 0", allocs)
	}
}

type mockExportServerStream struct {
	grpc.ServerStream
	ctx       context.Context
	recvCalls atomic.Int64
	sendCalls atomic.Int64
	recvFn    func(m any) error
	sendFn    func(m any) error
}

func (m *mockExportServerStream) Context() context.Context {
	return m.ctx
}

func (m *mockExportServerStream) RecvMsg(dst any) error {
	m.recvCalls.Add(1)
	if m.recvFn != nil {
		return m.recvFn(dst)
	}
	return nil
}

func (m *mockExportServerStream) SendMsg(src any) error {
	m.sendCalls.Add(1)
	if m.sendFn != nil {
		return m.sendFn(src)
	}
	return nil
}

type mockAuth struct {
	calls atomic.Int64
	fn    func(ctx context.Context, token string) (meta.ProjectInfo, error)
}

func (a *mockAuth) AuthenticateProjectToken(ctx context.Context, token string) (meta.ProjectInfo, error) {
	a.calls.Add(1)
	return a.fn(ctx, token)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestHandleExportStreamOrderAndDeterministicBufferCleanup(t *testing.T) {
	backend, err := storagepkg.New(
		storagepkg.MetricsVM,
		"http://metrics.invalid",
		"http://metrics.invalid",
		"http://logs.invalid",
		"http://traces.invalid",
		http.DefaultClient,
	)
	if err != nil {
		t.Fatal(err)
	}

	validTraceBytes, err := proto.Marshal(&tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracedata.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{
						Key: "service.name",
						Value: &commonpb.AnyValue{
							Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("s", 2048)},
						},
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("registered ServiceDesc uses single-request StreamDesc without UnknownServiceHandler", func(t *testing.T) {
		srv, err := NewServer(Config{
			Auth: &mockAuth{
				fn: func(context.Context, string) (meta.ProjectInfo, error) { return meta.ProjectInfo{}, nil },
			},
			Storage: backend,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		info := srv.grpcServer.GetServiceInfo()
		if len(info) != 3 {
			t.Fatalf("registered services=%d want 3: %v", len(info), info)
		}
		for _, svcName := range []string{
			"opentelemetry.proto.collector.trace.v1.TraceService",
			"opentelemetry.proto.collector.metrics.v1.MetricsService",
			"opentelemetry.proto.collector.logs.v1.LogsService",
		} {
			si, ok := info[svcName]
			if !ok {
				t.Fatalf("missing service %s", svcName)
			}
			if len(si.Methods) != 1 ||
				si.Methods[0].Name != "Export" ||
				si.Methods[0].IsClientStream ||
				si.Methods[0].IsServerStream {
				t.Fatalf("unexpected methods for %s: %+v", svcName, si.Methods)
			}
		}
	})

	t.Run("concurrency rejection stops before auth and RecvMsg", func(t *testing.T) {
		auth := &mockAuth{fn: func(context.Context, string) (meta.ProjectInfo, error) {
			return meta.ProjectInfo{ID: 1, Slug: "default"}, nil
		}}
		reg := selfmetrics.New(selfmetrics.Config{})
		srv, err := NewServer(Config{
			Auth:        auth,
			Storage:     backend,
			SelfMetrics: reg,
			MaxInFlight: 1,
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		srv.inFlightSem <- struct{}{} // Saturate concurrency limit.
		defer func() { <-srv.inFlightSem }()

		stream := &mockExportServerStream{
			ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer lo_ok")),
		}
		err = srv.handleExportStream(stream, tracesExportSpec)
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("code=%s want=ResourceExhausted (err=%v)", status.Code(err), err)
		}
		if !hasRetryInfoDetail(status.Convert(err)) {
			t.Fatalf("expected RetryInfo on concurrency rejection: %v", err)
		}
		if auth.calls.Load() != 0 {
			t.Fatalf("auth calls=%d want 0", auth.calls.Load())
		}
		if stream.recvCalls.Load() != 0 {
			t.Fatalf("RecvMsg calls=%d want 0", stream.recvCalls.Load())
		}
	})

	t.Run("auth rejection stops before RecvMsg", func(t *testing.T) {
		auth := &mockAuth{fn: func(context.Context, string) (meta.ProjectInfo, error) {
			return meta.ProjectInfo{}, meta.ErrInvalidProjectAPIToken
		}}
		srv, err := NewServer(Config{
			Auth:    auth,
			Storage: backend,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}

		stream := &mockExportServerStream{
			ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer lo_bad")),
		}
		err = srv.handleExportStream(stream, tracesExportSpec)
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("code=%s want=Unauthenticated (err=%v)", status.Code(err), err)
		}
		if stream.recvCalls.Load() != 0 {
			t.Fatalf("RecvMsg calls=%d want 0", stream.recvCalls.Load())
		}
	})

	t.Run("pooled buffer deterministically freed across all post-RecvMsg outcomes", func(t *testing.T) {
		exhaustedLimiter := ingestlimit.New(ingestlimit.Config{
			Enabled:           true,
			RequestsPerSecond: 0.001,
			Burst:             1,
		})
		_ = exhaustedLimiter.Allow(7, 0)

		outcomes := []struct {
			name     string
			payload  []byte
			limiter  *ingestlimit.Limiter
			rt       roundTripFunc
			sendErr  error
			wantCode codes.Code
		}{
			{
				name:    "success",
				payload: validTraceBytes,
				rt: func(r *http.Request) (*http.Response, error) {
					_, _ = io.ReadAll(r.Body)
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				},
				wantCode: codes.OK,
			},
			{
				name:     "malformed wire frame",
				payload:  append([]byte{0x08, 0x01}, make([]byte, 2048)...),
				wantCode: codes.InvalidArgument,
			},
			{
				name:     "rate limit exceeded",
				payload:  validTraceBytes,
				limiter:  exhaustedLimiter,
				wantCode: codes.ResourceExhausted,
			},
			{
				name:    "upstream 500",
				payload: validTraceBytes,
				rt: func(r *http.Request) (*http.Response, error) {
					_, _ = io.ReadAll(r.Body)
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Body:       io.NopCloser(strings.NewReader("boom")),
					}, nil
				},
				wantCode: codes.Unavailable,
			},
			{
				name:    "SendMsg failure",
				payload: validTraceBytes,
				rt: func(r *http.Request) (*http.Response, error) {
					_, _ = io.ReadAll(r.Body)
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				},
				sendErr:  status.Error(codes.Unavailable, "client gone"),
				wantCode: codes.Unavailable,
			},
			{
				name:    "handler panic",
				payload: validTraceBytes,
				rt: func(*http.Request) (*http.Response, error) {
					panic("simulated roundtrip panic")
				},
				wantCode: codes.Internal,
			},
		}

		for _, tc := range outcomes {
			t.Run(tc.name, func(t *testing.T) {
				pool := &trackingBufferPool{}
				client := http.DefaultClient
				if tc.rt != nil {
					client = &http.Client{Transport: tc.rt}
				}
				srv, err := NewServer(Config{
					Auth: &mockAuth{fn: func(context.Context, string) (meta.ProjectInfo, error) {
						return meta.ProjectInfo{ID: 7, Slug: "p7"}, nil
					}},
					Storage:       backend,
					HTTPClient:    client,
					IngestLimiter: tc.limiter,
					Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				if err != nil {
					t.Fatal(err)
				}

				stream := &mockExportServerStream{
					ctx: metadata.NewIncomingContext(
						context.Background(),
						metadata.Pairs("authorization", "Bearer lo_ok"),
					),
					recvFn: func(m any) error {
						raw := m.(*rawPayload)
						raw.setBuffer(newPooledBuffer(pool, tc.payload))
						return nil
					},
					sendFn: func(any) error {
						return tc.sendErr
					},
				}

				err = srv.handleExportStream(stream, tracesExportSpec)
				if status.Code(err) != tc.wantCode {
					t.Fatalf("code=%s want=%s (err=%v)", status.Code(err), tc.wantCode, err)
				}
				if pool.gets.Load() != 1 || pool.active.Load() != 0 {
					t.Fatalf("buffer leak or missing pool use: gets=%d puts=%d active=%d",
						pool.gets.Load(), pool.puts.Load(), pool.active.Load())
				}
			})
		}
	})
}
