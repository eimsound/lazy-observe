package gateway

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
)

const (
	vitalDimensionUnknown = "unknown"
	vitalDimensionOther   = "other"

	maxVitalUserAgentBytes = 1024
)

// trustedCountryHeaders lists standard CDN and reverse-proxy country headers in
// precedence order. They are consulted ONLY when c.IsProxyTrusted() is true
// (i.e. security.trusted_proxies explicitly trusts the direct peer IP).
var trustedCountryHeaders = []string{
	"CF-IPCountry",
	"CloudFront-Viewer-Country",
	"X-Vercel-IP-Country",
	"X-Country-Code",
}

type vitalDimensions struct {
	DeviceType    string
	BrowserFamily string
	OSFamily      string
	Country       string
}

type vitalRequestDimensions struct {
	fallbackDeviceType      string
	fallbackBrowserFamily   string
	fallbackOSFamily        string
	hasTrustedCountryHeader bool
	trustedCountry          string
}

func extractVitalRequestDimensions(c fiber.Ctx) vitalRequestDimensions {
	ua := c.Get(fiber.HeaderUserAgent)
	if len(ua) > maxVitalUserAgentBytes {
		ua = ua[:maxVitalUserAgentBytes]
	}
	dims := vitalRequestDimensions{
		fallbackDeviceType:    detectVitalDeviceTypeFromUA(ua),
		fallbackBrowserFamily: detectVitalBrowserFamilyFromUA(ua),
		fallbackOSFamily:      detectVitalOSFamilyFromUA(ua),
		trustedCountry:        vitalDimensionUnknown,
	}
	if c.IsProxyTrusted() {
		for _, header := range trustedCountryHeaders {
			raw := strings.TrimSpace(c.Get(header))
			if raw == "" {
				continue
			}
			dims.hasTrustedCountryHeader = true
			if normalized, ok := normalizeVitalCountry(raw); ok {
				dims.trustedCountry = normalized
			}
			// Lock to the highest-precedence present trusted proxy header so an
			// explicit unknown/reserved value (e.g. CF-IPCountry: XX/T1) cannot
			// be bypassed by a lower-precedence client-spoofed header.
			break
		}
	}
	return dims
}

func resolveVitalDimensions(reqDims vitalRequestDimensions, input vitalInput) vitalDimensions {
	deviceType := reqDims.fallbackDeviceType
	if raw := strings.TrimSpace(input.DeviceType); raw != "" {
		if normalized, ok := normalizeVitalDeviceType(raw); ok {
			deviceType = normalized
		} else {
			deviceType = vitalDimensionUnknown
		}
	}

	browserFamily := reqDims.fallbackBrowserFamily
	if raw := strings.TrimSpace(input.BrowserFamily); raw != "" {
		if normalized, ok := normalizeVitalBrowserFamily(raw); ok {
			browserFamily = normalized
		} else {
			browserFamily = vitalDimensionOther
		}
	}

	osFamily := reqDims.fallbackOSFamily
	if raw := strings.TrimSpace(input.OSFamily); raw != "" {
		if normalized, ok := normalizeVitalOSFamily(raw); ok {
			osFamily = normalized
		} else {
			osFamily = vitalDimensionOther
		}
	}

	country := vitalDimensionUnknown
	if reqDims.hasTrustedCountryHeader {
		country = reqDims.trustedCountry
	} else if normalized, ok := normalizeVitalCountry(input.Country); ok {
		country = normalized
	}

	return vitalDimensions{
		DeviceType:    deviceType,
		BrowserFamily: browserFamily,
		OSFamily:      osFamily,
		Country:       country,
	}
}

func normalizeVitalDeviceType(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "desktop", "mobile", "tablet", vitalDimensionUnknown:
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

func normalizeVitalBrowserFamily(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "chrome", "edge", "safari", "firefox", "opera", vitalDimensionOther:
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

func normalizeVitalOSFamily(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "windows", "macos", "ios", "android", "linux", "chromeos", vitalDimensionOther:
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

// normalizeVitalCountry validates a 2-letter uppercase ASCII ISO 3166-1 alpha-2
// country code and rejects reserved/unknown edge placeholders (XX, T1, ZZ, A1, A2, O1).
func normalizeVitalCountry(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) != 2 {
		return "", false
	}
	b0, b1 := upperASCIIByte(trimmed[0]), upperASCIIByte(trimmed[1])
	if b0 < 'A' || b0 > 'Z' || b1 < 'A' || b1 > 'Z' {
		return "", false
	}
	code := string([]byte{b0, b1})
	switch code {
	case "XX", "T1", "ZZ", "A1", "A2", "O1":
		return "", false
	}
	return code, true
}

func upperASCIIByte(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}

func detectVitalDeviceTypeFromUA(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" || strings.Contains(lower, "jsdom") {
		return vitalDimensionUnknown
	}
	if strings.Contains(lower, "ipad") || strings.Contains(lower, "tablet") ||
		strings.Contains(lower, "playbook") || strings.Contains(lower, "silk") ||
		strings.Contains(lower, "kindle") ||
		(strings.Contains(lower, "android") && !strings.Contains(lower, "mobile")) {
		return "tablet"
	}
	if strings.Contains(lower, "mobi") || strings.Contains(lower, "iphone") ||
		strings.Contains(lower, "ipod") || strings.Contains(lower, "windows phone") ||
		strings.Contains(lower, "blackberry") || strings.Contains(lower, "opera mini") ||
		strings.Contains(lower, "iemobile") {
		return "mobile"
	}
	if strings.Contains(lower, "windows nt") || strings.Contains(lower, "macintosh") ||
		strings.Contains(lower, "mac os x") || strings.Contains(lower, "cros") ||
		strings.Contains(lower, "x11") || strings.Contains(lower, "linux") {
		return "desktop"
	}
	return vitalDimensionUnknown
}

func detectVitalBrowserFamilyFromUA(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" || strings.Contains(lower, "jsdom") {
		return vitalDimensionOther
	}
	if strings.Contains(lower, "edg/") || strings.Contains(lower, "edga/") ||
		strings.Contains(lower, "edgios/") || strings.Contains(lower, "edge/") {
		return "edge"
	}
	if strings.Contains(lower, "opr/") || strings.Contains(lower, "opios/") ||
		strings.Contains(lower, "opera/") || strings.Contains(lower, "opera ") {
		return "opera"
	}
	if strings.Contains(lower, "firefox/") || strings.Contains(lower, "fxios/") {
		return "firefox"
	}
	if strings.Contains(lower, "chrome/") || strings.Contains(lower, "crios/") ||
		strings.Contains(lower, "headlesschrome/") {
		return "chrome"
	}
	if strings.Contains(lower, "safari/") {
		return "safari"
	}
	return vitalDimensionOther
}

func detectVitalOSFamilyFromUA(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" || strings.Contains(lower, "jsdom") {
		return vitalDimensionOther
	}
	if strings.Contains(lower, "cros") {
		return "chromeos"
	}
	if strings.Contains(lower, "android") {
		return "android"
	}
	if strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad") ||
		strings.Contains(lower, "ipod") || strings.Contains(lower, "cpu iphone os") ||
		strings.Contains(lower, "cpu os") {
		return "ios"
	}
	if strings.Contains(lower, "windows") {
		return "windows"
	}
	if strings.Contains(lower, "mac os x") || strings.Contains(lower, "macintosh") {
		return "macos"
	}
	if strings.Contains(lower, "linux") || strings.Contains(lower, "x11") {
		return "linux"
	}
	return vitalDimensionOther
}

func normalizeStoredVitalDeviceType(raw string) string {
	if normalized, ok := normalizeVitalDeviceType(raw); ok {
		return normalized
	}
	return vitalDimensionUnknown
}

func normalizeStoredVitalBrowserFamily(raw string) string {
	if normalized, ok := normalizeVitalBrowserFamily(raw); ok {
		return normalized
	}
	return vitalDimensionOther
}

func normalizeStoredVitalOSFamily(raw string) string {
	if normalized, ok := normalizeVitalOSFamily(raw); ok {
		return normalized
	}
	return vitalDimensionOther
}

func normalizeStoredVitalCountry(raw string) string {
	if normalized, ok := normalizeVitalCountry(raw); ok {
		return normalized
	}
	return vitalDimensionUnknown
}

func validateVitalPageFilter(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > maxVitalPageBytes || !utf8.ValidString(trimmed) {
		return "", fmt.Errorf("invalid page filter")
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < 0x20 || trimmed[i] == 0x7f {
			return "", fmt.Errorf("invalid page filter")
		}
	}
	return trimmed, nil
}
