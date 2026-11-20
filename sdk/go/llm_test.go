package lazyobserve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDetectLLMSystem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want llmSystem
		ok   bool
	}{
		{"/v1/chat/completions", llmOpenAI, true},
		{"/proxy/v1/messages", llmAnthropic, true},
		{"/api/chat", llmOllama, true},
		{"/v1/models", "", false},
	}
	for _, tt := range tests {
		got, ok := detectLLMSystem(tt.path)
		if got != tt.want || ok != tt.ok {
			t.Errorf("detectLLMSystem(%q) = %q,%v; want %q,%v", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLLMTransportNonStreamingProviders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, path, response, system, responseModel string
		input, output                               int64
	}{
		{
			"openai",
			"/v1/chat/completions",
			`{"model":"gpt-4.1-mini-2026","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			"openai",
			"gpt-4.1-mini-2026",
			11,
			7,
		},
		{
			"anthropic",
			"/v1/messages",
			`{"model":"claude-sonnet-4-2026","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":13,"output_tokens":9}}`,
			"anthropic",
			"claude-sonnet-4-2026",
			13,
			9,
		},
		{
			"ollama",
			"/api/chat",
			`{"model":"llama3.2","message":{"content":"hello"},"prompt_eval_count":17,"eval_count":5}`,
			"ollama",
			"llama3.2",
			17,
			5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter, client := llmTestClient(t, false)
			httpClient := &http.Client{
				Transport: client.LLMTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
					return llmHTTPResponse("application/json", tt.response), nil
				})),
			}
			body := `{"model":"request-model","messages":[{"role":"user","content":"secret prompt"}]}`
			resp, err := httpClient.Post("http://llm.test"+tt.path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			span := onlyLLMSpan(t, exporter)
			attrs := spanAttributes(span.Attributes)
			if attrs["gen_ai.operation.name"] != "chat" || attrs["gen_ai.provider.name"] != tt.system ||
				attrs["gen_ai.system"] != tt.system ||
				attrs["gen_ai.request.model"] != "request-model" ||
				attrs["gen_ai.response.model"] != tt.responseModel {
				t.Fatalf("metadata attributes = %#v", attrs)
			}
			if attrs["gen_ai.usage.input_tokens"] != tt.input || attrs["gen_ai.usage.output_tokens"] != tt.output {
				t.Fatalf("usage attributes = %#v", attrs)
			}
			if _, ok := attrs["gen_ai.prompt"]; ok {
				t.Fatalf("prompt captured by default: %#v", attrs)
			}
			if _, ok := attrs["gen_ai.completion"]; ok {
				t.Fatalf("completion captured by default: %#v", attrs)
			}
		})
	}
}

func TestLLMTransportStreamingUsageAndMissingUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, stream string
		missing      bool
	}{
		{
			"with usage",
			"data: {\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"usage\":{\"prompt_tokens\":19,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n",
			false,
		},
		{
			"without usage",
			"data: {\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exporter, client := llmTestClient(t, true)
			httpClient := &http.Client{
				Transport: client.LLMTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
					return llmHTTPResponse("text/event-stream", tt.stream), nil
				})),
			}
			resp, err := httpClient.Post(
				"http://llm.test/v1/chat/completions",
				"application/json",
				strings.NewReader(
					`{"model":"gpt-stream","stream":true,"messages":[{"role":"user","content":"prompt"}]}`,
				),
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(exporter.GetSpans()) != 0 {
				t.Fatal("stream span ended before response consumption")
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			attrs := spanAttributes(onlyLLMSpan(t, exporter).Attributes)
			if attrs["gen_ai.response.model"] != "gpt-stream" || attrs["gen_ai.completion"] != "hello" {
				t.Fatalf("stream attrs = %#v", attrs)
			}
			if tt.missing {
				if attrs["gen_ai.usage.missing"] != true {
					t.Fatalf("missing usage not marked: %#v", attrs)
				}
				if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
					t.Fatalf("usage guessed: %#v", attrs)
				}
			} else if attrs["gen_ai.usage.input_tokens"] != int64(19) || attrs["gen_ai.usage.output_tokens"] != int64(3) {
				t.Fatalf("stream usage attrs = %#v", attrs)
			}
		})
	}
}

func TestLLMTransportContentCaptureAndTruncation(t *testing.T) {
	t.Parallel()
	exporter, client := llmTestClient(t, true)
	completion := strings.Repeat("界", 2000)
	httpClient := &http.Client{
		Transport: client.LLMTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return llmHTTPResponse(
				"application/json",
				`{"model":"m","choices":[{"message":{"content":"`+completion+`"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			), nil
		})),
	}
	prompt := strings.Repeat("密", 2000)
	resp, err := httpClient.Post(
		"http://llm.test/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+prompt+`"}]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	attrs := spanAttributes(onlyLLMSpan(t, exporter).Attributes)
	for _, key := range []string{"gen_ai.prompt", "gen_ai.completion"} {
		value, ok := attrs[key].(string)
		if !ok || value == "" || len(value) > llmContentLimit || strings.ToValidUTF8(value, "?") != value {
			t.Fatalf("%s was not captured and safely truncated: %T %d", key, attrs[key], len(value))
		}
	}
}

func TestLLMTransportPassThroughAndError(t *testing.T) {
	t.Parallel()
	exporter, client := llmTestClient(t, false)
	wantErr := errors.New("provider unavailable")
	transport := client.LLMTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/health" {
			return llmHTTPResponse("text/plain", "ok"), nil
		}
		return nil, wantErr
	}))
	resp, err := (&http.Client{Transport: transport}).Get("http://llm.test/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(exporter.GetSpans()) != 0 {
		t.Fatal("non-LLM request produced a span")
	}
	_, err = (&http.Client{Transport: transport}).Post(
		"http://llm.test/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"m"}`),
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	span := onlyLLMSpan(t, exporter)
	if span.Status.Code.String() != "Error" || len(span.Events) == 0 {
		t.Fatalf("transport error not recorded: %#v", span)
	}
}

func TestLLMTransportDoesNotMutateCallerRequestAndSetsGetBody(t *testing.T) {
	t.Parallel()
	exporter, client := llmTestClient(t, false)
	payload := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`
	origBody := io.NopCloser(strings.NewReader(payload))
	origReq, err := http.NewRequestWithContext(
		sampledContext(),
		http.MethodPost,
		"http://llm.test/v1/chat/completions",
		origBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	origReq.Header.Set("X-Custom", "keep")
	origGetBody := origReq.GetBody

	var seenTraceparent string
	var retryBody string
	transport := client.LLMTransport(roundTripFunc(func(outReq *http.Request) (*http.Response, error) {
		seenTraceparent = outReq.Header.Get("Traceparent")
		if outReq.GetBody == nil {
			t.Error("cloned request GetBody is nil")
		} else {
			rc, getErr := outReq.GetBody()
			if getErr != nil {
				t.Errorf("GetBody failed: %v", getErr)
			} else {
				b, _ := io.ReadAll(rc)
				_ = rc.Close()
				retryBody = string(b)
			}
		}
		_, _ = io.Copy(io.Discard, outReq.Body)
		_ = outReq.Body.Close()
		return llmHTTPResponse(
			"application/json",
			`{"model":"gpt-test","usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		), nil
	}))

	resp, err := transport.RoundTrip(origReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if origReq.Body != origBody {
		t.Fatal("RoundTrip mutated caller req.Body")
	}
	if origReq.Header.Get("Traceparent") != "" {
		t.Fatalf("RoundTrip mutated caller req.Header: %v", origReq.Header)
	}
	if (origGetBody == nil) != (origReq.GetBody == nil) {
		t.Fatal("RoundTrip mutated caller req.GetBody")
	}
	if seenTraceparent == "" || retryBody != payload {
		t.Fatalf("seenTraceparent=%q retryBody=%q", seenTraceparent, retryBody)
	}
	if len(exporter.GetSpans()) != 1 {
		t.Fatalf("spans=%d want=1", len(exporter.GetSpans()))
	}
}

func TestLLMTransportStreamsOversizedRequestAndResponse(t *testing.T) {
	t.Parallel()
	exporter, client := llmTestClient(t, false)
	largeReq := `{"model":"gpt-vision","messages":[{"role":"user","content":"` +
		strings.Repeat("a", llmBodyInspectionLimit+64*1024) + `"}]}`
	largeResp := `{"model":"gpt-vision","choices":[{"message":{"content":"` +
		strings.Repeat("b", llmBodyInspectionLimit+64*1024) + `"}}]}`

	var receivedReqLen int
	httpClient := &http.Client{
		Transport: client.LLMTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			data, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			_ = req.Body.Close()
			receivedReqLen = len(data)
			return llmHTTPResponse("application/json", largeResp), nil
		})),
	}
	resp, err := httpClient.Post("http://llm.test/v1/chat/completions", "application/json", strings.NewReader(largeReq))
	if err != nil {
		t.Fatal(err)
	}
	gotResp, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if receivedReqLen != len(largeReq) {
		t.Fatalf("receivedReqLen=%d want=%d", receivedReqLen, len(largeReq))
	}
	if len(gotResp) != len(largeResp) {
		t.Fatalf("gotRespLen=%d want=%d", len(gotResp), len(largeResp))
	}
	attrs := spanAttributes(onlyLLMSpan(t, exporter).Attributes)
	if attrs["gen_ai.usage.missing"] != true {
		t.Fatalf("oversized response should mark usage missing: %#v", attrs)
	}
}

type failingReadCloser struct {
	prefix *bytes.Reader
	err    error
}

func (f *failingReadCloser) Read(p []byte) (int, error) {
	if f.prefix.Len() > 0 {
		return f.prefix.Read(p)
	}
	return 0, f.err
}

func (f *failingReadCloser) Close() error { return nil }

func TestLLMTransportPreservesResponseStatusAndHeadersOnBodyReadError(t *testing.T) {
	t.Parallel()
	exporter, client := llmTestClient(t, false)
	bodyErr := errors.New("connection reset mid-body")
	transport := client.LLMTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"req-42"}},
			Body:       &failingReadCloser{prefix: bytes.NewReader([]byte(`{"partial":`)), err: bodyErr},
		}, nil
	}))
	req, err := http.NewRequest(
		http.MethodPost,
		"http://llm.test/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip swallowed response on body read error: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-Request-Id") != "req-42" {
		t.Fatalf("resp status=%d header=%v", resp.StatusCode, resp.Header)
	}
	partial, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(partial) != `{"partial":` || !errors.Is(readErr, bodyErr) {
		t.Fatalf("body partial=%q err=%v", string(partial), readErr)
	}
	span := onlyLLMSpan(t, exporter)
	if span.Status.Code.String() != "Error" || len(span.Events) == 0 {
		t.Fatalf("body read error not recorded on span: %#v", span)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func llmHTTPResponse(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func llmTestClient(t *testing.T, capture bool) (*tracetest.InMemoryExporter, *Client) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exporter, &Client{
		cfg:        Config{CaptureLLMContent: capture},
		tracer:     tp,
		propagator: propagation.TraceContext{},
	}
}

func onlyLLMSpan(t *testing.T, exporter *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1", len(spans))
	}
	return spans[0]
}

func spanAttributes(values []attribute.KeyValue) map[string]any {
	result := make(map[string]any, len(values))
	for _, value := range values {
		result[string(value.Key)] = value.Value.AsInterface()
	}
	return result
}
