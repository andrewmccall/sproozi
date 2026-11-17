package sandboxpod

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The placement's table (placement/testdata/pod-readings.txt) records every
// site's reading of the native and capture containers, phase, deletion, node
// and init containers. These are the facts it does not reach.

func TestObserveCarriesTheTerminationMessageAndCode(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: AgentContainer, ContainerID: "containerd://a",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: "containerd://a", ExitCode: 4, Message: "result", FinishedAt: metav1.Now(), Reason: "Error"}},
	}}}}
	got := Observe(pod).Agent
	if got.Terminated == nil || *got.Terminated != (Termination{ExitCode: 4, Message: "result", Proven: true}) || !got.Ended() || got.Running {
		t.Fatalf("agent = %+v", got)
	}
	if capture := Observe(pod).Capture; capture.Running || capture.Terminated != nil || capture.Ended() {
		t.Fatalf("absent capture container = %+v", capture)
	}
}

func TestAStartedEphemeralContainerMeansThePodRan(t *testing.T) {
	deleted := metav1.NewTime(time.Now())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleted}}
	if !Observe(pod).NeverScheduled {
		t.Fatal("deleted unassigned Pod with no started container ran")
	}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", ContainerID: "containerd://d"}}
	if Observe(pod).NeverScheduled {
		t.Fatal("Pod whose ephemeral container started never ran")
	}
}

// The kubelet reports one status per container. Should two arrive, the
// first is read, and a later proven end does not prove the first one's.
func TestObserveReadsTheFirstStatusOfAContainer(t *testing.T) {
	running := corev1.ContainerStatus{Name: AgentContainer, ContainerID: "containerd://a",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	ended := corev1.ContainerStatus{Name: AgentContainer, ContainerID: "containerd://b",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://b", FinishedAt: metav1.Now()}}}
	got := Observe(&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running, ended}}}).Agent
	if !got.Running || got.Terminated != nil {
		t.Fatalf("agent = %+v", got)
	}
}
