// Package llmcost loads model token prices and calculates per-call cost.
package llmcost

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

//go:embed pricing.json
var builtInPricing []byte

type Price struct {
	ModelPattern string  `json:"model_pattern"`
	InputPer1K   float64 `json:"input_per_1k"`
	OutputPer1K  float64 `json:"output_per_1k"`
	Currency     string  `json:"currency"`
}

type priceFileEntry struct {
	Price
	Comment string `json:"_comment"`
}

type Table struct {
	prices []Price
}

// Load returns the built-in table, merged with entries from overridePath when set.
// An override replaces a built-in entry with the same normalized model pattern;
// patterns not present in the built-in table are appended.
func Load(overridePath string) (*Table, error) {
	builtIn, err := parsePrices(builtInPricing)
	if err != nil {
		return nil, fmt.Errorf("parse embedded LLM pricing: %w", err)
	}
	merged := make(map[string]Price, len(builtIn))
	for _, price := range builtIn {
		merged[normalize(price.ModelPattern)] = price
	}
	if strings.TrimSpace(overridePath) != "" {
		data, err := os.ReadFile(overridePath)
		if err != nil {
			return nil, fmt.Errorf("read LLM pricing override: %w", err)
		}
		overrides, err := parsePrices(data)
		if err != nil {
			return nil, fmt.Errorf("parse LLM pricing override: %w", err)
		}
		for _, price := range overrides {
			merged[normalize(price.ModelPattern)] = price
		}
	}
	prices := make([]Price, 0, len(merged))
	for _, price := range merged {
		prices = append(prices, price)
	}
	// Longest match wins, so gpt-4o-mini cannot be shadowed by gpt-4o.
	sort.Slice(prices, func(i, j int) bool {
		left, right := normalize(prices[i].ModelPattern), normalize(prices[j].ModelPattern)
		if len(left) != len(right) {
			return len(left) > len(right)
		}
		return left < right
	})
	return &Table{prices: prices}, nil
}

// Default returns the embedded table. The embedded asset is validated by tests;
// panic here indicates a broken release artifact rather than user input.
func Default() *Table {
	table, err := Load("")
	if err != nil {
		panic(err)
	}
	return table
}

func parsePrices(data []byte) ([]Price, error) {
	var entries []priceFileEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	prices := make([]Price, 0, len(entries))
	for i, entry := range entries {
		price := entry.Price
		if strings.TrimSpace(price.ModelPattern) == "" && entry.Comment != "" {
			continue
		}
		if strings.TrimSpace(price.ModelPattern) == "" {
			return nil, fmt.Errorf("entry %d: model_pattern is required", i)
		}
		if price.InputPer1K < 0 || price.OutputPer1K < 0 {
			return nil, fmt.Errorf("entry %d: prices must be non-negative", i)
		}
		if !strings.EqualFold(strings.TrimSpace(price.Currency), "USD") {
			return nil, fmt.Errorf("entry %d: currency must be USD", i)
		}
		price.ModelPattern = normalize(price.ModelPattern)
		price.Currency = "USD"
		prices = append(prices, price)
	}
	if len(prices) == 0 {
		return nil, errors.New("pricing table is empty")
	}
	return prices, nil
}

// Lookup tolerates provider prefixes and model snapshot suffixes.
func (t *Table) Lookup(model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	model = normalize(model)
	for _, price := range t.prices {
		pattern := normalize(price.ModelPattern)
		if model == pattern || strings.HasPrefix(model, pattern+"-") || strings.HasPrefix(model, pattern+":") {
			return price, true
		}
	}
	return Price{}, false
}

func (t *Table) Cost(model string, inputTokens, outputTokens int64) (float64, bool) {
	price, ok := t.Lookup(model)
	if !ok {
		return 0, true
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	return float64(inputTokens)/1000*price.InputPer1K + float64(outputTokens)/1000*price.OutputPer1K, false
}

func normalize(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	model = strings.ReplaceAll(model, "_", "-")
	model = strings.ReplaceAll(model, ".", "-")
	for strings.Contains(model, "--") {
		model = strings.ReplaceAll(model, "--", "-")
	}
	return strings.Trim(model, "-")
}
