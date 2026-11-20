package gateway

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
)

const (
	maxTraceServiceNameLength = 256
	maxTraceIDLength          = 32
	maxSourceMapReleaseLength = 256
	maxIssueEventIDLength     = 256
)

// unescapedPathParam decodes one Fiber v3 path parameter exactly once (Fiber v3
// leaves UnescapePath disabled by default), rejects malformed percent-encoding,
// empty/whitespace-only values, ASCII control characters, backslashes, and
// dot-segments ("." or ".."), then enforces the optional route-specific
// allowlist validator. Invalid input returns a unified 400 error.
func unescapedPathParam(c fiber.Ctx, name string, validator func(string) bool) (string, error) {
	decoded, err := url.PathUnescape(c.Params(name))
	if err != nil || !isSafePathParam(decoded) {
		return "", fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid %s", name))
	}
	if validator != nil && !validator(decoded) {
		return "", fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid %s", name))
	}
	return decoded, nil
}

func isSafePathParam(value string) bool {
	if value == "" || strings.TrimSpace(value) == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch < 0x20 || ch == 0x7f || ch == '\\' {
			return false
		}
	}
	for _, seg := range strings.Split(value, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validateTraceServiceName(svc string) bool {
	return len(svc) > 0 && len(svc) <= maxTraceServiceNameLength && isSafePathParam(svc)
}

func validateTraceID(traceID string) bool {
	if len(traceID) == 0 || len(traceID) > maxTraceIDLength {
		return false
	}
	for i := 0; i < len(traceID); i++ {
		ch := traceID[i]
		isDigit := ch >= '0' && ch <= '9'
		isLowerHex := ch >= 'a' && ch <= 'f'
		isUpperHex := ch >= 'A' && ch <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}
	return true
}

func validateSourceMapRelease(release string) bool {
	if len(release) == 0 || len(release) > maxSourceMapReleaseLength {
		return false
	}
	if release == "." || release == ".." {
		return false
	}
	return sourceMapComponent.MatchString(release)
}

func validateIssueEventID(eventID string) bool {
	return len(eventID) > 0 && len(eventID) <= maxIssueEventIDLength && isSafePathParam(eventID)
}
