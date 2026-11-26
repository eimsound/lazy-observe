package alert

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"text/template"
)

// annotationTemplatePrelude predeclares the n9e-style `$labels` and `$value`
// variables so rule annotations can write `{{$labels.instance}}` or
// `{{printf "%.0f" $value}}`. It shares line 1 with the user's text, so parse
// errors still report the user's own line numbers.
const annotationTemplatePrelude = `{{$labels := .Labels}}{{$value := .Value}}`

// maxRenderedAnnotationBytes bounds one rendered annotation; output past it
// aborts rendering and falls back to the raw annotation text.
const maxRenderedAnnotationBytes = 16 << 10

var errRenderedAnnotationTooLarge = errors.New("rendered annotation exceeds size limit")

type annotationTemplateData struct {
	Labels map[string]string
	Value  float64
}

func isAnnotationTemplate(text string) bool {
	return strings.Contains(text, "{{")
}

func parseAnnotationTemplate(key, text string) (*template.Template, error) {
	return template.New(key).Option("missingkey=zero").Parse(annotationTemplatePrelude + text)
}

// ValidateRuleAnnotations checks that every rule annotation is a valid Go
// text/template. Runtime errors (for example an out-of-range slice on real
// labels) are not detectable here and fall back to the raw text at
// evaluation time.
func ValidateRuleAnnotations(annotations map[string]string) error {
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		text := annotations[key]
		if !isAnnotationTemplate(text) {
			continue
		}
		if _, err := parseAnnotationTemplate(key, text); err != nil {
			return fmt.Errorf("%w: invalid alert annotation %q template: %w", ErrInvalidInput, key, err)
		}
	}
	return nil
}

type limitedBuffer struct {
	strings.Builder
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errRenderedAnnotationTooLarge
	}
	return b.Builder.Write(p)
}

// annotationRenderer renders one rule's annotation templates for every sample
// of a single evaluation, parsing each distinct template once.
type annotationRenderer struct {
	ruleID int
	parsed map[string]*template.Template
	failed map[string]struct{}
	warned map[string]struct{}
	buf    limitedBuffer
}

func newAnnotationRenderer(ruleID int) *annotationRenderer {
	return &annotationRenderer{
		ruleID: ruleID,
		parsed: map[string]*template.Template{},
		failed: map[string]struct{}{},
		warned: map[string]struct{}{},
	}
}

// Render returns a copy of sample.Annotations with every template rendered
// against the sample's labels and value. A failing annotation keeps its raw
// text so notifications are never dropped or delayed by a bad template.
func (r *annotationRenderer) Render(sample RuleSample) map[string]string {
	out := make(map[string]string, len(sample.Annotations))
	for key, text := range sample.Annotations {
		out[key] = r.renderOne(key, text, sample)
	}
	return out
}

func (r *annotationRenderer) renderOne(key, text string, sample RuleSample) string {
	if !isAnnotationTemplate(text) {
		return text
	}
	cacheKey := key + "\x00" + text
	if _, failed := r.failed[cacheKey]; failed {
		return text
	}
	tmpl, ok := r.parsed[cacheKey]
	if !ok {
		parsed, err := parseAnnotationTemplate(key, text)
		if err != nil {
			r.failed[cacheKey] = struct{}{}
			r.warn(cacheKey, key, sample.Fingerprint, err)
			return text
		}
		tmpl = parsed
		r.parsed[cacheKey] = tmpl
	}
	labels := sample.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	r.buf = limitedBuffer{limit: maxRenderedAnnotationBytes}
	if err := tmpl.Execute(&r.buf, annotationTemplateData{Labels: labels, Value: sample.Value}); err != nil {
		r.warn(cacheKey, key, sample.Fingerprint, err)
		return text
	}
	return r.buf.String()
}

// warn logs at most once per annotation template per evaluation so a broken
// template on a high-cardinality rule does not flood the log.
func (r *annotationRenderer) warn(cacheKey, key, fingerprint string, err error) {
	if _, done := r.warned[cacheKey]; done {
		return
	}
	r.warned[cacheKey] = struct{}{}
	slog.Warn("render alert annotation template failed; using raw text", "rule_id", r.ruleID,
		"annotation", key, "fingerprint", fingerprint, "error", err)
}
