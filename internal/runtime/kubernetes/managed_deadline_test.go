package kubernetes_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

// A session without a deadline builds its Pod, and the helper holds until
// stopped (ADR 0056).
func TestManagedPodWithoutDeadline(t *testing.T) {
	run, runtime, binding := managedFixture()
	binding.OriginalDeadline = time.Time{}
	pod, err := kubernetes.BuildManagedPodSpec(run, runtime, binding)
	if err != nil {
		t.Fatal(err)
	}
	if helper := pod.Spec.Containers[1]; !reflect.DeepEqual(helper.Args, []string{"hold", "--deadline", "none"}) {
		t.Fatalf("helper args = %v", helper.Args)
	}
}
