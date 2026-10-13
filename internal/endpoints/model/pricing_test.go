package model_test

import (
	"strings"
	"testing"

	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
)

const (
	testResponsesPath = "/v1/responses"
)

func TestPricingTableAccountsInputCachedInputAndOutput(t *testing.T) {
	p := modelgateway.PricingTable{Models: map[string]modelgateway.ModelPrice{
		"admin-model": {
			InputMicrosPerMillionTokens:       2_000_000,
			CachedInputMicrosPerMillionTokens: 1_000_000,
			OutputMicrosPerMillionTokens:      5_000_000,
		},
	}}
	got, err := p.Cost("admin-model", 100, 40, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got != 260 {
		t.Fatalf("cost = %d micros, want 260", got)
	}
}

func TestLoadPricingTableAcceptsAdministratorOwnedModelPrices(t *testing.T) {
	pricing, err := modelgateway.LoadPricingTable(strings.NewReader(`{
		"models": {
			"gpt-5.3-codex": {
				"inputMicrosPerMillionTokens": 1750000,
				"cachedInputMicrosPerMillionTokens": 175000,
				"outputMicrosPerMillionTokens": 14000000
			}
		}
	}`))
	if err != nil {
		t.Fatalf("LoadPricingTable: %v", err)
	}

	got, err := pricing.Cost("gpt-5.3-codex", 1_000_000, 100_000, 10_000)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got != 1_732_500 {
		t.Fatalf("cost = %d micros, want 1732500", got)
	}
}

func TestLoadPricingTableRejectsInvalidOrAmbiguousConfiguration(t *testing.T) {
	cases := map[string]string{
		"empty models":               `{"models":{}}`,
		"unknown field":              `{"models":{"demo":{"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":1,"secret":"no"}}}`,
		"zero input price":           `{"models":{"demo":{"inputMicrosPerMillionTokens":0,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":1}}}`,
		"zero cached price":          `{"models":{"demo":{"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":0,"outputMicrosPerMillionTokens":1}}}`,
		"zero output price":          `{"models":{"demo":{"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":0}}}`,
		"negative short cache write": `{"models":{"demo":{"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":1,"cacheWrite5mMicrosPerMillionTokens":-1}}}`,
		"negative long cache write":  `{"models":{"demo":{"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":1,"cacheWrite1hMicrosPerMillionTokens":-1}}}`,
		"multiple documents":         `{"models":{"demo":{"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":1}}} {}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := modelgateway.LoadPricingTable(strings.NewReader(input)); err == nil {
				t.Fatal("expected invalid pricing configuration to fail closed")
			}
		})
	}
}

func TestLoadPricingTableAcceptsOptionalCacheWriteRates(t *testing.T) {
	pricing, err := modelgateway.LoadPricingTable(strings.NewReader(`{"models":{"claude-test":{
		"inputMicrosPerMillionTokens":1,"cachedInputMicrosPerMillionTokens":1,"outputMicrosPerMillionTokens":1,
		"cacheWrite5mMicrosPerMillionTokens":2,"cacheWrite1hMicrosPerMillionTokens":3
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	price := pricing.Models["claude-test"]
	if price.CacheWrite5mMicrosPerMillionTokens != 2 || price.CacheWrite1hMicrosPerMillionTokens != 3 {
		t.Fatalf("cache write rates = %+v", price)
	}
}

func TestPricingTableFailsClosedForUnknownModel(t *testing.T) {
	_, err := (modelgateway.PricingTable{}).Cost("unpriced", 1, 0, 1)
	if err == nil {
		t.Fatal("expected missing administrator pricing error")
	}
}
