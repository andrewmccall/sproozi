// Package sandboxpod reads a sandbox Pod's reported status into the facts the
// controller, the gateway and the remote executor decide a run or visit by.
// It holds no client and grants no permission: each binary reads the Pod
// with its own client and RBAC, checks the Pod's identity its own way, and
// passes the Pod here.
package sandboxpod

import corev1 "k8s.io/api/core/v1"

const (
	// AgentContainer runs the harness, or the AgentRun runner. Under a
	// managed session it is the writer.
	AgentContainer = "agent"
	// CaptureContainer is a managed session's checkpoint helper.
	CaptureContainer = "checkpoint"
)

// containerStatusUnknown is the reason the kubelet gives a termination it
// made up for a container its runtime could no longer find.
const containerStatusUnknown = "ContainerStatusUnknown"

// Observation is what one reading of a sandbox Pod shows. Whether the Pod
// exists, and whether it is the expected one, is the reader's: a missing Pod
// means something different to each.
type Observation struct {
	// Phase is the Pod's phase as reported. A managed Pod stays Running
	// while its capture helper runs, whatever its agent did.
	Phase corev1.PodPhase
	// Deleting reports a deletion timestamp: the Pod is going away and must
	// not be given new work, but its containers may still run.
	Deleting bool
	// NeverScheduled reports a Pod no kubelet ever ran: being deleted,
	// assigned to no node and with no container ever started. The API server
	// refuses to bind a Pod that is being deleted to a node, so it never will
	// run.
	NeverScheduled bool
	// Agent and Capture are the two named containers.
	Agent, Capture Container
}

// Container is one container's reported state. A container with no reported
// status is neither running nor terminated.
type Container struct {
	// Running reports a container running in a runtime that gave it an ID.
	Running bool
	// Terminated is the reported termination, or nil. The kubelet also
	// reports a termination it made up, so only Ended proves the container
	// ran and ended.
	Terminated *Termination
}

// Termination is a reported container termination.
type Termination struct {
	ExitCode int32
	// Message is the container's termination message.
	Message string
	// Proven reports a termination the container runtime gave for this very
	// container: it names the container's runtime ID, has a finish time and
	// is not the kubelet's ContainerStatusUnknown placeholder, which carries
	// exit code 137 for a container that may still be running on a node the
	// kubelet lost.
	Proven bool
}

// Ended reports a proven termination, the only one that fences a writer.
func (c Container) Ended() bool { return c.Terminated != nil && c.Terminated.Proven }

// Observe reads pod. Each container is read from the first status reported
// under its name; the kubelet reports one per container.
func Observe(pod *corev1.Pod) Observation {
	o := Observation{Phase: pod.Status.Phase, Deleting: !pod.DeletionTimestamp.IsZero(),
		Agent: container(pod.Status.ContainerStatuses, AgentContainer), Capture: container(pod.Status.ContainerStatuses, CaptureContainer)}
	o.NeverScheduled = pod.Spec.NodeName == "" && o.Deleting && !started(pod)
	return o
}

func container(statuses []corev1.ContainerStatus, name string) Container {
	for _, s := range statuses {
		if s.Name != name {
			continue
		}
		c := Container{Running: s.State.Running != nil && s.ContainerID != ""}
		if t := s.State.Terminated; t != nil {
			c.Terminated = &Termination{ExitCode: t.ExitCode, Message: t.Message,
				Proven: s.ContainerID != "" && t.ContainerID == s.ContainerID && !t.FinishedAt.IsZero() && t.Reason != containerStatusUnknown}
		}
		return c
	}
	return Container{}
}

// started reports any container of pod, of any kind, that a runtime started.
func started(pod *corev1.Pod) bool {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, c := range statuses {
			if c.ContainerID != "" {
				return true
			}
		}
	}
	return false
}
