package model

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
)

const tokensPerMillion = 1_000_000

// ModelPrice is administrator-owned pricing in millionths of a US dollar per
// million tokens. Using the provider's published unit preserves fractional
// micro-dollar per-token rates without floating-point arithmetic.
type ModelPrice struct {
	InputMicrosPerMillionTokens        int64 `json:"inputMicrosPerMillionTokens"`
	CachedInputMicrosPerMillionTokens  int64 `json:"cachedInputMicrosPerMillionTokens"`
	OutputMicrosPerMillionTokens       int64 `json:"outputMicrosPerMillionTokens"`
	CacheWrite5mMicrosPerMillionTokens int64 `json:"cacheWrite5mMicrosPerMillionTokens,omitempty"`
	CacheWrite1hMicrosPerMillionTokens int64 `json:"cacheWrite1hMicrosPerMillionTokens,omitempty"`
}

// PricingTable contains the exact model identifiers an administrator permits
// the gateway to cost. A model absent from this table is denied.
type PricingTable struct {
	Models map[string]ModelPrice `json:"models"`
}

// LoadPricingTable decodes and validates one strict administrator-owned JSON
// pricing document.
func LoadPricingTable(r io.Reader) (*PricingTable, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	var table PricingTable
	if err := decoder.Decode(&table); err != nil {
		return nil, fmt.Errorf("modelgateway: decode administrator pricing: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("modelgateway: pricing must contain exactly one JSON document")
		}
		return nil, fmt.Errorf("modelgateway: decode administrator pricing: %w", err)
	}
	if len(table.Models) == 0 {
		return nil, fmt.Errorf("modelgateway: administrator pricing must contain at least one model")
	}
	for model, price := range table.Models {
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("modelgateway: administrator pricing contains an empty model identifier")
		}
		if price.InputMicrosPerMillionTokens <= 0 || price.CachedInputMicrosPerMillionTokens <= 0 || price.OutputMicrosPerMillionTokens <= 0 {
			return nil, fmt.Errorf("modelgateway: administrator pricing for model %q must contain positive rates", model)
		}
		if price.CacheWrite5mMicrosPerMillionTokens < 0 || price.CacheWrite1hMicrosPerMillionTokens < 0 {
			return nil, fmt.Errorf("modelgateway: administrator cache-write pricing for model %q must not be negative", model)
		}
	}
	return &table, nil
}

func (p PricingTable) Cost(model string, input, cachedInput, output int64) (int64, error) {
	price, ok := p.Models[model]
	if !ok {
		return 0, fmt.Errorf("modelgateway: no administrator pricing for model %q", model)
	}
	if input < 0 || cachedInput < 0 || output < 0 || cachedInput > input {
		return 0, fmt.Errorf("modelgateway: invalid trusted model usage")
	}
	numerator := new(big.Int)
	numerator.Add(numerator, new(big.Int).Mul(big.NewInt(input-cachedInput), big.NewInt(price.InputMicrosPerMillionTokens)))
	numerator.Add(numerator, new(big.Int).Mul(big.NewInt(cachedInput), big.NewInt(price.CachedInputMicrosPerMillionTokens)))
	numerator.Add(numerator, new(big.Int).Mul(big.NewInt(output), big.NewInt(price.OutputMicrosPerMillionTokens)))
	return roundedCost(numerator)
}

// costAnthropic prices disjoint usage categories. Optional cache-write rates
// become required only when the provider reports consumption in that category.
func (p PricingTable) costAnthropic(model string, usage anthropicUsage) (int64, error) {
	price, ok := p.Models[model]
	if !ok {
		return 0, fmt.Errorf("modelgateway: no administrator pricing for model %q", model)
	}
	numerator := new(big.Int)
	for _, category := range []struct{ tokens, rate int64 }{
		{usage.Input, price.InputMicrosPerMillionTokens},
		{usage.CacheRead, price.CachedInputMicrosPerMillionTokens},
		{usage.CacheWrite5m, price.CacheWrite5mMicrosPerMillionTokens},
		{usage.CacheWrite1h, price.CacheWrite1hMicrosPerMillionTokens},
		{usage.Output, price.OutputMicrosPerMillionTokens},
	} {
		if category.tokens < 0 || (category.tokens > 0 && category.rate <= 0) {
			return 0, fmt.Errorf("modelgateway: invalid trusted model usage or missing administrator rate")
		}
		numerator.Add(numerator, new(big.Int).Mul(big.NewInt(category.tokens), big.NewInt(category.rate)))
	}
	return roundedCost(numerator)
}

func roundedCost(numerator *big.Int) (int64, error) {
	// Round any fractional micro-dollar up so accounting never understates the
	// cost charged against a policy's hard ceiling.
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, big.NewInt(tokensPerMillion), remainder)
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("modelgateway: trusted model usage cost overflow")
	}
	return quotient.Int64(), nil
}
