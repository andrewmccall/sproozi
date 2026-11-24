package model_test

import (
	"net/http"
	"testing"

	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// A provider with an API authority and account requests on another
// capability's host serves the account requests only with the harness's own
// sign-in: a gateway-held key or token for its API never reaches that host
// (ADR 0074).
func TestAGatewayHeldKeyServesNoAccountAuthority(t *testing.T) {
	const api, account = "api.provider.example:443", "api.github.com:443"
	provider := model.Provider{Name: "fixture",
		Models: []model.ModelRoute{{Authority: api, UpstreamURL: "https://api.provider.example",
			New: func(model.HandlerConfig) http.Handler { return http.NotFoundHandler() }}},
		SignInAuthority:  "sign-in.provider.example:443",
		AccountAuthority: account, AccountPrefixes: []string{"/fixture_account/"},
		AccountRoutes: []model.RelayRoute{{Method: http.MethodGet, Path: "/fixture_account/", Prefix: true}}}
	for _, tc := range []struct {
		name    string
		auth    modelauth.Authenticator
		account bool
	}{
		{"gateway key", modelauth.APIKey{Key: "k"}, false},
		{"gateway token", modelauth.Token{Value: "t"}, false},
		{"own sign-in", modelauth.Workload{}, true},
	} {
		served := map[string]bool{}
		for _, r := range provider.Routes(tc.auth, model.RouteConfig{}) {
			served[r.Authority+r.Prefix] = true
		}
		if !served[api] {
			t.Errorf("%s: the API authority is not served", tc.name)
		}
		if served[account+"/fixture_account/"] != tc.account {
			t.Errorf("%s: account route served=%t, want %t", tc.name, !tc.account, tc.account)
		}
	}
}

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
