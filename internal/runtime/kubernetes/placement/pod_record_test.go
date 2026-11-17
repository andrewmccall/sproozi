package placement

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/sandboxpod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var updateRecords = flag.Bool("update-records", false,
	"rewrite testdata/pod-readings.txt and testdata/retained-records.txt from the current code")

// compareRecorded compares lines with the recorded file, or rewrites it under
// -update-records.
func compareRecorded(t *testing.T, file string, lines []string) {
	t.Helper()
	got := strings.Join(lines, "\n") + "\n"
	if *updateRecords {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d lines, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("reading changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

const readingContainerID = "containerd://recorded"

// readingContainers are the container states a sandbox Pod reports, by name.
// The terminated ones include the kubelet's synthetic ContainerStatusUnknown
// placeholder and each field the writer fence requires, removed one at a time.
func readingContainers() []struct {
	name   string
	status func(name string) *corev1.ContainerStatus
} {
	finished := metav1.NewTime(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	ended := func(code int32, mutate func(*corev1.ContainerStatus)) func(string) *corev1.ContainerStatus {
		return func(name string) *corev1.ContainerStatus {
			s := &corev1.ContainerStatus{Name: name, ContainerID: readingContainerID, State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ContainerID: readingContainerID, ExitCode: code,
					FinishedAt: finished, Reason: "Completed", Message: "result"}}}
			if mutate != nil {
				mutate(s)
			}
			return s
		}
	}
	return []struct {
		name   string
		status func(name string) *corev1.ContainerStatus
	}{
		{"absent", func(string) *corev1.ContainerStatus { return nil }},
		{"waiting", func(name string) *corev1.ContainerStatus {
			return &corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}
		}},
		{"running", func(name string) *corev1.ContainerStatus {
			return &corev1.ContainerStatus{Name: name, ContainerID: readingContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
		}},
		{"running-without-id", func(name string) *corev1.ContainerStatus {
			return &corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
		}},
		{"exited-0", ended(0, nil)},
		{"exited-3", ended(3, nil)},
		{"unknown-synthetic", ended(137, func(s *corev1.ContainerStatus) {
			s.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137, Reason: containerStatusUnknown,
				Message: unknownTerminationMessage}
		})},
		{"unknown-with-fields", ended(137, func(s *corev1.ContainerStatus) { s.State.Terminated.Reason = containerStatusUnknown })},
		{"without-finish", ended(0, func(s *corev1.ContainerStatus) { s.State.Terminated.FinishedAt = metav1.Time{} })},
		{"other-runtime", ended(0, func(s *corev1.ContainerStatus) { s.State.Terminated.ContainerID = "containerd://replacement" })},
		{"without-outer-id", ended(0, func(s *corev1.ContainerStatus) { s.ContainerID = "" })},
		{"without-ids", ended(0, func(s *corev1.ContainerStatus) { s.ContainerID, s.State.Terminated.ContainerID = "", "" })},
	}
}

// reading describes what each site that reads a sandbox Pod derives from pod:
// the remote executor's placement (whether each container runs, whether the
// native container's end is proven, its exit code and whether the Pod never
// ran; recorded before the placement read them through sandboxpod.Observe),
// the gateway's and placement's registry.RequireActive, and the controller's
// PodSandboxClient.GetStatus and GetCompletion.
func reading(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build()
	ctx := context.Background()
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{UID: types.UID(pod.Labels[runtimekube.RunUIDLabel])},
		Status: api.AgentRunStatus{Identity: api.AgentRunIdentity{SandboxName: pod.Name, ServiceAccountName: fixtureWorker}}}
	record := &registry.Record{Phase: registry.Active, RunUID: string(run.UID), PodUID: string(pod.UID)}
	record.OriginalDeadline = time.Now().Add(time.Hour)
	active := "ok"
	if err := registry.RequireActive(ctx, kube, run, record, pod.Namespace); errors.Is(err, registry.ErrConflict) {
		active = "conflict"
	} else if err != nil {
		active = "error: " + err.Error()
	}
	sandbox := runtimekube.NewPodSandboxClient(kube, kube)
	status, err := sandbox.GetStatus(ctx, pod.Name)
	if err != nil {
		t.Fatal(err)
	}
	var completion string
	if c, err := sandbox.GetCompletion(ctx, pod.Name, string(run.UID)); err != nil {
		completion = "error: " + err.Error()
	} else {
		completion = fmt.Sprintf("code=%d result=%t", c.ExitCode, c.Result != nil)
	}
	// The placement reads the exit code of a reported termination, or 0.
	o := sandboxpod.Observe(pod)
	var code int32
	if o.Agent.Terminated != nil {
		code = o.Agent.Terminated.ExitCode
	}
	return fmt.Sprintf("agent-running=%t capture-running=%t terminated=%t exit=%d never-scheduled=%t | require-active=%s | status=%s completion=%s",
		o.Agent.Running, o.Capture.Running, o.Agent.Ended(), code, o.NeverScheduled, active, status, completion)
}

// Every site that reads a sandbox Pod, for every combination of the native
// and capture containers' states, Pod phase, deletion, node assignment and an
// init container that started, derives what testdata/pod-readings.txt
// records. The file was written from the code before the sites shared one
// reading of the Pod.
func TestPodReadingsMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	lines := make([]string, 0, 2400)
	containers := readingContainers()
	for _, agent := range containers {
		for _, capture := range containers[:5] {
			for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed, ""} {
				for _, deleting := range []bool{false, true} {
					for _, node := range []string{"", "node-a"} {
						for _, init := range []bool{false, true} {
							pod := &corev1.Pod{
								ObjectMeta: metav1.ObjectMeta{Namespace: runtimekube.AgentsNamespace, Name: "sandbox", UID: "pod-uid",
									Finalizers: []string{runtimekube.ManagedWriterFenceFinalizer},
									Labels: map[string]string{runtimekube.RunUIDLabel: "run-uid", runtimekube.ManagedByLabel: runtimekube.ManagedByValue,
										runtimekube.RunNamespaceLabel: fixtureNamespace}},
								Spec:   corev1.PodSpec{ServiceAccountName: fixtureWorker, NodeName: node},
								Status: corev1.PodStatus{Phase: phase},
							}
							if deleting {
								now := metav1.NewTime(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
								pod.DeletionTimestamp = &now
							}
							if init {
								pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "setup", ContainerID: readingContainerID,
									State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: readingContainerID}}}}
							}
							for _, s := range []*corev1.ContainerStatus{agent.status(nativeContainer), capture.status(runtimekube.CaptureContainer)} {
								if s != nil {
									pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, *s)
								}
							}
							lines = append(lines, fmt.Sprintf("agent=%s capture=%s phase=%q deleting=%t node=%q init-started=%t -> %s",
								agent.name, capture.name, phase, deleting, node, init, reading(t, pod)))
						}
					}
				}
			}
		}
	}
	compareRecorded(t, "testdata/pod-readings.txt", lines)
}
