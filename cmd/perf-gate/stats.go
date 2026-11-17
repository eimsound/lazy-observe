package main

import (
	"math"
	"sort"
	"time"
)

type latencySummary struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}

func summarizeLatencies(values []time.Duration) latencySummary {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return latencySummary{
		P50MS: durationMilliseconds(nearestRank(sorted, 0.50)),
		P95MS: durationMilliseconds(nearestRank(sorted, 0.95)),
		P99MS: durationMilliseconds(nearestRank(sorted, 0.99)),
	}
}

func nearestRank(sorted []time.Duration, quantile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func durationMilliseconds(value time.Duration) float64 {
	return float64(value.Nanoseconds()) / float64(time.Millisecond)
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}
