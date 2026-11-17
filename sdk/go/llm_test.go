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
