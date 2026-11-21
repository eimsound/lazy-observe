package gateway

import (
	"bytes"
	"errors"
	"io"
	"strings"

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

// maxContentEncodingLayers bounds stacked HTTP Content-Encoding transformations
// so crafted multi-layer headers cannot amplify CPU work before auth.
const maxContentEncodingLayers = 4

var errDecodedBodyTooLarge = errors.New("decoded request body exceeds limit")

// boundedRequestDecoding removes HTTP content codings before any handler runs.
// Fiber's Ctx.Body, BodyParser, and fasthttp's MultipartForm otherwise
// decompress gzip, deflate, brotli, and zstd without a size limit, so a small
// compressed request could claim gigabytes of memory on any route, including
// unauthenticated ones. After this middleware the request carries the decoded
// body and no Content-Encoding header, so proxied requests also stay
// self-consistent. Only Prometheus remote-write (/api/v1/write) is exempted for
// a single snappy layer, whose handler enforces its own bounded snappy decode.
func boundedRequestDecoding(c fiber.Ctx) error {
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
	// Codings are listed in the order they were applied; undo them in reverse.
	for i := len(codings) - 1; i >= 0; i-- {
		decoded, err := decodeBounded(codings[i], body, maxDecodedRequestBody)
		if errors.Is(err, errDecodedBodyTooLarge) {
			return c.Status(fiber.StatusRequestEntityTooLarge).
				JSON(fiber.Map{"error": "decoded request body exceeds limit"})
		}
		if err != nil {
			return c.Status(fiber.StatusBadRequest).
				JSON(fiber.Map{"error": "invalid " + codings[i] + " request body"})
		}
		body = decoded
	}
	c.Request().SetBodyRaw(body)
	c.Request().Header.Del(fiber.HeaderContentEncoding)
	c.Locals(decodedContentEncodingLocal, strings.Join(codings, ", "))
	c.Locals(decodedBodyLocal, body)
	return c.Next()
}

func unsupportedContentEncoding(c fiber.Ctx) error {
	return c.Status(fiber.StatusUnsupportedMediaType).
		JSON(fiber.Map{"error": "unsupported Content-Encoding"})
}

func decodeBounded(coding string, body []byte, limit int) ([]byte, error) {
	if coding == "zstd" {
		var req fasthttp.Request
		req.SetBodyRaw(body)
		decoded, err := req.BodyUnzstdWithLimit(limit)
		if errors.Is(err, fasthttp.ErrBodyTooLarge) {
			return nil, errDecodedBodyTooLarge
		}
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	out := &limitedBuffer{limit: limit}
	var err error
	switch coding {
	case "gzip", "x-gzip":
		_, err = fasthttp.WriteGunzip(out, body)
	case "deflate":
		_, err = fasthttp.WriteInflate(out, body)
	case "br":
		_, err = fasthttp.WriteUnbrotli(out, body)
	default:
		return nil, errors.New("unsupported coding")
	}
	if out.exceeded {
		return nil, errDecodedBodyTooLarge
	}
	if err != nil {
		return nil, err
	}
	return out.buf.Bytes(), nil
}

// gunzipBounded decodes a gzip payload carried inside a request body (for
// example a pprof file part) with the same limit as HTTP content codings.
func gunzipBounded(data []byte, limit int) ([]byte, error) {
	return decodeBounded("gzip", data, limit)
}

// limitedBuffer stops a streaming decoder as soon as output would exceed
// limit, so the decoder never materializes more than limit bytes.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.exceeded = true
		return 0, errDecodedBodyTooLarge
	}
	return b.buf.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)

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
