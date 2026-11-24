package endpoints_test

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
)

// Every reserved model authority belongs to a provider in the table: one
// that does not is never served (ADR 0070), so adding one without a table
// entry is a mistake this test names.
func TestEveryModelAuthorityBelongsToAProvider(t *testing.T) {
	owned := map[string]string{}
	for _, provider := range model.Providers() {
		for _, m := range provider.Models {
			owned[m.Authority] = provider.Name
		}
		if provider.SignInAuthority != "" {
			owned[provider.SignInAuthority] = provider.Name
		}
		if provider.AccountAuthority != "" {
			owned[provider.AccountAuthority] = provider.Name
		}
	}
	for authority, capability := range endpoints.SemanticAuthorities {
		if capability == api.CapabilityModelInference && owned[authority] == "" {
			t.Errorf("%s is a model.inference authority no provider in internal/endpoints/model/providers.go names",
				authority)
		}
	}
}
