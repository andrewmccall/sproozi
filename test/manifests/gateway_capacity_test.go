package manifests_test

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

// TestGatewayCapacityLimitsFitConcurrentRuns keeps the gateway's admission
// limits and resource ceilings consistent (ADR 0027): requests stay small,
// limits are raised, the connection limit fits the memory limit at the
// measured per-session budget with Go GC headroom, and the gateway derives its
// Go memory limit from the container limit.
func TestGatewayCapacityLimitsFitConcurrentRuns(t *testing.T) {
	container := renderedGatewayDeployment(t, renderDefault(t)).Spec.Template.Spec.Containers[0]
	resources := container.Resources
	for name, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "25m", corev1.ResourceMemory: "128Mi"} {
		if got := resources.Requests[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("gateway %s request = %s, want %s unchanged", name, got.String(), want)
		}
	}
	memory, cpu := resources.Limits[corev1.ResourceMemory], resources.Limits[corev1.ResourceCPU]
	if memory.Cmp(resource.MustParse("1Gi")) < 0 || cpu.Cmp(resource.MustParse("1")) < 0 {
		t.Fatalf("gateway limits %s CPU, %s memory are too small for concurrent runs", cpu.String(), memory.String())
	}
	env := map[string]string{}
	var memoryLimitSource *corev1.ResourceFieldSelector
	for _, variable := range container.Env {
		env[variable.Name] = variable.Value
		if variable.Name == "SPROOZI_GATEWAY_MEMORY_LIMIT" && variable.ValueFrom != nil {
			memoryLimitSource = variable.ValueFrom.ResourceFieldRef
		}
	}
	if _, set := env["GOMEMLIMIT"]; set || memoryLimitSource == nil ||
		memoryLimitSource.Resource != "limits.memory" || memoryLimitSource.ContainerName != container.Name ||
		(!memoryLimitSource.Divisor.IsZero() && memoryLimitSource.Divisor.Cmp(resource.MustParse("1")) != 0) {
		t.Fatalf("gateway Go memory limit must come from its own limits.memory in bytes: GOMEMLIMIT %q, source %+v",
			env["GOMEMLIMIT"], memoryLimitSource)
	}
	connections, err := strconv.Atoi(env["SPROOZI_GATEWAY_MAX_CONNECTIONS"])
	if err != nil || connections < 1 || connections > proxytransport.MaxConnectionsLimit {
		t.Fatalf("SPROOZI_GATEWAY_MAX_CONNECTIONS = %q", env["SPROOZI_GATEWAY_MAX_CONNECTIONS"])
	}
	// The per-execution limit is left unset so the gateway clamps its
	// default to the connection limit; pinning it would make a lowered
	// connection limit fail at startup.
	if value, set := env["SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION"]; set {
		t.Fatalf("SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION = %q, "+
			"want it unset so the default follows the connection limit", value)
	}
	if connections != proxytransport.DefaultMaxConnections {
		t.Fatalf("install connection limit %d differs from the gateway default %d", connections,
			proxytransport.DefaultMaxConnections)
	}
	// Twice the live session memory, for GC headroom, must fit the limit.
	if need := int64(2 * connections * proxytransport.InspectedSessionBytes); memory.Value() < need {
		t.Fatalf("memory limit %s cannot hold %d inspected connections (needs %d bytes)", memory.String(), connections, need)
	}
}
