package ai

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// UntrustedSection is one bounded block of untrusted telemetry or user data
// formatted inside a structured delimiter tag.
type UntrustedSection struct {
	Name string
	Text string
	Cap  int
}

// EscapeDelimiter prevents untrusted content from closing an XML-style prompt
// boundary tag (such as </untrusted_input> or </untrusted_telemetry>).
func EscapeDelimiter(text string) string {
	return strings.ReplaceAll(text, "</", "<\\/")
}

// WrapUntrustedRawJSON wraps raw JSON bytes inside <tag>...</tag> with closing
// delimiter sequences escaped so untrusted data stays outside trusted system instructions.
func WrapUntrustedRawJSON(tag, preamble string, payload []byte) string {
	tag = sanitizeTag(tag)
	escaped := EscapeDelimiter(string(payload))
	var out strings.Builder
	if preamble != "" {
		out.WriteString(preamble)
		out.WriteByte('\n')
	}
	out.WriteByte('<')
	out.WriteString(tag)
	out.WriteString(">\n")
	out.WriteString(escaped)
	out.WriteString("\n</")
	out.WriteString(tag)
	out.WriteByte('>')
	return out.String()
}

// WrapUntrustedJSON marshals payload as JSON and wraps it inside <tag>...</tag>.
func WrapUntrustedJSON(tag, preamble string, payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return WrapUntrustedRawJSON(tag, preamble, encoded), nil
}

// BuildUntrustedSections packs prioritized untrusted telemetry sections into a
// single <tag>...</tag> block bounded strictly by budget bytes on UTF-8 boundaries.
func BuildUntrustedSections(tag string, sections []UntrustedSection, budget int) string {
	tag = sanitizeTag(tag)
	openTag := "<" + tag + ">\n"
	closeTag := "\n</" + tag + ">"
	innerBudget := budget - len(openTag) - len(closeTag)
	if innerBudget <= 0 {
		return ClipUTF8(openTag+closeTag, budget)
	}
	var body strings.Builder
	for _, section := range sections {
		safeText := EscapeDelimiter(section.Text)
		piece := "\n== " + section.Name + " ==\n" + ClipUTF8(safeText, section.Cap)
		remaining := innerBudget - body.Len()
		if remaining <= 0 {
			break
		}
		body.WriteString(ClipUTF8(piece, remaining))
	}
	return openTag + strings.TrimSpace(body.String()) + closeTag
}

// ClipUTF8 truncates value to at most limit bytes on a valid UTF-8 rune
// boundary, appending a truncation marker when clipped.
func ClipUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	const suffix = "\n...[truncated]"
	limit -= len(suffix)
	if limit < 0 {
		return suffix[:max(0, limit+len(suffix))]
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + suffix
}

// FirstRunes returns at most limit Unicode runes from value.
func FirstRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func sanitizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "untrusted_input"
	}
	return tag
}
