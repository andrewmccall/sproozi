package localsetup

import (
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// providerFor is ProviderFor for a kind and credential the test expects it
// to know.
func providerFor(t *testing.T, kind harness.NativeKind, credential harness.NativeCredential) Provider {
	t.Helper()
	p, err := ProviderFor(kind, credential)
	if err != nil {
		t.Fatalf("%s with %s: %v", kind, credential, err)
	}
	return p
}

// A kind or provider local setup does not know gets no provider, never
// OpenAI's by default, and every registered kind still gets one for each way
// it reaches its provider.
func TestProviderForRefusesAProviderItDoesNotKnow(t *testing.T) {
	if got, err := ProviderFor(harness.NativeKind("not-registered"), harness.CredentialKey); err == nil {
		t.Fatalf("an unregistered kind reached %+v", got)
	}
	if got, err := providerNamed("not-a-provider"); err == nil || !strings.Contains(err.Error(), `"not-a-provider"`) {
		t.Fatalf("an unknown provider reached %+v (%v)", got, err)
	}
	for _, kind := range harness.ManagedKinds() {
		for _, credential := range []harness.NativeCredential{harness.CredentialKey, harness.CredentialToken,
			harness.CredentialSignIn} {
			if p := providerFor(t, kind, credential); p.Name == "" || p.URL == "" {
				t.Errorf("%s with %s: %+v", kind, credential, p)
			}
		}
	}
}
