package localsetup

import "testing"

// Stopped and imported sessions have no worker container, so they never keep
// the home daemon from stopping.
func TestRunningPhasesExcludeStoppedSessions(t *testing.T) {
	for phase, want := range map[string]bool{
		"idle": false, "cancelled": false, "retained": false, "importing": false, "": false,
		"active": true, "stopping": true, "unknown-stop": true, "unknown-usage": true, "preparing": true,
	} {
		if got := running(phase); got != want {
			t.Errorf("running(%q) = %v, want %v", phase, got, want)
		}
	}
}
