package llmcost

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltInMatchingAndCost(t *testing.T) {
	table, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		model        string
		pattern      string
		wantUnpriced bool
	}{
		{model: "openai/gpt-4o-mini-2024-07-18", pattern: "gpt-4o-mini"},
		{model: "gpt-4.1-2025-04-14", pattern: "gpt-4-1"},
		{model: "anthropic/claude-3.5-sonnet-20241022", pattern: "claude-3-5-sonnet"},
		{model: "models/gemini-2.5-flash-001", pattern: "gemini-2-5-flash"},
		{model: "vendor/unknown-future-model", wantUnpriced: true},
	} {
		t.Run(test.model, func(t *testing.T) {
			price, ok := table.Lookup(test.model)
			if test.wantUnpriced {
				if ok {
					t.Fatalf("unexpected price: %#v", price)
				}
				if cost, unpriced := table.Cost(test.model, 1000, 1000); cost != 0 || !unpriced {
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
		t.Fatalf("open-source price cost=%v unpriced=%v", cost, unpriced)
	}
}

func TestPricingOverrideMergesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	data := `[
  {"model_pattern":"gpt-4o-mini","input_per_1k":1,"output_per_1k":2,"currency":"USD"},
  {"model_pattern":"private-model","input_per_1k":3,"output_per_1k":4,"currency":"USD"}
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
	if _, ok := table.Lookup("claude-sonnet-4-20250514"); !ok {
		t.Fatal("override discarded built-in entries")
	}
}
