package checkpoint

import (
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// Every managed harness in the registry needs a checkpoint format.
func TestEveryManagedKindHasACheckpointFormat(t *testing.T) {
	for _, kind := range harness.ManagedKinds() {
		_, _, err := CaptureStopped(t.Context(), kind, t.TempDir(), t.TempDir(),
			"01234567-89ab-cdef-0123-456789abcdef", nil, 1<<20)
		if err != nil && strings.Contains(err.Error(), "unsupported native harness") {
			t.Errorf("managed harness %q has no checkpoint format; add capture and restore cases in "+
				"internal/checkpoint/artifact.go", kind)
		}
	}
}
