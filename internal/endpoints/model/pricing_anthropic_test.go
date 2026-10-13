package model

import (
	"math"
	"testing"
)

const testAnthropicPricingModel = "claude-test"

func TestAnthropicPricingCountsDisjointCacheCategories(t *testing.T) {
	pricing := PricingTable{Models: map[string]ModelPrice{
		testAnthropicPricingModel: {
			InputMicrosPerMillionTokens:        2_000_000,
			CachedInputMicrosPerMillionTokens:  100_000,
			CacheWrite5mMicrosPerMillionTokens: 2_500_000,
			CacheWrite1hMicrosPerMillionTokens: 4_000_000,
			OutputMicrosPerMillionTokens:       10_000_000,
		},
	}}
	got, err := pricing.costAnthropic(testAnthropicPricingModel, anthropicUsage{
		Input: 10, CacheRead: 20, CacheWrite5m: 30, CacheWrite1h: 40, Output: 50, Total: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 757 {
		t.Fatalf("cost = %d micros, want 757", got)
	}
}

func TestAnthropicPricingRoundsCombinedCostOnce(t *testing.T) {
	pricing := PricingTable{Models: map[string]ModelPrice{
		testAnthropicPricingModel: {
			InputMicrosPerMillionTokens: 1, CachedInputMicrosPerMillionTokens: 1,
			CacheWrite5mMicrosPerMillionTokens: 1, CacheWrite1hMicrosPerMillionTokens: 1,
			OutputMicrosPerMillionTokens: 1,
		},
	}}
	got, err := pricing.costAnthropic(testAnthropicPricingModel, anthropicUsage{
		Input: 1, CacheRead: 1, CacheWrite5m: 1, CacheWrite1h: 1, Output: 1, Total: 5,
	})
	if err != nil || got != 1 {
		t.Fatalf("cost = %d, error = %v, want one micro", got, err)
	}
}

func TestAnthropicPricingRequiresOnlyConsumedCacheWriteRates(t *testing.T) {
	pricing := PricingTable{Models: map[string]ModelPrice{
		testAnthropicPricingModel: {
			InputMicrosPerMillionTokens: 1_000_000, CachedInputMicrosPerMillionTokens: 1_000_000,
			OutputMicrosPerMillionTokens: 1_000_000,
		},
	}}
	if got, err := pricing.costAnthropic(testAnthropicPricingModel, anthropicUsage{Input: 1, Output: 1, Total: 2}); err != nil || got != 2 {
		t.Fatalf("unused optional rates: cost = %d, error = %v", got, err)
	}
	for _, usage := range []anthropicUsage{{CacheWrite5m: 1, Total: 1}, {CacheWrite1h: 1, Total: 1}} {
		if _, err := pricing.costAnthropic(testAnthropicPricingModel, usage); err == nil {
			t.Fatal("consumed cache-write category accepted missing rate")
		}
	}
}

func TestAnthropicPricingRejectsUnknownNegativeAndOverflowUsage(t *testing.T) {
	pricing := PricingTable{Models: map[string]ModelPrice{
		testAnthropicPricingModel: {
			InputMicrosPerMillionTokens: math.MaxInt64, CachedInputMicrosPerMillionTokens: math.MaxInt64,
			CacheWrite5mMicrosPerMillionTokens: math.MaxInt64, CacheWrite1hMicrosPerMillionTokens: math.MaxInt64,
			OutputMicrosPerMillionTokens: math.MaxInt64,
		},
	}}
	for _, tc := range []struct {
		name, model string
		usage       anthropicUsage
	}{
		{"unknown model", "unknown", anthropicUsage{Input: 1, Total: 1}},
		{"negative input", testAnthropicPricingModel, anthropicUsage{Input: -1}},
		{"negative cache read", testAnthropicPricingModel, anthropicUsage{CacheRead: -1}},
		{"negative short cache write", testAnthropicPricingModel, anthropicUsage{CacheWrite5m: -1}},
		{"negative long cache write", testAnthropicPricingModel, anthropicUsage{CacheWrite1h: -1}},
		{"negative output", testAnthropicPricingModel, anthropicUsage{Output: -1}},
		{"cost overflow", testAnthropicPricingModel, anthropicUsage{Input: math.MaxInt64, Total: math.MaxInt64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pricing.costAnthropic(tc.model, tc.usage); err == nil {
				t.Fatal("invalid pricing accepted")
			}
		})
	}
}

func TestAnthropicPricingAllowsLargeIntermediateWithinFinalCost(t *testing.T) {
	pricing := PricingTable{Models: map[string]ModelPrice{
		testAnthropicPricingModel: {InputMicrosPerMillionTokens: math.MaxInt64},
	}}
	got, err := pricing.costAnthropic(testAnthropicPricingModel, anthropicUsage{Input: 1_000_000, Total: 1_000_000})
	if err != nil || got != math.MaxInt64 {
		t.Fatalf("cost = %d, error = %v, want %d", got, err, int64(math.MaxInt64))
	}
}
