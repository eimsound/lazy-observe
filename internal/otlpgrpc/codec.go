package otlpgrpc

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/grpc/encoding"
	grpcproto "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/mem"
)

type rawPayload struct {
	buf mem.Buffer
}

func (p *rawPayload) Bytes() []byte {
	if p == nil || p.buf == nil {
		return nil
	}
	return p.buf.ReadOnlyData()
}

func (p *rawPayload) Len() int {
	if p == nil || p.buf == nil {
		return 0
	}
	return p.buf.Len()
}

func (p *rawPayload) Free() {
	if p == nil || p.buf == nil {
		return
	}
	buf := p.buf
	p.buf = nil
	buf.Free()
}

func (p *rawPayload) setBuffer(buf mem.Buffer) {
	if p.buf != nil {
		prev := p.buf
		p.buf = nil
		prev.Free()
	}
	p.buf = buf
}

type rawOrProtoCodec struct {
	protoCodec encoding.CodecV2
	pool       mem.BufferPool
}

func newRawOrProtoCodec(pool mem.BufferPool) *rawOrProtoCodec {
	if pool == nil {
		pool = mem.DefaultBufferPool()
	}
	protoCodec := encoding.GetCodecV2(grpcproto.Name)
	if protoCodec == nil {
		panic(fmt.Sprintf("otlpgrpc: codec %q is not registered", grpcproto.Name))
	}
	return &rawOrProtoCodec{
		protoCodec: protoCodec,
		pool:       pool,
	}
}

func (c *rawOrProtoCodec) Marshal(v any) (mem.BufferSlice, error) {
	return c.protoCodec.Marshal(v)
}

func (c *rawOrProtoCodec) Unmarshal(data mem.BufferSlice, v any) error {
	if raw, ok := v.(*rawPayload); ok {
		if raw == nil {
			return errors.New("otlpgrpc: nil rawPayload destination")
		}
		raw.setBuffer(data.MaterializeToBuffer(c.pool))
		return nil
	}
	return c.protoCodec.Unmarshal(data, v)
}

func (c *rawOrProtoCodec) Name() string {
	return grpcproto.Name
}

// rawBufferReader exposes a reference-counted mem.Buffer as an io.ReadCloser
// for http.Client. If an upstream HTTP timeout cancels RoundTrip while
// net/http's persistConn.writeLoop is still reading the request body, mu and
// a dedicated buf.Ref() guarantee the backing slice is never freed mid-read.
type rawBufferReader struct {
	mu     sync.Mutex
	buf    mem.Buffer
	data   []byte
	offset int
	closed bool
}

func newRawBufferReader(buf mem.Buffer) *rawBufferReader {
	if buf == nil || buf.Len() == 0 {
		return &rawBufferReader{}
	}
	buf.Ref()
	return &rawBufferReader{
		buf:  buf,
		data: buf.ReadOnlyData(),
	}
}

func (r *rawBufferReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	if r.buf == nil {
		return 0, io.ErrClosedPipe
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	if r.offset >= len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

func (r *rawBufferReader) Close() error {
	r.mu.Lock()
	buf := r.buf
	r.buf = nil
	r.data = nil
	r.closed = true
	r.mu.Unlock()

	if buf != nil {
		buf.Free()
	}
	return nil
}
