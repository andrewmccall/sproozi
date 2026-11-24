package localsetup

import (
	"os"
	"path/filepath"
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

// doctor names a harness the installation journal holds but this release
// does not know, and says what can fix it, and still checks the others.
func TestDoctorNamesAHarnessThisReleaseDoesNotKnow(t *testing.T) {
	l, inst := testInstallation(t)
	inst.Harnesses = append(inst.Harnesses, Harness{Harness: harness.NativeKind("not-registered"), Name: "local-x",
		Port: 18444, ProviderURL: openAIURL, Credential: filepath.Join(l.Credentials(), "openai")})
	if err := os.WriteFile(filepath.Join(l.Credentials(), "openai"), []byte("sk-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var r Report
	checkCredentials(t.Context(), &r, l, inst, false)
	if len(r.Checks) != 2 {
		t.Fatalf("checks %+v; want one for each harness", r.Checks)
	}
	known, unknown := r.Checks[0], r.Checks[1]
	if !known.OK || known.Component != "credential" || !strings.Contains(known.Detail, "codex") {
		t.Errorf("the registered harness's check %+v", known)
	}
	if unknown.OK || unknown.Component != "credential" ||
		!strings.Contains(unknown.Detail, `"not-registered"`) || !strings.Contains(unknown.Detail, "does not know") ||
		!strings.Contains(unknown.Next, "move "+l.Installation()+" aside") {
		t.Errorf("the unknown harness's check %+v; want a failure naming it and how to start over", unknown)
	}
}
