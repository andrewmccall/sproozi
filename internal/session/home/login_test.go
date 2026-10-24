package home

import (
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
)

// A named login exists only for a harness that signs in itself, and only
// once set up; a refusal names the logins there are (ADR 0049).
func TestStartAcceptsOnlyAConfiguredNamedLogin(t *testing.T) {
	s := &Service{config: Config{Logins: map[harness.NativeKind][]string{harness.NativeCopilot: {"work"}}}}
	copilot := runtimedocker.PlacementConfig{Harness: harness.NativeCopilot, SignIn: true}
	for _, login := range []string{"", "default", "work"} {
		if err := s.checkLogin(copilot, login); err != nil {
			t.Errorf("%q refused: %v", login, err)
		}
	}
	err := s.checkLogin(copilot, "personal")
	if err == nil || !strings.Contains(err.Error(), "default, work") ||
		!strings.Contains(err.Error(), "sproozi init --harness copilot --sign-in --login personal") {
		t.Fatalf("an unknown login: %v", err)
	}
	// Logins are per harness: Copilot's "work" is not Codex's.
	codex := runtimedocker.PlacementConfig{Harness: harness.NativeCodex, SignIn: true}
	if err = s.checkLogin(codex, "work"); err == nil {
		t.Fatal("another harness's login was accepted")
	}
	keyed := runtimedocker.PlacementConfig{Harness: harness.NativeCopilot}
	if err = s.checkLogin(keyed, "work"); err == nil || !strings.Contains(err.Error(), "gateway holds") {
		t.Fatalf("a login for a gateway-held key: %v", err)
	}
	if err = s.checkLogin(copilot, "Bad_Name"); err == nil {
		t.Fatal("an invalid login name was accepted")
	}
}
