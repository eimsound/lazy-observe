package lazyobserve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type genAIContractFixture struct {
	Version                  int               `json:"version"`
	ContentLimitBytes        int               `json:"content_limit_bytes"`
	BodyInspectionLimitBytes int               `json:"body_inspection_limit_bytes"`
	Attributes               map[string]string `json:"attributes"`
	PathDetectionCases       []struct {
		Path   string `json:"path"`
		System string `json:"system"`
	} `json:"path_detection_cases"`
}

type genAICasesFixture struct {
	Version int             `json:"version"`
	Cases   []genAIConfCase `json:"cases"`
}

type genAIConfCase struct {
	Name           string `json:"name"`
	CaptureContent bool   `json:"capture_content"`
	Request        struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	} `json:"request"`
	Response struct {
		Status      int      `json:"status"`
		ContentType string   `json:"content_type"`
		Body        string   `json:"body"`
		Chunks      []string `json:"chunks"`
		ConsumeBody bool     `json:"consume_body"`
	} `json:"response"`
	Expect struct {
		SpanName               string         `json:"span_name"`
		SpanKind               string         `json:"span_kind"`
		Status                 string         `json:"status"`
		EndedBeforeConsume     bool           `json:"ended_before_consume"`
		Attributes             map[string]any `json:"attributes"`
		AbsentAttributes       []string       `json:"absent_attributes"`
		PromptPresent          bool           `json:"prompt_present"`
		PromptContains         string         `json:"prompt_contains"`
		PromptMaxUTF8Bytes     int            `json:"prompt_max_utf8_bytes"`
		CompletionExact        string         `json:"completion_exact"`
		CompletionMaxUTF8Bytes int            `json:"completion_max_utf8_bytes"`
	} `json:"expect"`
}

var repeatTemplatePattern = regexp.MustCompile(`\{\{repeat:([^:}]+):(\d+)\}\}`)

func expandGenAITemplates(input string) string {
	return repeatTemplatePattern.ReplaceAllStringFunc(input, func(match string) string {
		parts := repeatTemplatePattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		count, err := strconv.Atoi(parts[2])
		if err != nil || count < 0 {
			return match
		}
		return strings.Repeat(parts[1], count)
	})
}

func loadGenAIFixture[T any](t *testing.T, name string) T {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller path")
	}
	path := filepath.Join(filepath.Dir(file), "..", "testdata", "genai", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared GenAI fixture %s: %v", path, err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse shared GenAI fixture %s: %v", path, err)
	}
	return out
}

func TestGenAIConformanceContractAndPathDetection(t *testing.T) {
	t.Parallel()
	contract := loadGenAIFixture[genAIContractFixture](t, "contract.json")

	if llmContentLimit != contract.ContentLimitBytes {
		t.Fatalf("llmContentLimit=%d want=%d", llmContentLimit, contract.ContentLimitBytes)
	}
	if llmBodyInspectionLimit != contract.BodyInspectionLimitBytes {
		t.Fatalf("llmBodyInspectionLimit=%d want=%d", llmBodyInspectionLimit, contract.BodyInspectionLimitBytes)
	}

	gotAttrs := map[string]string{
		"operation_name":                    genAIAttrOperationName,
		"provider_name":                     genAIAttrProviderName,
		"system":                            genAIAttrSystem,
		"request_model":                     genAIAttrRequestModel,
		"response_model":                    genAIAttrResponseModel,
		"usage_input_tokens":                genAIAttrUsageInputTokens,
		"usage_output_tokens":               genAIAttrUsageOutputTokens,
		"usage_cache_read_input_tokens":     genAIAttrUsageCacheReadTokens,
		"usage_cache_creation_input_tokens": genAIAttrUsageCacheCreateTokens,
		"usage_reasoning_tokens":            genAIAttrUsageReasoningTokens,
		"usage_missing":                     genAIAttrUsageMissing,
		"prompt":                            genAIAttrPrompt,
		"completion":                        genAIAttrCompletion,
	}
	if len(gotAttrs) != len(contract.Attributes) {
		t.Fatalf("attribute constant count=%d want=%d", len(gotAttrs), len(contract.Attributes))
	}
	for key, want := range contract.Attributes {
		if gotAttrs[key] != want {
			t.Errorf("attribute %q = %q, want %q", key, gotAttrs[key], want)
		}
	}

	for _, tc := range contract.PathDetectionCases {
		got, ok := detectLLMSystem(tc.Path)
		wantOK := tc.System != ""
		if ok != wantOK || string(got) != tc.System {
			t.Errorf("detectLLMSystem(%q) = (%q, %v), want (%q, %v)", tc.Path, got, ok, tc.System, wantOK)
		}
	}
}

func TestGenAIConformanceCases(t *testing.T) {
	t.Parallel()
	suite := loadGenAIFixture[genAICasesFixture](t, "cases.json")
	if len(suite.Cases) == 0 {
		t.Fatal("shared GenAI conformance cases must not be empty")
	}

	for _, tc := range suite.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			exporter, client := llmTestClient(t, tc.CaptureContent)
			httpClient := &http.Client{
				Transport: client.LLMTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
					status := tc.Response.Status
					if status == 0 {
						status = http.StatusOK
					}
					var body io.ReadCloser
					if len(tc.Response.Chunks) > 0 {
						pr, pw := io.Pipe()
						chunks := make([]string, len(tc.Response.Chunks))
						for i, c := range tc.Response.Chunks {
							chunks[i] = expandGenAITemplates(c)
						}
						go func() {
							for _, chunk := range chunks {
								if _, err := io.WriteString(pw, chunk); err != nil {
									_ = pw.CloseWithError(err)
									return
								}
							}
							_ = pw.Close()
						}()
						body = pr
					} else {
						body = io.NopCloser(bytes.NewBufferString(expandGenAITemplates(tc.Response.Body)))
					}
					return &http.Response{
						StatusCode: status,
						Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
						Header:     http.Header{"Content-Type": []string{tc.Response.ContentType}},
						Body:       body,
					}, nil
				})),
			}

			reqBody := expandGenAITemplates(string(tc.Request.Body))
			req, err := http.NewRequest(
				tc.Request.Method,
				"http://llm.conformance.test"+tc.Request.Path,
				strings.NewReader(reqBody),
			)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.Expect.EndedBeforeConsume {
				if got := len(exporter.GetSpans()); got != 1 {
					t.Fatalf("spans before body consume = %d, want 1", got)
				}
			} else {
				if got := len(exporter.GetSpans()); got != 0 {
					t.Fatalf("spans before stream consume = %d, want 0", got)
				}
			}
			if tc.Response.ConsumeBody {
				_, _ = io.Copy(io.Discard, resp.Body)
			}
			_ = resp.Body.Close()

			span := onlyLLMSpan(t, exporter)
			if span.Name != tc.Expect.SpanName {
				t.Fatalf("span.Name = %q, want %q", span.Name, tc.Expect.SpanName)
			}
			if tc.Expect.SpanKind == "CLIENT" && span.SpanKind != trace.SpanKindClient {
				t.Fatalf("span.SpanKind = %v, want CLIENT", span.SpanKind)
			}
			switch tc.Expect.Status {
			case "ERROR":
				if span.Status.Code != codes.Error {
					t.Fatalf("span.Status.Code = %v, want Error", span.Status.Code)
				}
			case "UNSET":
				if span.Status.Code == codes.Error {
					t.Fatalf("span.Status.Code = %v, want non-Error", span.Status.Code)
				}
			}

			attrs := spanAttributes(span.Attributes)
			for key, want := range tc.Expect.Attributes {
				got, ok := attrs[key]
				if !ok {
					t.Fatalf("missing attribute %q; attrs=%#v", key, attrs)
				}
				switch expected := want.(type) {
				case float64:
					if got != int64(expected) {
						t.Fatalf("attr[%q] = %#v (%T), want %d", key, got, got, int64(expected))
					}
				default:
					if got != expected {
						t.Fatalf("attr[%q] = %#v, want %#v", key, got, expected)
					}
				}
			}
			for _, key := range tc.Expect.AbsentAttributes {
				if val, exists := attrs[key]; exists {
					t.Fatalf("attribute %q unexpectedly present: %#v", key, val)
				}
			}

			if tc.Expect.PromptPresent {
				prompt, ok := attrs[genAIAttrPrompt].(string)
				if !ok || prompt == "" {
					t.Fatalf("expected non-empty %s; attrs=%#v", genAIAttrPrompt, attrs)
				}
				if !utf8.ValidString(prompt) || strings.ContainsRune(prompt, utf8.RuneError) {
					t.Fatalf("invalid UTF-8 in %s", genAIAttrPrompt)
				}
				if tc.Expect.PromptMaxUTF8Bytes > 0 && len(prompt) > tc.Expect.PromptMaxUTF8Bytes {
					t.Fatalf("%s len=%d > max %d", genAIAttrPrompt, len(prompt), tc.Expect.PromptMaxUTF8Bytes)
				}
				if tc.Expect.PromptContains != "" && !strings.Contains(prompt, tc.Expect.PromptContains) {
					t.Fatalf("%s = %q does not contain %q", genAIAttrPrompt, prompt, tc.Expect.PromptContains)
				}
			}

			if tc.Expect.CompletionExact != "" {
				wantCompletion := expandGenAITemplates(tc.Expect.CompletionExact)
				completion, ok := attrs[genAIAttrCompletion].(string)
				if !ok {
					t.Fatalf("expected %s; attrs=%#v", genAIAttrCompletion, attrs)
				}
				if !utf8.ValidString(completion) || strings.ContainsRune(completion, utf8.RuneError) {
					t.Fatalf("invalid UTF-8 in %s", genAIAttrCompletion)
				}
				if tc.Expect.CompletionMaxUTF8Bytes > 0 && len(completion) > tc.Expect.CompletionMaxUTF8Bytes {
					t.Fatalf(
						"%s len=%d > max %d",
						genAIAttrCompletion,
						len(completion),
						tc.Expect.CompletionMaxUTF8Bytes,
					)
				}
				if completion != wantCompletion {
					t.Fatalf(
						"%s len=%d mismatch want len=%d",
						genAIAttrCompletion,
						len(completion),
						len(wantCompletion),
					)
				}
			}
		})
	}
}
