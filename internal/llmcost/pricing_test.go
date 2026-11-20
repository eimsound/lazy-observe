package llmcost

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuiltInMatchingAndCost(t *testing.T) {
	table, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		model        string
		provider     string
		pattern      string
		wantUnpriced bool
	}{
		{model: "openai/gpt-4o-mini-2024-07-18", pattern: "gpt-4o-mini"},
		{model: "gpt-4.1-2025-04-14", pattern: "gpt-4-1"},
		{model: "anthropic/claude-3.5-sonnet-20241022", pattern: "claude-3-5-sonnet"},
		{model: "anthropic.claude-3-5-sonnet-20241022-v2:0", pattern: "claude-3-5-sonnet"},
		{model: "us.anthropic.claude-sonnet-4-20250514-v1:0", pattern: "claude-sonnet-4"},
		{model: "claude-3-5-sonnet@20241022", pattern: "claude-3-5-sonnet"},
		{model: "claude-3-5-sonnet-latest", pattern: "claude-3-5-sonnet"},
		{model: "models/gemini-2.5-flash-001", pattern: "gemini-2-5-flash"},
		{model: "gemini-2.5-flash-preview-04-17", pattern: "gemini-2-5-flash"},
		{model: "ollama/llama-3.1", pattern: "ollama"},
		{model: "llama3", provider: "ollama", pattern: "ollama"},
		{model: "llama3.2", provider: "ollama", pattern: "ollama"},
		{model: "qwen2.5:7b", provider: "ollama", pattern: "ollama"},
		// Distinct generations and higher-priced variants must NOT be swallowed by base patterns.
		{model: "claude-opus-4-5", wantUnpriced: true},
		{model: "claude-opus-4-5-20251101", wantUnpriced: true},
		{model: "o1-pro", wantUnpriced: true},
		{model: "o1-pro-2025-03-19", wantUnpriced: true},
		{model: "gpt-4o-realtime-preview", wantUnpriced: true},
		{model: "gpt-4o-realtime-preview-2024-12-17", wantUnpriced: true},
		// Commercial open-weight APIs must NOT silently report $0 priced.
		{model: "deepseek-chat", provider: "openai", wantUnpriced: true},
		{model: "qwen-max", wantUnpriced: true},
		{model: "mistral-large-latest", wantUnpriced: true},
		{model: "meta-llama/llama-3.3-70b-instruct", wantUnpriced: true},
		{model: "vendor/unknown-future-model", wantUnpriced: true},
	} {
		t.Run(test.model+"_"+test.provider, func(t *testing.T) {
			price, ok := table.LookupWithProvider(test.model, test.provider)
			if test.wantUnpriced {
				if ok {
					t.Fatalf("unexpected price match: %#v", price)
				}
				if cost, unpriced := table.CostWithUsage(test.model, Usage{
					Provider:     test.provider,
					InputTokens:  1000,
					OutputTokens: 1000,
				}); cost != 0 || !unpriced {
					t.Fatalf("unknown cost=%v unpriced=%v", cost, unpriced)
				}
				return
			}
			if !ok || price.ModelPattern != test.pattern {
				t.Fatalf("price=%#v ok=%v want pattern %q", price, ok, test.pattern)
			}
		})
	}
	cost, unpriced := table.Cost("gpt-4o-mini-2024-07-18", 2000, 500)
	if unpriced || math.Abs(cost-0.0006) > 1e-12 {
		t.Fatalf("cost=%v unpriced=%v", cost, unpriced)
	}
	if cost, unpriced := table.Cost("ollama/llama-3.1", 1000, 1000); cost != 0 || unpriced {
		t.Fatalf("ollama price cost=%v unpriced=%v", cost, unpriced)
	}
}

func TestCostWithCacheAndReasoningTokens(t *testing.T) {
	table := Default()

	// Anthropic prompt caching: input_tokens is exclusive of cache creation and cache read tokens.
	// 100 * 0.003/1K + 5000 * 0.00375/1K + 20000 * 0.0003/1K + 200 * 0.015/1K
	// = 0.0003 + 0.01875 + 0.006 + 0.003 = 0.02805
	anthropicCost, unpriced := table.CostWithUsage("claude-sonnet-4-20250514", Usage{
		Provider:            "anthropic",
		InputTokens:         100,
		CacheCreationTokens: 5000,
		CacheReadTokens:     20000,
		OutputTokens:        200,
	})
	if unpriced || math.Abs(anthropicCost-0.02805) > 1e-12 {
		t.Fatalf("anthropic cached cost=%v unpriced=%v want=0.02805", anthropicCost, unpriced)
	}

	// OpenAI prompt caching: prompt_tokens (1000) includes cached_tokens (800).
	// Uncached 200 * 0.0025/1K + cached 800 * 0.00125/1K + output 200 * 0.01/1K
	// = 0.0005 + 0.001 + 0.002 = 0.0035
	openAICost, unpriced := table.CostWithUsage("gpt-4o-2024-11-20", Usage{
		Provider:        "openai",
		InputTokens:     1000,
		CacheReadTokens: 800,
		OutputTokens:    200,
		ReasoningTokens: 50,
	})
	if unpriced || math.Abs(openAICost-0.0035) > 1e-12 {
		t.Fatalf("openai cached cost=%v unpriced=%v want=0.0035", openAICost, unpriced)
	}
}

func TestPricingOverrideMergesReplacesAndSupportsAliasesAndReasoningRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	data := `[
  {"model_pattern":"gpt-4o-mini","input_per_1k":1,"output_per_1k":2,"currency":"USD"},
  {"model_pattern":"private-model","aliases":["private-alias"],"input_per_1k":3,"output_per_1k":4,"reasoning_per_1k":8,"currency":"USD"}
]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cost, unpriced := table.Cost("openai/gpt-4o-mini-2024-07-18", 1000, 1000); cost != 3 || unpriced {
		t.Fatalf("override cost=%v unpriced=%v", cost, unpriced)
	}
	if cost, unpriced := table.Cost("private-model-v2", 1000, 1000); cost != 7 || unpriced {
		t.Fatalf("added cost=%v unpriced=%v", cost, unpriced)
	}
	// 1000 input ($3) + (1000-500) output ($2) + 500 reasoning ($4) = $9
	if cost, unpriced := table.CostWithUsage("private-alias", Usage{
		InputTokens:     1000,
		OutputTokens:    1000,
		ReasoningTokens: 500,
	}); cost != 9 || unpriced {
		t.Fatalf("alias + reasoning cost=%v unpriced=%v want=9", cost, unpriced)
	}
	if _, ok := table.Lookup("claude-sonnet-4-20250514"); !ok {
		t.Fatal("override discarded built-in entries")
	}
}

func TestPricingSelfHostedProviderAndEffectiveFromSchedule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	data := `[
  {"model_pattern":"gpt-4o","input_per_1k":0.005,"output_per_1k":0.015,"effective_from":"2025-01-01","currency":"USD"},
  {"model_pattern":"gpt-4o","input_per_1k":0.0025,"output_per_1k":0.01,"effective_from":"2026-01-01","currency":"USD"},
  {"model_pattern":"gpt-4o","provider":"azure","input_per_1k":0.003,"output_per_1k":0.012,"effective_from":"2026-01-01","currency":"USD"},
  {"model_pattern":"llama-3.3-70b","provider":"vllm","self_hosted":true,"input_per_1k":0,"output_per_1k":0},
  {"model_pattern":"qwen-32b","provider":"vllm","self_hosted":true,"input_per_1k":0.0002,"output_per_1k":0.0004,"currency":"USD"}
]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Historical timestamp selects earlier effective_from rate.
	oldCost, unpriced := table.CostWithUsage("gpt-4o", Usage{
		Provider:     "openai",
		Timestamp:    time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC),
		InputTokens:  1000,
		OutputTokens: 1000,
	})
	if unpriced || math.Abs(oldCost-0.02) > 1e-12 {
		t.Fatalf("2025 cost=%v unpriced=%v want=0.02", oldCost, unpriced)
	}

	// Current timestamp selects newer effective_from rate.
	newCost, unpriced := table.CostWithUsage("gpt-4o", Usage{
		Provider:     "openai",
		Timestamp:    time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
		InputTokens:  1000,
		OutputTokens: 1000,
	})
	if unpriced || math.Abs(newCost-0.0125) > 1e-12 {
		t.Fatalf("2026 cost=%v unpriced=%v want=0.0125", newCost, unpriced)
	}

	// Timestamp predating all snapshots falls back to earliest known snapshot.
	preCost, unpriced := table.CostWithUsage("gpt-4o", Usage{
		Provider:     "openai",
		Timestamp:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		InputTokens:  1000,
		OutputTokens: 1000,
	})
	if unpriced || math.Abs(preCost-0.02) > 1e-12 {
		t.Fatalf("pre-snapshot cost=%v unpriced=%v want=0.02", preCost, unpriced)
	}

	// Provider-specific entry beats provider-agnostic entry for matching provider.
	azureCost, unpriced := table.CostWithUsage("gpt-4o", Usage{
		Provider:     "azure",
		Timestamp:    time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
		InputTokens:  1000,
		OutputTokens: 1000,
	})
	if unpriced || math.Abs(azureCost-0.015) > 1e-12 {
		t.Fatalf("azure cost=%v unpriced=%v want=0.015", azureCost, unpriced)
	}

	// Self-hosted vllm entry prices at $0 (with SelfHosted=true), while unknown provider stays unpriced.
	vllmPrice, ok := table.LookupWithProvider("llama-3.3-70b", "vllm")
	if !ok || !vllmPrice.SelfHosted || vllmPrice.InputPer1K != 0 || vllmPrice.OutputPer1K != 0 {
		t.Fatalf("vllm price=%#v ok=%v", vllmPrice, ok)
	}
	if _, ok := table.LookupWithProvider("llama-3.3-70b", "together"); ok {
		t.Fatal("commercial provider must not inherit vllm self_hosted entry")
	}

	// Self-hosted with explicit amortized GPU cost still bills the configured rate.
	gpuCost, unpriced := table.CostWithUsage("qwen-32b", Usage{
		Provider:     "vllm",
		InputTokens:  1000,
		OutputTokens: 1000,
	})
	if unpriced || math.Abs(gpuCost-0.0006) > 1e-12 {
		t.Fatalf("vllm gpu cost=%v unpriced=%v want=0.0006", gpuCost, unpriced)
	}

	// Built-in ollama fallback marks SelfHosted=true.
	ollamaPrice, ok := table.LookupWithProvider("llama3.2", "ollama")
	if !ok || !ollamaPrice.SelfHosted {
		t.Fatalf("ollama fallback=%#v ok=%v", ollamaPrice, ok)
	}
	// Built-in entries carry snapshot effective_from metadata.
	gpt4oMini, ok := Default().Lookup("gpt-4o-mini")
	if !ok || gpt4oMini.EffectiveFrom != "2026-07-17" {
		t.Fatalf("built-in gpt-4o-mini=%#v ok=%v", gpt4oMini, ok)
	}
}

func TestPricingValidationRejectsMalformedOverrideFiles(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{
			name:    "unknown field typo",
			payload: `[{"model_pattern":"m","input_per_1k":1,"output_per_1k":2,"unknown_field":true,"currency":"USD"}]`,
			wantErr: "unknown field",
		},
		{
			name:    "trailing json content",
			payload: `[{"model_pattern":"m","input_per_1k":1,"output_per_1k":2,"currency":"USD"}] {"extra":1}`,
			wantErr: "trailing content",
		},
		{
			name:    "invalid effective_from",
			payload: `[{"model_pattern":"m","input_per_1k":1,"output_per_1k":2,"effective_from":"2026-13-45","currency":"USD"}]`,
			wantErr: "effective_from",
		},
		{
			name:    "negative rate",
			payload: `[{"model_pattern":"m","input_per_1k":-0.1,"output_per_1k":2,"currency":"USD"}]`,
			wantErr: "input_per_1k",
		},
		{
			name:    "unsupported currency",
			payload: `[{"model_pattern":"m","input_per_1k":1,"output_per_1k":2,"currency":"EUR"}]`,
			wantErr: "currency must be USD",
		},
		{
			name: "duplicate model provider effective_from",
			payload: `[
				{"model_pattern":"m","provider":"openai","effective_from":"2026-01-01","input_per_1k":1,"output_per_1k":2,"currency":"USD"},
				{"model_pattern":"m","provider":"openai","effective_from":"2026-01-01","input_per_1k":3,"output_per_1k":4,"currency":"USD"}
			]`,
			wantErr: "duplicate pricing entry",
		},
		{
			name:    "empty pricing array",
			payload: `[{"_comment":"comment only"}]`,
			wantErr: "empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.json")
			if err := os.WriteFile(path, []byte(tc.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}
