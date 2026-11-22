package gateway

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// decodedContentEncodingLocal records the Content-Encoding that
// boundedRequestDecoding removed, for handlers whose contract depends on it.
const decodedContentEncodingLocal = "lazy-observe.decoded-content-encoding"

// decodedBodyLocal stores the already-owned byte slice produced by
// boundedRequestDecoding so requestBody avoids copying a decoded payload a
// second time under Immutable mode.
const decodedBodyLocal = "lazy-observe.decoded-body"

// decodedBodySizeLocal records the decoded plaintext payload byte count so
// ingest rate limiting meters compressed and passthrough requests consistently.
const decodedBodySizeLocal = "lazy-observe.decoded-body-size"

// decodedBufLocal holds the pooled decode buffer backing decodedBodyLocal so
// handlers or upstream proxies can retain it until their async body reader closes.
const decodedBufLocal = "lazy-observe.decoded-buf"

// replayRawGzipPassthroughLocal marks that boundedRequestDecoding verified the
// original single-member gzip request body for POST /api/replay/segments so
// replayIngest can store the raw gzip bytes without re-compressing.
const replayRawGzipPassthroughLocal = "lazy-observe.replay-raw-gzip-passthrough"

// maxContentEncodingLayers bounds stacked HTTP Content-Encoding transformations
// so crafted multi-layer headers cannot amplify CPU work before auth.
const maxContentEncodingLayers = 4

// maxPooledDecodeBufCap caps the capacity of decode buffers returned to the pool
// so rare large bursts (up to 52 MiB) do not stay pinned across idle periods.
const maxPooledDecodeBufCap = 1 << 20

var errDecodedBodyTooLarge = errors.New("decoded request body exceeds limit")

type pooledGzipDecoder struct {
	br      bytes.Reader
	zr      *gzip.Reader
	scratch [32 * 1024]byte
}

var gzipDecoderPool = sync.Pool{
	New: func() any {
		return &pooledGzipDecoder{}
	},
}

func acquireGzipDecoder(body []byte) (*pooledGzipDecoder, error) {
	dec := gzipDecoderPool.Get().(*pooledGzipDecoder)
	dec.br.Reset(body)
	var err error
	if dec.zr == nil {
		dec.zr, err = gzip.NewReader(&dec.br)
		if err != nil {
			dec.br.Reset(nil)
			return nil, err
		}
		return dec, nil
	}
	if err = dec.zr.Reset(&dec.br); err != nil {
		dec.br.Reset(nil)
		gzipDecoderPool.Put(dec)
		return nil, err
	}
	return dec, nil
}

func releaseGzipDecoder(dec *pooledGzipDecoder) {
	if dec == nil {
		return
	}
	if dec.zr != nil {
		_ = dec.zr.Close()
	}
	dec.br.Reset(nil)
	if dec.zr != nil {
		gzipDecoderPool.Put(dec)
	}
}

type pooledDecodeBuf struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
	refs     atomic.Int32
}

var decodeBufferPool = sync.Pool{
	New: func() any {
		return &pooledDecodeBuf{}
	},
}

func acquireDecodeBuf(limit, hint int) *pooledDecodeBuf {
	pb := decodeBufferPool.Get().(*pooledDecodeBuf)
	pb.buf.Reset()
	pb.limit = limit
	pb.exceeded = false
	pb.refs.Store(1)
	if pb.buf.Cap() == 0 && hint > 0 {
		growCap := min(hint, limit, maxPooledDecodeBufCap)
		if growCap > 0 {
			pb.buf.Grow(growCap)
		}
	}
	return pb
}

func (b *pooledDecodeBuf) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.exceeded = true
		return 0, errDecodedBodyTooLarge
	}
	return b.buf.Write(p)
}

func (b *pooledDecodeBuf) retain() {
	b.refs.Add(1)
}

func (b *pooledDecodeBuf) release() {
	if b == nil {
		return
	}
	if b.refs.Add(-1) != 0 {
		return
	}
	b.buf.Reset()
	b.limit = 0
	b.exceeded = false
	if b.buf.Cap() <= maxPooledDecodeBufCap {
		decodeBufferPool.Put(b)
	}
}

var _ io.Writer = (*pooledDecodeBuf)(nil)

type pooledBodyReader struct {
	r      bytes.Reader
	buf    *pooledDecodeBuf
	closed atomic.Bool
}

func (r *pooledBodyReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *pooledBodyReader) Close() error {
	if r.closed.CompareAndSwap(false, true) {
		r.buf.release()
	}
	return nil
}

var _ io.ReadCloser = (*pooledBodyReader)(nil)

// gzipPassthroughPolicy decides per OTLP HTTP ingest signal whether a single
// gzip layer can be validated streaming-only and forwarded compressed to a
// remote Victoria upstream instead of materializing the full plaintext payload.
type gzipPassthroughPolicy struct {
	metrics bool
	logs    bool
	traces  bool
}

func newGzipPassthroughPolicy(storage storagepkg.SignalBackend, explicit *bool) gzipPassthroughPolicy {
	if storage == nil {
		return gzipPassthroughPolicy{}
	}
	allow := func(upstreamBase string) bool {
		if strings.TrimSpace(upstreamBase) == "" {
			return false
		}
		if explicit != nil {
			return *explicit
		}
		return !isLoopbackUpstreamURL(upstreamBase)
	}
	return gzipPassthroughPolicy{
		metrics: storage.MetricsMode() == storagepkg.MetricsVM &&
			storage.MetricsIngestEnabled() &&
			allow(storage.MetricsIngestBase()),
		logs:   allow(storage.LogsIngestBase()),
		traces: allow(storage.TracesIngestBase()),
	}
}

func (p gzipPassthroughPolicy) allowsRequest(c fiber.Ctx) bool {
	if c.Method() != fiber.MethodPost {
		return false
	}
	switch c.Path() {
	case "/v1/metrics":
		if !p.metrics {
			return false
		}
		// Form-encoded bodies must still be decoded so proxy.unary strips reserved
		// isolation parameters from form fields before forwarding upstream.
		return normalizedMediaType(string(c.Request().Header.ContentType())) != fiber.MIMEApplicationForm
	case "/v1/logs":
		return p.logs
	case "/v1/traces":
		return p.traces
	default:
		return false
	}
}

func isLoopbackUpstreamURL(rawURL string) bool {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return true
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
		return ip.Unmap().IsLoopback()
	}
	return false
}

func isReplaySegmentIngest(c fiber.Ctx) bool {
	return c.Method() == fiber.MethodPost && c.Path() == "/api/replay/segments"
}

// boundedRequestDecoding removes HTTP content codings before any handler runs.
// Fiber's Ctx.Body, BodyParser, and fasthttp's MultipartForm otherwise
// decompress gzip, deflate, brotli, and zstd without a size limit, so a small
// compressed request could claim gigabytes of memory on any route, including
// unauthenticated ones. After this middleware the request carries the decoded
// body and no Content-Encoding header, so proxied requests also stay
// self-consistent. Only Prometheus remote-write (/api/v1/write) is exempted for
// a single snappy layer, whose handler enforces its own bounded snappy decode.
func boundedRequestDecoding(c fiber.Ctx) error {
	return boundedRequestDecodingWithPolicy(gzipPassthroughPolicy{})(c)
}

func boundedRequestDecodingWithPolicy(policy gzipPassthroughPolicy) fiber.Handler {
	return func(c fiber.Ctx) error {
		rawHeaders := c.Request().Header.PeekAll(fiber.HeaderContentEncoding)
		if len(rawHeaders) == 0 {
			return c.Next()
		}
		codings := make([]string, 0, 2)
		known := false
		unknown := false
		for _, rawHeader := range rawHeaders {
			for _, raw := range strings.Split(string(rawHeader), ",") {
				coding := strings.ToLower(strings.TrimSpace(raw))
				switch coding {
				case "", "identity":
					continue
				case "gzip", "x-gzip", "deflate", "br", "zstd":
					known = true
				default:
					unknown = true
				}
				codings = append(codings, coding)
				if len(codings) > maxContentEncodingLayers {
					return unsupportedContentEncoding(c)
				}
			}
		}
		if len(codings) == 0 {
			return c.Next()
		}
		if !known {
			if len(codings) == 1 && codings[0] == "snappy" && c.Path() == "/api/v1/write" {
				return c.Next()
			}
			return unsupportedContentEncoding(c)
		}
		if unknown {
			return unsupportedContentEncoding(c)
		}
		body := c.Request().Body()

		// For single-layer gzip on OTLP HTTP endpoints targeting a remote Victoria
		// upstream, validate gzip integrity and the decoded byte limit streaming
		// to io.Discard, bill decoded bytes for rate limiting, and forward the
		// original gzip body as-is.
		if len(codings) == 1 && (codings[0] == "gzip" || codings[0] == "x-gzip") && policy.allowsRequest(c) {
			decodedSize, err := validateGzipBounded(body, maxDecodedRequestBody)
			if errors.Is(err, errDecodedBodyTooLarge) {
				return c.Status(fiber.StatusRequestEntityTooLarge).
					JSON(fiber.Map{"error": "decoded request body exceeds limit"})
			}
			if err != nil {
				return c.Status(fiber.StatusBadRequest).
					JSON(fiber.Map{"error": "invalid " + codings[0] + " request body"})
			}
			c.Request().Header.Set(fiber.HeaderContentEncoding, "gzip")
			c.Locals(decodedBodySizeLocal, decodedSize)
			return c.Next()
		}

		// For single-layer gzip on POST /api/replay/segments, decode once while
		// checking via Multistream(false) whether the payload is a clean single
		// gzip member with zero trailing bytes. Keep the original compressed body
		// intact on c.Request() only when passthrough conditions hold; calling
		// SetBodyRaw would reset and recycle fasthttp's pooled request buffer.
		if len(codings) == 1 && (codings[0] == "gzip" || codings[0] == "x-gzip") && isReplaySegmentIngest(c) {
			pb, decoded, singleMember, err := decodeGzipSinglePass(body, maxReplaySegment)
			if errors.Is(err, errDecodedBodyTooLarge) {
				return c.Status(fiber.StatusRequestEntityTooLarge).
					JSON(fiber.Map{"error": "decoded request body exceeds limit"})
			}
			if err != nil {
				return c.Status(fiber.StatusBadRequest).
					JSON(fiber.Map{"error": "invalid " + codings[0] + " request body"})
			}
			passthrough := singleMember && len(body) < len(decoded) && len(body) <= maxReplaySegment
			if passthrough {
				c.Locals(replayRawGzipPassthroughLocal, true)
			} else {
				c.Request().SetBodyRaw(decoded)
			}
			c.Request().Header.Del(fiber.HeaderContentEncoding)
			c.Locals(decodedContentEncodingLocal, codings[0])
			c.Locals(decodedBodyLocal, decoded)
			c.Locals(decodedBodySizeLocal, int64(len(decoded)))
			c.Locals(decodedBufLocal, pb)
			defer func() {
				if !passthrough {
					c.Request().SetBodyRaw(nil)
				}
				c.Locals(decodedBodyLocal, nil)
				c.Locals(decodedBufLocal, nil)
				c.Locals(replayRawGzipPassthroughLocal, nil)
				pb.release()
			}()
			return c.Next()
		}

		var curBuf *pooledDecodeBuf
		releaseCurBuf := func() {
			if curBuf != nil {
				curBuf.release()
				curBuf = nil
			}
		}
		// Codings are listed in the order they were applied; undo them in reverse.
		for i := len(codings) - 1; i >= 0; i-- {
			nextBuf, decoded, err := decodeBoundedPooled(codings[i], body, maxDecodedRequestBody)
			if errors.Is(err, errDecodedBodyTooLarge) {
				releaseCurBuf()
				return c.Status(fiber.StatusRequestEntityTooLarge).
					JSON(fiber.Map{"error": "decoded request body exceeds limit"})
			}
			if err != nil {
				releaseCurBuf()
				return c.Status(fiber.StatusBadRequest).
					JSON(fiber.Map{"error": "invalid " + codings[i] + " request body"})
			}
			releaseCurBuf()
			curBuf = nextBuf
			body = decoded
		}
		c.Request().SetBodyRaw(body)
		c.Request().Header.Del(fiber.HeaderContentEncoding)
		c.Locals(decodedContentEncodingLocal, strings.Join(codings, ", "))
		c.Locals(decodedBodyLocal, body)
		c.Locals(decodedBodySizeLocal, int64(len(body)))
		if curBuf != nil {
			c.Locals(decodedBufLocal, curBuf)
			defer func() {
				c.Request().SetBodyRaw(nil)
				c.Locals(decodedBodyLocal, nil)
				c.Locals(decodedBufLocal, nil)
				curBuf.release()
			}()
		}
		return c.Next()
	}
}

func unsupportedContentEncoding(c fiber.Ctx) error {
	return c.Status(fiber.StatusUnsupportedMediaType).
		JSON(fiber.Map{"error": "unsupported Content-Encoding"})
}

func validateGzipBounded(body []byte, limit int) (int64, error) {
	dec, err := acquireGzipDecoder(body)
	if err != nil {
		return 0, err
	}
	defer releaseGzipDecoder(dec)

	n, copyErr := io.Copy(io.Discard, io.LimitReader(dec.zr, int64(limit)+1))
	if n > int64(limit) {
		return 0, errDecodedBodyTooLarge
	}
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr := dec.zr.Close(); closeErr != nil {
		return 0, closeErr
	}
	return n, nil
}

func decodeGzipSinglePass(body []byte, limit int) (*pooledDecodeBuf, []byte, bool, error) {
	dec, err := acquireGzipDecoder(body)
	if err != nil {
		return nil, nil, false, err
	}
	defer releaseGzipDecoder(dec)

	dec.zr.Multistream(false)

	hint := len(body)
	if hint < limit/2 {
		hint *= 2
	} else {
		hint = limit
	}

	pb := acquireDecodeBuf(limit, hint)
	_, copyErr := io.CopyBuffer(pb, dec.zr, dec.scratch[:])
	if pb.exceeded {
		pb.release()
		return nil, nil, false, errDecodedBodyTooLarge
	}
	if copyErr != nil {
		pb.release()
		return nil, nil, false, copyErr
	}
	if closeErr := dec.zr.Close(); closeErr != nil {
		pb.release()
		return nil, nil, false, closeErr
	}

	if dec.br.Len() == 0 {
		return pb, pb.buf.Bytes(), true, nil
	}

	// Trailing bytes exist after the first gzip member: re-enable multistream to
	// either reject trailing garbage or decode valid subsequent members into pb.
	if resetErr := dec.zr.Reset(&dec.br); resetErr != nil {
		pb.release()
		return nil, nil, false, resetErr
	}
	_, copyErr = io.CopyBuffer(pb, dec.zr, dec.scratch[:])
	if pb.exceeded {
		pb.release()
		return nil, nil, false, errDecodedBodyTooLarge
	}
	if copyErr != nil {
		pb.release()
		return nil, nil, false, copyErr
	}
	if closeErr := dec.zr.Close(); closeErr != nil {
		pb.release()
		return nil, nil, false, closeErr
	}
	return pb, pb.buf.Bytes(), false, nil
}

func decodeBoundedPooled(coding string, body []byte, limit int) (*pooledDecodeBuf, []byte, error) {
	if coding == "zstd" {
		var req fasthttp.Request
		req.SetBodyRaw(body)
		decoded, err := req.BodyUnzstdWithLimit(limit)
		if errors.Is(err, fasthttp.ErrBodyTooLarge) {
			return nil, nil, errDecodedBodyTooLarge
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, decoded, nil
	}

	hint := len(body)
	if hint < limit/2 {
		hint *= 2
	} else {
		hint = limit
	}

	switch coding {
	case "gzip", "x-gzip":
		dec, err := acquireGzipDecoder(body)
		if err != nil {
			return nil, nil, err
		}
		defer releaseGzipDecoder(dec)

		pb := acquireDecodeBuf(limit, hint)
		_, copyErr := io.CopyBuffer(pb, dec.zr, dec.scratch[:])
		if pb.exceeded {
			pb.release()
			return nil, nil, errDecodedBodyTooLarge
		}
		if copyErr != nil {
			pb.release()
			return nil, nil, copyErr
		}
		if closeErr := dec.zr.Close(); closeErr != nil {
			pb.release()
			return nil, nil, closeErr
		}
		return pb, pb.buf.Bytes(), nil
	case "deflate":
		pb := acquireDecodeBuf(limit, hint)
		_, err := fasthttp.WriteInflate(pb, body)
		if pb.exceeded {
			pb.release()
			return nil, nil, errDecodedBodyTooLarge
		}
		if err != nil {
			pb.release()
			return nil, nil, err
		}
		return pb, pb.buf.Bytes(), nil
	case "br":
		pb := acquireDecodeBuf(limit, hint)
		_, err := fasthttp.WriteUnbrotli(pb, body)
		if pb.exceeded {
			pb.release()
			return nil, nil, errDecodedBodyTooLarge
		}
		if err != nil {
			pb.release()
			return nil, nil, err
		}
		return pb, pb.buf.Bytes(), nil
	default:
		return nil, nil, errors.New("unsupported coding")
	}
}

// gunzipBounded decodes a gzip payload carried inside a request body (for
// example a pprof file part) with the same limit as HTTP content codings.
func gunzipBounded(data []byte, limit int) ([]byte, error) {
	pb, decoded, err := decodeBoundedPooled("gzip", data, limit)
	if err != nil {
		return nil, err
	}
	owned := bytes.Clone(decoded)
	pb.release()
	return owned, nil
}

// requestBody returns the request body exactly as boundedRequestDecoding left
// it. Handlers must not use Ctx.Body: Fiber v3's Body decodes Content-Encoding
// a second time, and for a coding it cannot undo (snappy on Prometheus
// remote-write) it answers 415 or 501 and returns the error text in place of
// the bytes. Reading fasthttp's raw body keeps the v2 contract. When
// boundedRequestDecoding already decoded into an owned slice, return it directly
// to avoid a second full-body copy; otherwise GetBytes detaches the slice when
// the app is Immutable because fasthttp reuses the buffer for the next request.
func requestBody(c fiber.Ctx) []byte {
	if owned, ok := c.Locals(decodedBodyLocal).([]byte); ok {
		return owned
	}
	return c.App().GetBytes(c.Request().Body())
}
