package manifests_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// An executor shutting down gives requests profile.ShutdownRequestGrace, then
// waits profile.StopBound for the stops it carries on and profile.DrainTail
// for interrupted ones to let go. Each example executor's Pod must outlast
// that, or a rollout kills a stop in flight and loses the agent's reply.
func TestExampleExecutorsOutlastAStopInFlight(t *testing.T) {
	need := profile.ShutdownRequestGrace + profile.StopBound + profile.DrainTail
	for _, example := range []string{filepath.Join(repoRoot(t), "examples", "portable-workflows", "kubernetes"),
		claudeExampleDir(t)} {
		found := 0
		for _, doc := range objectsOfKind(renderKustomization(t, example), "Deployment") {
			spec := nested(doc, "spec", "template", "spec")
			if !runsExecutor(spec) {
				continue
			}
			found++
			seconds, ok := spec["terminationGracePeriodSeconds"].(float64)
			if grace := time.Duration(seconds) * time.Second; !ok || grace <= need {
				t.Errorf("%s: executor Deployment %s has terminationGracePeriodSeconds %v; it needs more than %s",
					example, metadataName(doc), spec["terminationGracePeriodSeconds"], need)
			}
		}
		if found == 0 {
			t.Errorf("%s defines no executor Deployment", example)
		}
	}
}

// runsExecutor reports a Pod spec with a container that runs the executor.
func runsExecutor(spec map[string]any) bool {
	containers, _ := spec["containers"].([]any)
	for _, c := range containers {
		container, _ := c.(map[string]any)
		command, _ := container["command"].([]any)
		if slices.Contains(command, any("/usr/local/bin/session-profile")) {
			return true
		}
	}
	return false
}

func nested(doc map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		next, _ := doc[key].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		doc = next
	}
	return doc
}
