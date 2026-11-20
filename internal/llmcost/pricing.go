// Package llmcost loads model token prices and calculates per-call cost.
package llmcost

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

//go:embed pricing.json
var builtInPricing []byte

type Price struct {
	ModelPattern       string   `json:"model_pattern"`
	Aliases            []string `json:"aliases,omitempty"`
	InputPer1K         float64  `json:"input_per_1k"`
	OutputPer1K        float64  `json:"output_per_1k"`
	CacheReadPer1K     *float64 `json:"cache_read_per_1k,omitempty"`
	CacheCreationPer1K *float64 `json:"cache_creation_per_1k,omitempty"`
	ReasoningPer1K     *float64 `json:"reasoning_per_1k,omitempty"`
	Currency           string   `json:"currency"`
}

// Usage captures token counts and provider context for an LLM call or aggregate.
type Usage struct {
	Provider            string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	ReasoningTokens     int64
}

type priceFileEntry struct {
	Price
	Comment string `json:"_comment"`
}

type Table struct {
	prices []Price
}

var bedrockGeoPrefixes = []string{
	"us.", "eu.", "apac.", "global.", "au.", "jp.", "ca.", "sa.", "me.", "af.",
}

var bedrockVendorPrefixes = []string{
	"anthropic.", "openai.", "google.", "meta.", "mistral.", "deepseek.", "cohere.", "amazon.", "ai21.",
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
		if err := validateRate(price.InputPer1K); err != nil {
			return nil, fmt.Errorf("entry %d: input_per_1k %w", i, err)
		}
		if err := validateRate(price.OutputPer1K); err != nil {
			return nil, fmt.Errorf("entry %d: output_per_1k %w", i, err)
		}
		if price.CacheReadPer1K != nil {
			if err := validateRate(*price.CacheReadPer1K); err != nil {
				return nil, fmt.Errorf("entry %d: cache_read_per_1k %w", i, err)
			}
		}
		if price.CacheCreationPer1K != nil {
			if err := validateRate(*price.CacheCreationPer1K); err != nil {
				return nil, fmt.Errorf("entry %d: cache_creation_per_1k %w", i, err)
			}
		}
		if price.ReasoningPer1K != nil {
			if err := validateRate(*price.ReasoningPer1K); err != nil {
				return nil, fmt.Errorf("entry %d: reasoning_per_1k %w", i, err)
			}
		}
		if !strings.EqualFold(strings.TrimSpace(price.Currency), "USD") {
			return nil, fmt.Errorf("entry %d: currency must be USD", i)
		}
		price.ModelPattern = normalize(price.ModelPattern)
		if len(price.Aliases) > 0 {
			aliases := make([]string, 0, len(price.Aliases))
			for _, alias := range price.Aliases {
				if normalized := normalize(alias); normalized != "" {
					aliases = append(aliases, normalized)
				}
			}
			price.Aliases = aliases
		}
		price.Currency = "USD"
		prices = append(prices, price)
	}
	if len(prices) == 0 {
		return nil, errors.New("pricing table is empty")
	}
	return prices, nil
}

func validateRate(rate float64) error {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return errors.New("must be a non-negative finite number")
	}
	return nil
}

// Lookup matches exact model patterns, explicit aliases, and known version/date
// snapshot suffixes without swallowing distinct model variants or generations.
func (t *Table) Lookup(model string) (Price, bool) {
	return t.LookupWithProvider(model, "")
}

// LookupWithProvider resolves pricing for model using optional provider context.
// Self-hosted providers (such as ollama) default to $0 unless explicitly priced.
func (t *Table) LookupWithProvider(model, provider string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	selfHosted := hasOllamaPrefix(model) || isSelfHostedProvider(provider)
	normalized := normalize(model)
	if normalized == "" || normalized == "unknown" {
		return Price{}, false
	}
	for _, price := range t.prices {
		if matchesPrice(normalized, price) {
			return price, true
		}
	}
	if selfHosted {
		return Price{ModelPattern: "ollama", InputPer1K: 0, OutputPer1K: 0, Currency: "USD"}, true
	}
	return Price{}, false
}

func (t *Table) Cost(model string, inputTokens, outputTokens int64) (float64, bool) {
	return t.CostWithUsage(model, Usage{InputTokens: inputTokens, OutputTokens: outputTokens})
}

func (t *Table) CostWithUsage(model string, usage Usage) (float64, bool) {
	price, ok := t.LookupWithProvider(model, usage.Provider)
	if !ok {
		return 0, true
	}
	inputTokens := clampNonNegative(usage.InputTokens)
	outputTokens := clampNonNegative(usage.OutputTokens)
	cacheReadTokens := clampNonNegative(usage.CacheReadTokens)
	cacheCreationTokens := clampNonNegative(usage.CacheCreationTokens)
	reasoningTokens := clampNonNegative(usage.ReasoningTokens)

	cacheReadRate := price.InputPer1K
	if price.CacheReadPer1K != nil {
		cacheReadRate = *price.CacheReadPer1K
	}
	cacheCreationRate := price.InputPer1K
	if price.CacheCreationPer1K != nil {
		cacheCreationRate = *price.CacheCreationPer1K
	}

	var inputCost float64
	if isExclusiveCacheInput(model, usage.Provider) {
		inputCost = float64(inputTokens)/1000*price.InputPer1K +
			float64(cacheReadTokens)/1000*cacheReadRate +
			float64(cacheCreationTokens)/1000*cacheCreationRate
	} else {
		uncachedInput := inputTokens - cacheReadTokens
		if uncachedInput < 0 {
			uncachedInput = 0
		}
		inputCost = float64(uncachedInput)/1000*price.InputPer1K +
			float64(cacheReadTokens)/1000*cacheReadRate +
			float64(cacheCreationTokens)/1000*cacheCreationRate
	}

	var outputCost float64
	if price.ReasoningPer1K != nil && reasoningTokens > 0 {
		if reasoningTokens <= outputTokens {
			outputCost = float64(outputTokens-reasoningTokens)/1000*price.OutputPer1K +
				float64(reasoningTokens)/1000*(*price.ReasoningPer1K)
		} else {
			outputCost = float64(outputTokens)/1000*price.OutputPer1K +
				float64(reasoningTokens)/1000*(*price.ReasoningPer1K)
		}
	} else {
		billableOutput := outputTokens
		if reasoningTokens > billableOutput {
			billableOutput = reasoningTokens
		}
		outputCost = float64(billableOutput) / 1000 * price.OutputPer1K
	}

	total := inputCost + outputCost
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return 0, false
	}
	return total, false
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func isSelfHostedProvider(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "ollama")
}

func hasOllamaPrefix(rawModel string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawModel)), "ollama/")
}

func isExclusiveCacheInput(model, provider string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), "anthropic") {
		return true
	}
	return strings.HasPrefix(normalize(model), "claude-")
}

func matchesPrice(normalizedModel string, price Price) bool {
	if matchesPattern(normalizedModel, price.ModelPattern) {
		return true
	}
	for _, alias := range price.Aliases {
		if matchesPattern(normalizedModel, alias) {
			return true
		}
	}
	return false
}

func matchesPattern(normalizedModel, pattern string) bool {
	if normalizedModel == pattern {
		return true
	}
	if !strings.HasPrefix(normalizedModel, pattern) {
		return false
	}
	return isAllowedModelSuffix(normalizedModel[len(pattern):])
}

// isAllowedModelSuffix accepts only explicit aliases (-latest), numeric
// revisions (-001), version tags (-v1, -v2:0, :0), and date/preview snapshot
// suffixes (-YYYY-MM-DD, -YYYYMMDD, -preview-MM-DD). Distinct variants such as
// -pro, -5, or -realtime-preview are rejected so they are never silently billed
// at a cheaper base model's rate.
func isAllowedModelSuffix(suffix string) bool {
	if suffix == "" || suffix == "-latest" || suffix == ":latest" {
		return true
	}
	if strings.HasPrefix(suffix, ":") {
		return isAllDigits(suffix[1:])
	}
	if !strings.HasPrefix(suffix, "-") {
		return false
	}
	rest := suffix[1:]
	if isVersionTag(rest) || isThreeDigitRevision(rest) {
		return true
	}
	if dateTail, ok := cutCompactDate(rest); ok {
		return dateTail == "" || isTrailingVersionOrTag(dateTail)
	}
	if dateTail, ok := cutDashedDate(rest); ok {
		return dateTail == "" || isTrailingVersionOrTag(dateTail)
	}
	if previewRest, ok := strings.CutPrefix(rest, "preview-"); ok {
		if isMonthDay(previewRest) {
			return true
		}
		if dateTail, ok := cutDashedDate(previewRest); ok && dateTail == "" {
			return true
		}
	}
	return false
}

func isTrailingVersionOrTag(tail string) bool {
	if strings.HasPrefix(tail, ":") {
		return isAllDigits(tail[1:])
	}
	if strings.HasPrefix(tail, "-") {
		return isVersionTag(tail[1:])
	}
	return false
}

func isVersionTag(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	rest := s[1:]
	if major, minor, ok := strings.Cut(rest, ":"); ok {
		return isAllDigits(major) && isAllDigits(minor)
	}
	return isAllDigits(rest)
}

func isThreeDigitRevision(s string) bool {
	return len(s) == 3 && isAllDigits(s)
}

func cutCompactDate(s string) (string, bool) {
	if len(s) < 8 || !isAllDigits(s[:8]) {
		return "", false
	}
	if !validYMD(s[0:4], s[4:6], s[6:8]) {
		return "", false
	}
	return s[8:], true
}

func cutDashedDate(s string) (string, bool) {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return "", false
	}
	if !validYMD(s[0:4], s[5:7], s[8:10]) {
		return "", false
	}
	return s[10:], true
}

func isMonthDay(s string) bool {
	if len(s) != 5 || s[2] != '-' || !isAllDigits(s[0:2]) || !isAllDigits(s[3:5]) {
		return false
	}
	month, _ := strconv.Atoi(s[0:2])
	day, _ := strconv.Atoi(s[3:5])
	return month >= 1 && month <= 12 && day >= 1 && day <= 31
}

func validYMD(yearStr, monthStr, dayStr string) bool {
	if !isAllDigits(yearStr) || !isAllDigits(monthStr) || !isAllDigits(dayStr) {
		return false
	}
	year, _ := strconv.Atoi(yearStr)
	month, _ := strconv.Atoi(monthStr)
	day, _ := strconv.Atoi(dayStr)
	return year >= 2000 && year <= 2099 && month >= 1 && month <= 12 && day >= 1 && day <= 31
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func stripBedrockPrefix(model string) string {
	candidate := model
	for _, geo := range bedrockGeoPrefixes {
		if trimmed, ok := strings.CutPrefix(candidate, geo); ok {
			for _, vendor := range bedrockVendorPrefixes {
				if rest, ok := strings.CutPrefix(trimmed, vendor); ok {
					return rest
				}
			}
			break
		}
	}
	for _, vendor := range bedrockVendorPrefixes {
		if rest, ok := strings.CutPrefix(candidate, vendor); ok {
			return rest
		}
	}
	return model
}

func normalize(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	model = stripBedrockPrefix(model)
	model = strings.ReplaceAll(model, "@", "-")
	model = strings.ReplaceAll(model, "_", "-")
	model = strings.ReplaceAll(model, ".", "-")
	for strings.Contains(model, "--") {
		model = strings.ReplaceAll(model, "--", "-")
	}
	return strings.Trim(model, "-")
}
