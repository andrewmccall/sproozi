package kubernetes

import (
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

// The gateway derives an unrecorded sandbox name the way the controller names
// the Pod; a drift would deny every run's first requests.
func TestSandboxNameMatchesTheController(t *testing.T) {
	run := sprooziv1alpha1.AgentRun{}
	run.UID = "AbC-12-de"
	if got, want := sandboxName(run), runtimekube.RunName(run.UID); got != want {
		t.Fatalf("sandboxName = %q, controller names %q", got, want)
	}
}
