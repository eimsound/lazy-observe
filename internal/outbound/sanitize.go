package outbound

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// RedactedValue is the canonical placeholder used in redacted channel configs
// and sanitized errors.
const RedactedValue = "[REDACTED]"

// redactedURLPlaceholder uses RFC 3986 unreserved characters so url.URL.String
// never percent-encodes it.
const redactedURLPlaceholder = "REDACTED"

var (
	rawURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)
	kvSecretRe    = regexp.MustCompile(
		`(?i)\b(access_token|api_key|apikey|token|secret|password|passwd|key)=([^\s&;"']+)`,
	)
	bearerRe = regexp.MustCompile(`(?i)\b(Bearer\s+)([^\s;"']+)`)
)

// sensitivePathMarkers identify path segments whose trailing token segments
// carry webhook credentials (Feishu /hook/<token>, Slack /services/T/B/token,
// generic /webhook/<token>, /webhooks/<id>/<token>, etc.).
var sensitivePathMarkers = map[string]struct{}{
	"hook":     {},
	"hooks":    {},
	"webhook":  {},
	"webhooks": {},
	"services": {},
	"token":    {},
	"tokens":   {},
}

// SanitizeURL removes userinfo, fragments, query parameter values, and
// sensitive webhook token path segments from raw. Non-secret URLs with no
// query, userinfo, or sensitive path segments remain unchanged.
func SanitizeURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return RedactedValue
	}
	parsed.User = nil
	parsed.Fragment = ""
	parsed.RawFragment = ""

	if parsed.Path != "" {
		parsed.Path = sanitizeURLPath(parsed.Path)
		parsed.RawPath = ""
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = sanitizeURLQuery(parsed.RawQuery)
	}
	return parsed.String()
}

func sanitizeURLPath(p string) string {
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		lower := strings.ToLower(seg)
		if _, isMarker := sensitivePathMarkers[lower]; isMarker && i+1 < len(segments) && segments[i+1] != "" {
			// Fixed endpoint names such as WeCom's /cgi-bin/webhook/send carry
			// their credential in the query string rather than the path.
			if lower == "webhook" && i+2 == len(segments) && strings.EqualFold(segments[i+1], "send") {
				continue
			}
			return strings.Join(append(segments[:i+1], redactedURLPlaceholder), "/")
		}
		// Telegram-style /bot<token>/sendMessage
		if strings.HasPrefix(lower, "bot") && len(seg) > 3 && strings.Contains(seg, ":") {
			segments[i] = "bot" + redactedURLPlaceholder
		}
	}
	return strings.Join(segments, "/")
}

func sanitizeURLQuery(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err != nil || len(values) == 0 {
		return redactedURLPlaceholder
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+redactedURLPlaceholder)
	}
	return strings.Join(parts, "&")
}

// SanitizeText scrubs URLs, query-style secrets, and bearer tokens from
// arbitrary error strings.
func SanitizeText(text string) string {
	if text == "" {
		return ""
	}
	cleaned := rawURLPattern.ReplaceAllStringFunc(text, SanitizeURL)
	cleaned = kvSecretRe.ReplaceAllString(cleaned, "$1="+RedactedValue)
	cleaned = bearerRe.ReplaceAllString(cleaned, "$1"+RedactedValue)
	return cleaned
}

// SanitizeError converts an outbound transport or policy error into a safe,
// coarse message that never exposes webhook URLs, tokens, credentials, or
// internal IP/port connection details.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrBlockedAddress) {
		return ErrBlockedAddress.Error()
	}
	if errors.Is(err, ErrRedirectBlocked) {
		return ErrRedirectBlocked.Error()
	}
	if errors.Is(err, ErrInvalidTarget) {
		return SanitizeText(err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "outbound request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "outbound request canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "outbound request timed out"
	}
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidCertErr x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &invalidCertErr) {
		return "outbound TLS verification failed"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "outbound DNS resolution failed"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "outbound connection failed"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Err != nil {
			if inner := SanitizeError(urlErr.Err); inner != "" && !strings.Contains(inner, urlErr.URL) {
				return inner
			}
		}
		return "outbound HTTP request failed"
	}
	return SanitizeText(err.Error())
}
