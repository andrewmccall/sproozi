package model_test

import (
	"testing"

	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// A provider without a credential serves no route, and a table entry
// without model routes neither panics nor serves its sign-in or account
// hosts with a gateway-held credential.
func TestRoutesNeedACredentialAndTolerateAnEntryWithoutModels(t *testing.T) {
	for _, provider := range model.Providers() {
		if routes := provider.Routes(nil, model.RouteConfig{}); len(routes) != 0 {
			t.Errorf("%s: %d routes without a credential", provider.Name, len(routes))
		}
	}
	empty := model.Provider{Name: "empty", SignInAuthority: "sign-in.example:443"}
	if routes := empty.Routes(modelauth.APIKey{Key: "k"}, model.RouteConfig{}); len(routes) != 0 {
		t.Errorf("an entry without models served %d routes with a gateway key", len(routes))
	}
	if routes := empty.Routes(modelauth.Workload{}, model.RouteConfig{}); len(routes) != 1 {
		t.Errorf("an entry without models served %d routes with a forwarded sign-in, want its sign-in relay",
			len(routes))
	}
	if hosts := empty.Hosts(true); len(hosts) != 1 {
		t.Errorf("an entry without models needs hosts %v", hosts)
	}
}
