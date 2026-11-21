package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unsafe"

	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
)

func TestUnifiedErrorHandlerFormatsAndSanitizesErrors(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	app := newFiberApp(defaultReadTimeout, defaultIdleTimeout, nil, logger)
	app.Use(fiberrecover.New())

	app.Get("/bad-request", func(_ fiber.Ctx) error {
		return fiber.NewError(fiber.StatusBadRequest, "invalid PromQL: unexpected token")
	})
	app.Get("/leaky-400", func(_ fiber.Ctx) error {
		return fiber.NewError(fiber.StatusBadRequest, "dial tcp 10.96.0.12:5432: connection refused")
	})
	app.Get("/leaky-500", func(_ fiber.Ctx) error {
		return fiber.NewError(fiber.StatusInternalServerError, "open /etc/lazy-observe/secret.key: permission denied")
	})
	app.Get("/leaky-502", func(_ fiber.Ctx) error {
		return fiber.NewError(fiber.StatusBadGateway, "Get http://10.0.0.8:9428/select/logsql/query: EOF")
	})
	app.Get("/raw-error", func(_ fiber.Ctx) error {
		return errors.New("postgres://admin:supersecret@db.internal:5432/meta failed")
	})
	app.Get("/panic", func(_ fiber.Ctx) error {
		panic("boom with /var/lib/secret token=abc")
	})
	app.Get("/already-written", func(c fiber.Ctx) error {
		_ = c.Status(fiber.StatusTeapot).JSON(fiber.Map{"code": "teapot", "error": "short and stout"})
		return errResponseWritten
	})

	type errResp struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}

	cases := []struct {
		path         string
		wantStatus   int
		wantCode     string
		wantError    string
		forbidSubstr []string
	}{
		{
			path:       "/bad-request",
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
			wantError:  "invalid PromQL: unexpected token",
		},
		{
			path:         "/leaky-400",
			wantStatus:   http.StatusBadRequest,
			wantCode:     "bad_request",
			wantError:    "bad request",
			forbidSubstr: []string{"10.96.0.12", "5432", "dial tcp"},
		},
		{
			path:         "/leaky-500",
			wantStatus:   http.StatusInternalServerError,
			wantCode:     "internal_error",
			wantError:    "internal error",
			forbidSubstr: []string{"/etc/", "secret.key", "permission denied"},
		},
		{
			path:         "/leaky-502",
			wantStatus:   http.StatusBadGateway,
			wantCode:     "bad_gateway",
			wantError:    "upstream storage unavailable",
			forbidSubstr: []string{"10.0.0.8", "9428", "http://", "logsql"},
		},
		{
			path:         "/raw-error",
			wantStatus:   http.StatusInternalServerError,
			wantCode:     "internal_error",
			wantError:    "internal error",
			forbidSubstr: []string{"postgres://", "supersecret", "db.internal"},
		},
		{
			path:         "/panic",
			wantStatus:   http.StatusInternalServerError,
			wantCode:     "internal_error",
			wantError:    "internal error",
			forbidSubstr: []string{"boom", "/var/lib", "token=abc"},
		},
		{
			path:       "/already-written",
			wantStatus: http.StatusTeapot,
			wantCode:   "teapot",
			wantError:  "short and stout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.wantStatus, raw)
			}
			if ct := resp.Header.Get(fiber.HeaderContentType); !strings.Contains(ct, fiber.MIMEApplicationJSON) {
				t.Fatalf("content-type=%q want JSON", ct)
			}
			var got errResp
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("response is not valid JSON (%q): %v", raw, err)
			}
			if got.Code != tc.wantCode || got.Error != tc.wantError {
				t.Fatalf("got=%+v want code=%q error=%q", got, tc.wantCode, tc.wantError)
			}
			for _, forbidden := range tc.forbidSubstr {
				if strings.Contains(string(raw), forbidden) {
					t.Errorf("response leaked %q: %s", forbidden, raw)
				}
			}
		})
	}
}

func TestDecodedRequestBodyAvoidsSecondCopy(t *testing.T) {
	app := newFiberApp(defaultReadTimeout, defaultIdleTimeout, nil, nil)
	app.Use(boundedRequestDecoding)

	var sameBacking bool
	app.Post("/check", func(c fiber.Ctx) error {
		decoded, _ := c.Locals(decodedBodyLocal).([]byte)
		fromReqBody := requestBody(c)
		fromProxy := ownedProxyRequestBody(c, nil)
		if len(decoded) > 0 && len(fromReqBody) == len(decoded) && len(fromProxy) == len(decoded) {
			sameBacking = unsafe.SliceData(decoded) == unsafe.SliceData(fromReqBody) &&
				unsafe.SliceData(decoded) == unsafe.SliceData(fromProxy)
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	payload := bytes.Repeat([]byte("metric_name{env=\"prod\"} 1\n"), 4096)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/check", bytes.NewReader(compressed.Bytes()))
	req.Header.Set(fiber.HeaderContentEncoding, "gzip")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want=204", resp.StatusCode)
	}
	if !sameBacking {
		t.Fatal("decoded request body was copied again instead of reusing the owned decoded slice")
	}
}
