package errortrack

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"strings"
)

var (
	goClosureRE  = regexp.MustCompile(`(?i)(?:\.|·)func\d+$`)
	anonSuffixRE = regexp.MustCompile(
		`(?i)(?:[._-](?:anonymous|lambda|closure)(?:[._-]?\d+)?|(?:anonymous|lambda|closure)\d+|<anonymous>\d*)$`,
	)
	hashPartRE = regexp.MustCompile(`(?i)([._-])[0-9a-f]{6,}([._-]|$)`)
	uuidRE     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`)
	hexRE      = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b`)
	numberRE   = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	spaceRE    = regexp.MustCompile(`\s+`)
)

type normalizedFrame struct{ module, function, filename string }

// Fingerprint groups an event without using line numbers or exception values.
func Fingerprint(e Event) string {
	frames := normalizedFrames(e.Platform, e.Exception.Stacktrace.Frames)
	parts := []string{
		strings.ToLower(strings.TrimSpace(e.Platform)),
		strings.ToLower(strings.TrimSpace(e.Exception.Type)),
	}
	if len(frames) == 0 {
		parts = append(parts, normalizeMessage(e.Message))
	} else {
		for _, f := range frames {
			parts = append(parts, f.module+"|"+f.function+"|"+f.filename)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func normalizedFrames(platform string, input []Frame) []normalizedFrame {
	hasInApp := false
	for _, f := range input {
		hasInApp = hasInApp || f.InApp
	}
	candidates := make([]Frame, 0, len(input))
	for _, f := range input {
		if hasInApp && !f.InApp {
			continue
		}
		candidates = append(candidates, f)
	}
	// SDKs report stack frames outermost-first (caller -> callee), so the tail
	// holds the innermost frames where the failure actually occurred.
	if len(candidates) > 8 {
		candidates = candidates[len(candidates)-8:]
	}
	result := make([]normalizedFrame, 0, len(candidates))
	for _, f := range candidates {
		filename := normalizeFilename(f.Filename)
		module := strings.ToLower(strings.TrimSpace(f.Module))
		function := normalizeFunction(f.Function)
		if isJavaScript(platform) && len([]rune(function)) < 3 {
			function = strings.Trim(strings.Join([]string{module, filename}, "/"), "/")
		}
		n := normalizedFrame{module: module, function: function, filename: filename}
		if len(result) != 0 {
			previous := result[len(result)-1]
			if previous.module == n.module && previous.function == n.function {
				continue
			}
		}
		result = append(result, n)
	}
	return result
}

func normalizeFunction(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	v = goClosureRE.ReplaceAllString(v, "")
	v = anonSuffixRE.ReplaceAllString(v, "")
	return v
}

func normalizeFilename(value string) string {
	v := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	v = strings.TrimPrefix(v, "webpack://")
	v = strings.TrimPrefix(v, "file://")
	parts := strings.Split(v, "/")
	cut := 0
	for i, part := range parts {
		switch strings.ToLower(part) {
		case "dist", "build", "out", "static", "assets", ".next":
			cut = i + 1
		}
	}
	parts = parts[cut:]
	v = path.Clean(strings.Join(parts, "/"))
	v = hashPartRE.ReplaceAllString(v, "$2")
	return strings.ToLower(strings.TrimPrefix(v, "./"))
}

func normalizeMessage(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	v = uuidRE.ReplaceAllString(v, "<uuid>")
	v = hexRE.ReplaceAllString(v, "<hex>")
	v = numberRE.ReplaceAllString(v, "<num>")
	return spaceRE.ReplaceAllString(v, " ")
}

func isJavaScript(platform string) bool {
	return strings.EqualFold(platform, "javascript") || strings.EqualFold(platform, "node")
}
