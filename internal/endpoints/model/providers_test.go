package model_test

import (
	"testing"

	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// A provider with an API authority says how a gateway-held key reaches it;
// Anthropic's Messages API takes it in its own header.
func TestEveryProviderWithAnAPIAuthorityWrapsItsKey(t *testing.T) {
	for _, provider := range model.Providers() {
		hasAPI := len(provider.Models) > 0 && !provider.Models[0].SignedIn
		if hasAPI != (provider.APIKey != nil) {
			t.Errorf("%s: API authority %t, key wrapper %t", provider.Name, hasAPI, provider.APIKey != nil)
		}
	}
	anthropic, _ := model.ProviderFor(model.ProviderAnthropic)
	if _, ok := anthropic.APIKey("k").(modelauth.AnthropicAPIKey); !ok {
		t.Error("an Anthropic key is not sent as an Anthropic API key")
	}
	openAI, _ := model.ProviderFor(model.ProviderOpenAI)
	if _, ok := openAI.APIKey("k").(modelauth.APIKey); !ok {
		t.Error("an OpenAI key is not sent as a bearer API key")
	}
}
