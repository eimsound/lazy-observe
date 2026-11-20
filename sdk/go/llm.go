package lazyobserve

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	llmContentLimit        = 4 * 1024
	llmBodyInspectionLimit = 1 << 20
)

type llmSystem string

const (
	llmOpenAI    llmSystem = "openai"
	llmAnthropic llmSystem = "anthropic"
	llmOllama    llmSystem = "ollama"
)

// LLMTransport explicitly instruments OpenAI-compatible, Anthropic, and
// Ollama HTTP calls. Assign it to an http.Client Transport; it does not patch
// the default client or any provider SDK.
func (c *Client) LLMTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &llmTransport{
		base:           base,
		tracer:         c.tracer.Tracer(instrumentationName + "/llm"),
		propagator:     c.propagator,
		captureContent: c.cfg.CaptureLLMContent,
	}
}

type llmTransport struct {
	base           http.RoundTripper
	tracer         trace.Tracer
	propagator     propagation.TextMapPropagator
	captureContent bool
}

func (t *llmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	system, ok := detectLLMSystem(req.URL.Path)
	if !ok {
		return t.base.RoundTrip(req)
	}
	reqBody, restoredBody, fullRead, err := peekRequestBody(req.Body, llmBodyInspectionLimit)
	if err != nil {
		return nil, err
	}
	var request llmRequest
	if fullRead && len(reqBody) > 0 {
		request = parseLLMRequest(system, reqBody, t.captureContent)
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", string(system)),
		// Retained for compatibility with older OpenTelemetry GenAI consumers.
		attribute.String("gen_ai.system", string(system)),
	}
	if request.Model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", request.Model))
	}
	if t.captureContent && request.Prompt != "" {
		attrs = append(attrs, attribute.String("gen_ai.prompt", truncateLLMContent(request.Prompt)))
	}
	ctx, span := t.tracer.Start(
		req.Context(),
		"chat "+string(system),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	outReq := req.Clone(ctx)
	if outReq.Header == nil {
		outReq.Header = make(http.Header)
	}
	if restoredBody != nil {
		outReq.Body = restoredBody
		if fullRead {
			bodyCopy := reqBody
			outReq.ContentLength = int64(len(bodyCopy))
			outReq.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(bodyCopy)), nil
			}
		}
	}
	t.propagator.Inject(ctx, propagation.HeaderCarrier(outReq.Header))
	resp, err := t.base.RoundTrip(outReq)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		span.SetStatus(codes.Error, resp.Status)
	}
	if resp.Body == nil {
		span.End()
		return resp, nil
	}
	if isStreamingResponse(resp, request.Stream) {
		resp.Body = newLLMStreamBody(resp.Body, system, span, t.captureContent)
		return resp, nil
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, llmBodyInspectionLimit+1))
	if readErr != nil {
		span.RecordError(readErr)
		span.SetStatus(codes.Error, readErr.Error())
		span.End()
		resp.Body = &errTailReadCloser{
			prefix: bytes.NewReader(responseBody),
			err:    readErr,
			closer: resp.Body,
		}
		return resp, nil
	}
	if len(responseBody) > llmBodyInspectionLimit {
		span.SetAttributes(attribute.Bool("gen_ai.usage.missing", true))
		resp.Body = &multiReadCloser{
			Reader: io.MultiReader(bytes.NewReader(responseBody), resp.Body),
			closer: resp.Body,
		}
		span.End()
		return resp, nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(responseBody))
	applyLLMResponse(span, parseLLMResponse(system, responseBody, t.captureContent), t.captureContent)
	span.End()
	return resp, nil
}

type llmRequest struct {
	Model  string
	Prompt string
	Stream bool
}

type llmResponse struct {
	Model        string
	Completion   string
	InputTokens  *int64
	OutputTokens *int64
}

func detectLLMSystem(path string) (llmSystem, bool) {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return llmOpenAI, true
	case strings.Contains(path, "/v1/messages"):
		return llmAnthropic, true
	case strings.Contains(path, "/api/chat"):
		return llmOllama, true
	default:
		return "", false
	}
}

func isStreamingResponse(resp *http.Response, requested bool) bool {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	return requested || strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/x-ndjson")
}

type multiReadCloser struct {
	io.Reader
	closer io.Closer
}

func (m *multiReadCloser) Close() error {
	if m.closer != nil {
		return m.closer.Close()
	}
	return nil
}

type errTailReadCloser struct {
	prefix *bytes.Reader
	err    error
	closer io.Closer
}

func (r *errTailReadCloser) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	return 0, r.err
}

func (r *errTailReadCloser) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

func peekRequestBody(body io.ReadCloser, limit int64) ([]byte, io.ReadCloser, bool, error) {
	if body == nil {
		return nil, nil, true, nil
	}
	prefix, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		_ = body.Close()
		return nil, nil, false, err
	}
	if int64(len(prefix)) <= limit {
		_ = body.Close()
		return prefix, io.NopCloser(bytes.NewReader(prefix)), true, nil
	}
	combined := &multiReadCloser{
		Reader: io.MultiReader(bytes.NewReader(prefix), body),
		closer: body,
	}
	return nil, combined, false, nil
}

func parseLLMRequest(system llmSystem, body []byte, captureContent bool) llmRequest {
	if !captureContent {
		var meta struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(body, &meta) != nil {
			return llmRequest{}
		}
		return llmRequest{Model: meta.Model, Stream: meta.Stream}
	}
	var payload struct {
		Model    string          `json:"model"`
		Stream   bool            `json:"stream"`
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return llmRequest{}
	}
	result := llmRequest{Model: payload.Model, Stream: payload.Stream}
	prompt := map[string]json.RawMessage{"messages": payload.Messages}
	if len(payload.Messages) == 0 {
		prompt["messages"] = json.RawMessage("null")
	}
	if system == llmAnthropic && len(payload.System) > 0 && string(payload.System) != "null" {
		prompt["system"] = payload.System
	}
	if encoded, err := json.Marshal(prompt); err == nil {
		result.Prompt = string(encoded)
	}
	return result
}

func parseLLMResponse(system llmSystem, body []byte, captureContent bool) llmResponse {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return llmResponse{}
	}
	result := llmResponse{Model: stringValue(raw["model"])}
	switch system {
	case llmOpenAI:
		usage := objectValue(raw["usage"])
		result.InputTokens = int64Pointer(usage["prompt_tokens"])
		result.OutputTokens = int64Pointer(usage["completion_tokens"])
		if captureContent {
			result.Completion = openAICompletion(raw)
		}
	case llmAnthropic:
		usage := objectValue(raw["usage"])
		result.InputTokens = int64Pointer(usage["input_tokens"])
		result.OutputTokens = int64Pointer(usage["output_tokens"])
		if captureContent {
			result.Completion = anthropicCompletion(raw["content"])
		}
	case llmOllama:
		result.InputTokens = int64Pointer(raw["prompt_eval_count"])
		result.OutputTokens = int64Pointer(raw["eval_count"])
		if captureContent {
			result.Completion = stringValue(objectValue(raw["message"])["content"])
		}
	}
	return result
}

func applyLLMResponse(span trace.Span, response llmResponse, captureContent bool) {
	attrs := make([]attribute.KeyValue, 0, 4)
	if response.Model != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", response.Model))
	}
	if response.InputTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.input_tokens", *response.InputTokens))
	}
	if response.OutputTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.output_tokens", *response.OutputTokens))
	}
	if response.InputTokens == nil || response.OutputTokens == nil {
		attrs = append(attrs, attribute.Bool("gen_ai.usage.missing", true))
	}
	if captureContent && response.Completion != "" {
		attrs = append(attrs, attribute.String("gen_ai.completion", truncateLLMContent(response.Completion)))
	}
	span.SetAttributes(attrs...)
}

type llmStreamBody struct {
	body           io.ReadCloser
	reader         *bufio.Reader
	system         llmSystem
	span           trace.Span
	captureContent bool
	result         llmResponse
	buffer         bytes.Buffer
	once           sync.Once
}

func newLLMStreamBody(body io.ReadCloser, system llmSystem, span trace.Span, capture bool) io.ReadCloser {
	return &llmStreamBody{
		body:           body,
		reader:         bufio.NewReader(body),
		system:         system,
		span:           span,
		captureContent: capture,
	}
}

func (b *llmStreamBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if n > 0 {
		b.consume(p[:n])
	}
	if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *llmStreamBody) Close() error {
	err := b.body.Close()
	b.finish(err)
	return err
}

func (b *llmStreamBody) consume(chunk []byte) {
	// Streaming JSON is line-oriented (SSE data lines or Ollama NDJSON). Keep
	// incomplete data between Read calls by feeding it through a bounded buffer.
	_, _ = b.buffer.Write(chunk)
	for {
		line, err := b.buffer.ReadString('\n')
		if err != nil {
			if len(line) <= llmBodyInspectionLimit {
				b.buffer.WriteString(line)
			}
			return
		}
		parseStreamLine(&b.result, b.system, line, b.captureContent)
	}
}

func (b *llmStreamBody) finish(err error) {
	b.once.Do(func() {
		if b.buffer.Len() > 0 {
			parseStreamLine(&b.result, b.system, b.buffer.String(), b.captureContent)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			b.span.RecordError(err)
			b.span.SetStatus(codes.Error, err.Error())
		}
		applyLLMResponse(b.span, b.result, b.captureContent)
		b.span.End()
	})
}

func parseStreamLine(result *llmResponse, system llmSystem, line string, captureContent bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
		return
	}
	var raw map[string]any
	if json.Unmarshal([]byte(line), &raw) != nil {
		return
	}
	parsed := parseLLMResponse(system, []byte(line), captureContent)
	mergeLLMResponse(result, parsed, captureContent)
	if system == llmOpenAI && captureContent {
		appendBoundedCompletion(&result.Completion, openAIStreamContent(raw))
	}
	if system == llmAnthropic {
		if message := objectValue(raw["message"]); message != nil {
			if result.Model == "" {
				result.Model = stringValue(message["model"])
			}
			usage := objectValue(message["usage"])
			if value := int64Pointer(usage["input_tokens"]); value != nil {
				result.InputTokens = value
			}
		}
		if usage := objectValue(raw["usage"]); usage != nil {
			if value := int64Pointer(usage["output_tokens"]); value != nil {
				result.OutputTokens = value
			}
		}
		if captureContent {
			if delta := objectValue(raw["delta"]); delta != nil {
				appendBoundedCompletion(&result.Completion, stringValue(delta["text"]))
			}
		}
	}
}

func mergeLLMResponse(dst *llmResponse, src llmResponse, captureContent bool) {
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.InputTokens != nil {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != nil {
		dst.OutputTokens = src.OutputTokens
	}
	if captureContent && src.Completion != "" {
		appendBoundedCompletion(&dst.Completion, src.Completion)
	}
}

func appendBoundedCompletion(dst *string, chunk string) {
	const maxRetained = llmContentLimit + utf8.UTFMax
	if chunk == "" || len(*dst) >= maxRetained {
		return
	}
	remaining := maxRetained - len(*dst)
	if len(chunk) > remaining {
		chunk = chunk[:remaining]
	}
	*dst += chunk
}

func openAICompletion(raw map[string]any) string {
	choices, _ := raw["choices"].([]any)
	var result strings.Builder
	for _, choice := range choices {
		message := objectValue(objectValue(choice)["message"])
		result.WriteString(stringValue(message["content"]))
	}
	return result.String()
}

func openAIStreamContent(raw map[string]any) string {
	choices, _ := raw["choices"].([]any)
	var result strings.Builder
	for _, choice := range choices {
		result.WriteString(stringValue(objectValue(objectValue(choice)["delta"])["content"]))
	}
	return result.String()
}

func anthropicCompletion(value any) string {
	parts, _ := value.([]any)
	var result strings.Builder
	for _, part := range parts {
		result.WriteString(stringValue(objectValue(part)["text"]))
	}
	return result.String()
}

func objectValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func stringValue(value any) string         { result, _ := value.(string); return result }
func int64Pointer(value any) *int64 {
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	result := int64(number)
	return &result
}

func truncateLLMContent(value string) string {
	if len(value) <= llmContentLimit {
		return value
	}
	value = value[:llmContentLimit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
