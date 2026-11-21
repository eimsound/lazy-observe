package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// apiErrorHandler enforces a uniform JSON error contract across all Fiber
// framework errors, handler-returned *fiber.Error values, and unexpected Go
// errors (including recovered panics). All 5xx responses are sanitized so
// internal paths, storage addresses, credentials, and stack traces never leak.
func apiErrorHandler(logger *slog.Logger) fiber.ErrorHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c fiber.Ctx, err error) error {
		if err == nil || errors.Is(err, errResponseWritten) {
			return nil
		}
		status := fiber.StatusInternalServerError
		rawMsg := ""
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			if fiberErr.Code >= 400 && fiberErr.Code <= 599 {
				status = fiberErr.Code
			}
			rawMsg = fiberErr.Message
		}

		if status >= fiber.StatusInternalServerError {
			logger.Warn(
				"gateway request failed",
				"method", strings.Clone(c.Method()),
				"path", strings.Clone(c.Path()),
				"status", status,
				"error", err,
			)
			return c.Status(status).JSON(fiber.Map{
				"code":  httpErrorCode(status),
				"error": sanitizedServerMessage(status),
			})
		}

		return c.Status(status).JSON(fiber.Map{
			"code":  httpErrorCode(status),
			"error": sanitizedClientMessage(status, rawMsg),
		})
	}
}

func httpErrorCode(status int) string {
	switch status {
	case fiber.StatusBadRequest:
		return "bad_request"
	case fiber.StatusUnauthorized:
		return "unauthorized"
	case fiber.StatusForbidden:
		return "forbidden"
	case fiber.StatusNotFound:
		return "not_found"
	case fiber.StatusMethodNotAllowed:
		return "method_not_allowed"
	case fiber.StatusConflict:
		return "conflict"
	case fiber.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case fiber.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case fiber.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case fiber.StatusTooManyRequests:
		return "too_many_requests"
	case fiber.StatusNotImplemented:
		return "not_implemented"
	case fiber.StatusBadGateway:
		return "bad_gateway"
	case fiber.StatusServiceUnavailable:
		return "service_unavailable"
	case fiber.StatusGatewayTimeout:
		return "gateway_timeout"
	default:
		if status >= fiber.StatusInternalServerError {
			return "internal_error"
		}
		return "request_error"
	}
}

func sanitizedServerMessage(status int) string {
	switch status {
	case fiber.StatusNotImplemented:
		return "not implemented"
	case fiber.StatusBadGateway:
		return "upstream storage unavailable"
	case fiber.StatusServiceUnavailable:
		return "service unavailable"
	case fiber.StatusGatewayTimeout:
		return "upstream request timed out"
	default:
		return "internal error"
	}
}

func sanitizedClientMessage(status int, raw string) string {
	msg := strings.TrimSpace(raw)
	if msg == "" || looksLikeInternalDetail(msg) {
		if text := http.StatusText(status); text != "" {
			return strings.ToLower(text)
		}
		return "request error"
	}
	return msg
}

func looksLikeInternalDetail(msg string) bool {
	if strings.ContainsAny(msg, "\r\n") {
		return true
	}
	lower := strings.ToLower(msg)
	for _, marker := range []string{
		"goroutine ",
		"runtime error:",
		"dial tcp",
		"dial unix",
		"http://",
		"https://",
		"postgres://",
		"postgresql://",
		"redis://",
		"sql:",
		"/home/",
		"/var/",
		"/etc/",
		"/tmp/",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
