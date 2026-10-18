package placement

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const fixtureNamespace = "runs"
const fixtureInstruction = "Approved immutable instructions"
const fixtureThread = "01a126e8-288d-7350-9b52-627e16048343"
const fixtureMax = int64(8 << 20)
const fixtureRuntimeName = "native"
const fixtureTypeKey = "type"

func fixtureTermination(containerID string, exitCode int32) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ContainerID: containerID, ExitCode: exitCode, FinishedAt: metav1.Now(), Reason: "Completed",
	}}
}

func TestWriterFenceRejectsSyntheticUnknownContainerTermination(t *testing.T) {
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	local, r, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	// Literal kubelet v1.33.1 unknown-runtime shape: the outer container ID is
	// retained, but synthetic termination carries no runtime ID or finish time.
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 137, Reason: "ContainerStatusUnknown",
		Message: "The container could not be located when the pod was terminated",
	}}
	if err = kube.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if fenced, err := p.physicalFence(t.Context(), &local, r); err != nil || fenced || local.WriterFenced {
		t.Fatalf("synthetic termination became runtime fence: %v %v", fenced, err)
	}
	kube.deferTermination = true
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	o, err := p.Abort(ctx, l, i)
	if err == nil || o.State == session.PlacementStopped || o.Stopped != nil {
		t.Fatalf("synthetic termination supplied cancellation refund: %+v %v", o, err)
	}
	local, r, err = p.retained(t.Context(), l)
	if err != nil || local.WriterFenced || r.Receipt != nil {
		t.Fatalf("synthetic termination retained stop or usage: %+v %+v %v", local, r, err)
	}
	if _, err = p.ledger.Reserve(t.Context(), account(r, api.CapabilityModelInference), budget.UsageReservation{
		RequireExisting: true, MaxUnits: 100, MaxCostMicros: 1000, Units: 1, CostMicros: 1,
	}); err != nil {
		t.Fatalf("synthetic termination sealed the uncertain account: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*corev1.ContainerStateTerminated)
		want   bool
	}{
		{"real-completion", func(*corev1.ContainerStateTerminated) {}, true},
		{"unknown-with-fields", func(s *corev1.ContainerStateTerminated) { s.Reason = containerStatusUnknown }, false},
		{"missing-finish", func(s *corev1.ContainerStateTerminated) { s.FinishedAt = metav1.Time{} }, false},
		{"different-runtime", func(s *corev1.ContainerStateTerminated) { s.ContainerID = "containerd://replacement" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := pod.DeepCopy()
			candidate.Status.ContainerStatuses[0].State = fixtureTermination(candidate.Status.ContainerStatuses[0].ContainerID, 0)
			test.mutate(candidate.Status.ContainerStatuses[0].State.Terminated)
			if got := terminated(candidate); got != test.want {
				t.Fatalf("writer termination=%v, want %v", got, test.want)
			}
		})
	}
}

// admissionClient models an ordinary controller publishing an admitted Pod. It
// does not replace live controller/CNI/helper proof: command execution below is
// deliberately a retained protocol fixture.
type admissionClient struct {
	client.Client
	runCreates       int
	deferTermination bool
}

func (c *admissionClient) Create(ctx context.Context, obj client.Object, options ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID(uuid.NewString()))
	}
	run, ok := obj.(*api.AgentRun)
	if !ok {
		return c.Client.Create(ctx, obj, options...)
	}
	c.runCreates++
	run.Status.Phase = api.AgentRunPhaseRunning
	run.Status.Identity = api.AgentRunIdentity{SandboxName: runtimekube.RunName(run.UID), ServiceAccountName: "approved-worker"}
	if err := c.Client.Create(ctx, run, options...); err != nil {
		return err
	}
	return c.Client.Create(ctx, admittedPod(run))
}

// admittedPod is the running managed worker Pod the controller would create
// for run: writer-fence finalizer, Run labels, and both the native container
// and the capture helper running.
func admittedPod(run *api.AgentRun) *corev1.Pod {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  runtimekube.AgentsNamespace,
			Name:       run.Status.Identity.SandboxName,
			UID:        types.UID(uuid.NewString()),
			Finalizers: []string{runtimekube.ManagedWriterFenceFinalizer},
			Labels: map[string]string{
				runtimekube.RunUIDLabel:       string(run.UID),
				runtimekube.RunNamespaceLabel: run.Namespace,
			},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "approved-worker"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: nativeContainer, ContainerID: "containerd://native", State: running},
				{Name: runtimekube.CaptureContainer, ContainerID: "containerd://helper", State: running},
			},
		},
	}
}

// The API finalizer retains the object; this separate fixture observation models
// the kubelet reporting actual termination, not deletion itself proving a stop.
func (c *admissionClient) Delete(ctx context.Context, obj client.Object, options ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, obj, options...); err != nil {
		return err
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok || c.deferTermination {
		return nil
	}
	var current corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	for index := range current.Status.ContainerStatuses {
		current.Status.ContainerStatuses[index].State = fixtureTermination(current.Status.ContainerStatuses[index].ContainerID, 137)
	}
	return c.Client.Status().Update(ctx, &current)
}

type protocolExecutor struct {
	kube                       client.Client
	data                       []byte
	meta                       checkpoint.Metadata
	initial                    []byte
	state                      harness.NativeState
	lostActivate, skipActivate bool
	finishes                   int
	onCommand                  func(string) error
	failQuiesce                bool
	lostQuiesce                bool
	captureArgs                []string
}

func (e *protocolExecutor) Execute(ctx context.Context, namespace, pod, container string, args []string, input io.Reader, output, _ io.Writer) error {
	if e.onCommand != nil {
		if err := e.onCommand(args[1]); err != nil {
			return err
		}
	}
	if container == runtimekube.CaptureContainer {
		switch args[1] {
		case helperPrepareWorkspace:
			if slices.Contains(args, "--instruction-digest") {
				return errors.New("initial workspace restore cannot accept resume-only instructions")
			}
			data, err := io.ReadAll(input)
			if err != nil || string(data) != string(e.initial) {
				return errors.New("invalid initial input")
			}
			return json.NewEncoder(output).Encode(checkpoint.Metadata{Harness: harness.NativeCodex, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Bytes: len(data)})
		case helperPrepare:
			data, err := io.ReadAll(input)
			if err != nil || string(data) != string(e.data) {
				return errors.New("invalid input")
			}
			return json.NewEncoder(output).Encode(e.meta)
		case helperCapture:
			e.captureArgs = append([]string(nil), args...)
			_, err := output.Write(e.data)
			return err
		case helperFinish:
			e.finishes++
			var actual corev1.Pod
			if err := e.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod}, &actual); err != nil {
				return err
			}
			actual.Status.ContainerStatuses[1].State = fixtureTermination(actual.Status.ContainerStatuses[1].ContainerID, 0)
			return e.kube.Status().Update(ctx, &actual)
		}
	}
	switch args[1] {
	case string(harness.NativeActivate):
		if !e.skipActivate {
			e.state = harness.NativeActive
		}
		if e.lostActivate {
			e.lostActivate = false
			return errors.New("lost native acknowledgement")
		}
	case string(harness.NativeGate):
		e.state = harness.NativeStopping
	case string(harness.NativeQuiesce):
		if e.failQuiesce {
			return errors.New("native completed turn remains unavailable")
		}
		e.state = harness.NativeStopped
		var actual corev1.Pod
		if err := e.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod}, &actual); err != nil {
			return err
		}
		actual.Status.ContainerStatuses[0].State = fixtureTermination(actual.Status.ContainerStatuses[0].ContainerID, 0)
		if err := e.kube.Status().Update(ctx, &actual); err != nil {
			return err
		}
		if e.lostQuiesce {
			return errors.New("lost completed-turn acknowledgement")
		}
	case string(harness.NativeStatus):
	default:
		return errors.New("unexpected command")
	}
	thread := fixtureThread
	if e.initial != nil && (e.state == harness.NativePrepared || args[1] == string(harness.NativeActivate)) {
		thread = ""
	}
	return json.NewEncoder(output).Encode(map[string]any{"op": args[1], "state": e.state, "threadId": thread})
}

type observedLedger struct {
	budget.Ledger
	beforeSeal func(context.Context, budget.AccountKey) error
}

func (l *observedLedger) Seal(ctx context.Context, key budget.AccountKey) (budget.TrustedUsage, error) {
	if err := l.beforeSeal(ctx, key); err != nil {
		return budget.TrustedUsage{}, err
	}
	return l.Ledger.Seal(ctx, key)
}

func TestMoveStopWaitsForNativeTurnBeforeGatewayRevocationAndSeal(t *testing.T) {
	p, kube, e, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	e.onCommand = func(command string) error {
		if command == string(harness.NativeGate) || command == string(harness.NativeQuiesce) {
			r, err := p.registry.Get(ctx, local.Reference)
			if err != nil {
				return err
			}
			if r.Phase != registry.Active {
				return errors.New("gateway revoked before native completed turn")
			}
			var run api.AgentRun
			if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: runName(l)}, &run); err != nil {
				return err
			}
			if err = registry.RequireActive(ctx, kube, &run, &r, runtimekube.AgentsNamespace); err != nil {
				return err
			}
			reservation, err := p.ledger.Reserve(ctx, account(r, api.CapabilityModelInference), budget.UsageReservation{RequireExisting: true, MaxUnits: 100, MaxCostMicros: 1000, Units: 1, CostMicros: 1})
			if err != nil {
				return err
			}
			if err = p.ledger.Settle(ctx, reservation, budget.TrustedUsage{Units: 1, CostMicros: 1}); err != nil {
				return err
			}
		}
		events = append(events, command)
		return nil
	}
	p.ledger = &observedLedger{Ledger: p.ledger, beforeSeal: func(ctx context.Context, _ budget.AccountKey) error {
		r, err := p.registry.Get(ctx, local.Reference)
		if err != nil {
			return err
		}
		if r.Phase != registry.Stopping {
			return errors.New("seal preceded gateway revocation")
		}
		retained, err := p.load(l)
		if err != nil {
			return err
		}
		if !retained.NativeQuiet {
			return errors.New("quiet handoff was not durably saved")
		}
		pod, err := p.exactPod(ctx, r)
		if err != nil {
			return err
		}
		if !terminated(pod) {
			return errors.New("seal preceded actual writer stop")
		}
		events = append(events, "seal")
		return nil
	}}
	s, err := p.Stop(ctx, l, i)
	if err != nil {
		t.Fatal(err)
	}
	want := "gate quiesce seal capture-stopped finish"
	if strings.Join(events, " ") != want || s.Receipt.Usage["model.inference"].Units != 2 {
		t.Fatalf("stop order/turn usage: %v %+v", events, s.Receipt)
	}
}

func TestUnknownNativeQuiescenceKeepsGatewayAndAllocationUntilRetry(t *testing.T) {
	p, _, e, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	e.failQuiesce = true
	sealed := 0
	p.ledger = &observedLedger{Ledger: p.ledger, beforeSeal: func(context.Context, budget.AccountKey) error { sealed++; return nil }}
	if _, err = p.Stop(ctx, l, i); err == nil {
		t.Fatal("unknown quiescence was accepted")
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Get(ctx, local.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != registry.Active || local.NativeQuiet || r.Receipt != nil || local.OutputDigest != "" || sealed != 0 {
		t.Fatalf("unknown turn freed/revoked allocation: %+v %+v seals=%d", r, local, sealed)
	}
	e.failQuiesce = false
	if _, err = p.Stop(ctx, l, i); err != nil || sealed != 1 {
		t.Fatalf("completed turn retry: %v seals=%d", err, sealed)
	}
}

// A failed seal after the exact Pod terminated is the Placement.Stop
// "fenced, usage unsealed" outcome, the same as Docker's, not an unknown stop.
func TestSealFailureAfterFenceReportsStoppedWriterWithUnsealedUsage(t *testing.T) {
	p, _, _, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	ledgerDown := true
	p.ledger = &observedLedger{Ledger: p.ledger, beforeSeal: func(context.Context, budget.AccountKey) error {
		if ledgerDown {
			return errors.New("ledger unavailable")
		}
		return nil
	}}
	stopped, err := p.Stop(ctx, l, i)
	if !errors.Is(err, session.ErrUsageUnsealed) {
		t.Fatalf("seal failure error = %v, want ErrUsageUnsealed", err)
	}
	if !stopped.ActualStopped || stopped.Receipt != nil || stopped.Incarnation != i {
		t.Fatalf("seal failure result = %+v, want fenced writer without receipt", stopped)
	}
	ledgerDown = false
	if stopped, err = p.Stop(ctx, l, i); err != nil || stopped.Receipt == nil {
		t.Fatalf("retried stop did not seal: %+v %v", stopped, err)
	}
}

func TestRevokedStoppingRetryNeedsPositiveNativeQuietObservation(t *testing.T) {
	p, _, e, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.registry.Stop(ctx, local.Reference); err != nil {
		t.Fatal(err)
	}
	var commands []string
	e.onCommand = func(command string) error { commands = append(commands, command); return nil }
	if _, err = p.Stop(ctx, l, i); err == nil {
		t.Fatal("revoked but active writer was accepted as quiet")
	}
	if strings.Join(commands, " ") != "status" {
		t.Fatalf("revoked writer was allowed another turn/control: %v", commands)
	}
}

func TestStoppingRetryReusesDurableQuietHandoffAfterCaptureFailure(t *testing.T) {
	p, _, e, l, data := setupKind(t, harness.NativeClaude)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	failedCapture := false
	var commands []string
	e.onCommand = func(command string) error {
		commands = append(commands, command)
		if command == helperCapture && !failedCapture {
			failedCapture = true
			return errors.New("lost helper response")
		}
		return nil
	}
	if _, err = p.Stop(ctx, l, i); err == nil {
		t.Fatal("expected unknown checkpoint after capture failure")
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Get(ctx, local.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if !local.NativeQuiet || r.Phase != registry.Stopping || r.Receipt != nil {
		t.Fatalf("missing durable quiet handoff: %+v %+v", local, r)
	}
	s, err := p.Stop(ctx, l, i)
	if err != nil || !s.ActualStopped {
		t.Fatalf("same stopped writer recovery: %+v %v", s, err)
	}
	want := "gate quiesce capture-stopped capture-stopped finish"
	if strings.Join(commands, " ") != want {
		t.Fatalf("retry reopened native execution: %v", commands)
	}
}
func approvedRuntime() *api.AgentRuntime {
	security := &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), RunAsNonRoot: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	c := corev1.Container{Name: nativeContainer, Image: "example/native@sha256:" + strings.Repeat("a", 64), SecurityContext: security, Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}
	return &api.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: fixtureRuntimeName}, Spec: api.AgentRuntimeSpec{ClientConfig: api.RuntimeClientConfig{Harness: "codex"}, WorkloadContainers: []string{nativeContainer}, PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{c}, AutomountServiceAccountToken: ptr.To(false), SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532))}}}}}
}
func fixtureArtifact(t *testing.T) ([]byte, checkpoint.Metadata) {
	return fixtureArtifactKind(t, harness.NativeCodex)
}
func fixtureArtifactKind(t *testing.T, kind harness.NativeKind) ([]byte, checkpoint.Metadata) {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + repo, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("Git fixture: %v %s", err, out)
		}
	}
	git("init", "--quiet", "--template=", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "note"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "Fixture")
	home := t.TempDir()
	rollout := `{"type":"session_meta","payload":{"id":"01a126e8-288d-7350-9b52-627e16048343","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"Approved immutable instructions"}]}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture"}}
`
	if kind == harness.NativeCodex {
		state := harness.CodexState{ThreadID: fixtureThread, Path: "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + fixtureThread + ".jsonl", Rollout: []byte(rollout)}
		if err := harness.RestoreCodexState(home, state, fixtureMax); err != nil {
			t.Fatal(err)
		}
	} else {
		userID, assistantID := "12345678-1234-1234-1234-123456789abc", "22345678-1234-1234-1234-123456789abc"
		user, _ := json.Marshal(map[string]any{fixtureTypeKey: "user", "sessionId": fixtureThread,
			"version": "2.1.27", "uuid": userID, "parentUuid": nil,
			"isSidechain": false, "cwd": repo, "message": map[string]any{"role": "user", "content": "Fixture question"}})
		assistant, _ := json.Marshal(map[string]any{fixtureTypeKey: "assistant", "sessionId": fixtureThread,
			"version": "2.1.27", "uuid": assistantID, "parentUuid": userID,
			"isSidechain": false, "cwd": repo, "message": map[string]any{"role": "assistant",
				"content": []map[string]any{{fixtureTypeKey: "text", "text": "Claude fixture complete"}}}})
		path, err := harness.ClaudeProjectPath(repo, fixtureThread)
		if err != nil {
			t.Fatal(err)
		}
		state := harness.ClaudeState{ThreadID: fixtureThread, Workspace: repo, Path: path,
			Transcript: append(append(append(user, '\n'), assistant...), '\n')}
		if err := harness.RestoreClaudeState(home, repo, state, fixtureMax); err != nil {
			t.Fatal(err)
		}
	}
	data, meta, err := checkpoint.CaptureStopped(context.Background(), kind, home, repo, fixtureThread, nil, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	return data, meta
}
func setup(t *testing.T) (*Placement, *admissionClient, *protocolExecutor, session.Launch, []byte) {
	return setupKind(t, harness.NativeCodex)
}
func setupKind(t *testing.T, kind harness.NativeKind) (*Placement, *admissionClient, *protocolExecutor, session.Launch, []byte) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: "approved", Annotations: map[string]string{registry.ManagedTemplateAnnotation: "true"}}, Spec: api.AgentTemplateSpec{Instructions: fixtureInstruction, PolicyRef: api.AgentPolicyReference{Name: "policy"}, RuntimeRef: api.AgentRuntimeReference{Name: fixtureRuntimeName}}}
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: "policy"}, Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{api.CapabilityModelInference}, MaxExecutionDuration: metav1.Duration{Duration: time.Hour}}}
	nativeRuntime := approvedRuntime()
	nativeRuntime.Spec.ClientConfig.Harness = string(kind)
	kube := &admissionClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.AgentRun{}, &corev1.Pod{}).WithObjects(template, policy, nativeRuntime).Build()}
	a := map[string]session.Allowance{string(api.CapabilityModelInference): {Units: session.Limit{Bounded: true, Value: 100}, CostMicros: session.Limit{Bounded: true, Value: 1000}}}
	p, err := Open(Config{Harness: kind, Kube: kube, RESTConfig: &rest.Config{Host: "https://fixture.invalid"}, Namespace: fixtureNamespace, TemplateName: "approved", Principal: "developer", Directory: filepath.Join(t.TempDir(), "profile"), Name: "remote", InstructionDigest: registry.InstructionsDigest(fixtureInstruction), Allowances: a, MaxDuration: time.Hour, MaxCheckpointBytes: fixtureMax})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	data, meta := fixtureArtifactKind(t, kind)
	executor := &protocolExecutor{kube: kube, data: data, meta: meta, state: harness.NativePrepared}
	p.exec = executor
	l := session.Launch{Harness: kind, ExecutionID: uuid.NewString(), AllocationID: uuid.NewString(), OperationID: uuid.NewString(), ThreadID: fixtureThread, Epoch: 2, Placement: "remote", InstructionDigest: p.config.InstructionDigest, Deadline: time.Now().Add(30 * time.Minute).UTC(), Allowances: a, CheckpointDigest: meta.SHA256}
	return p, kube, executor, l, data
}

func TestIgnoredSelectionBindsRemoteVisitAndFixedCapture(t *testing.T) {
	p, _, e, l, data := setup(t)
	l.IncludeIgnored = []string{"needed.bin", "cache/chosen.bin"}
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	changed := l
	changed.IncludeIgnored = []string{"other.bin"}
	if _, err = p.Prepare(t.Context(), changed, data); err == nil {
		t.Fatal("retained remote operation accepted changed ignored selection")
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.Join(e.captureArgs, " "), "--include-ignored needed.bin --include-ignored cache/chosen.bin") {
		t.Fatalf("fixed helper did not receive selected ignored paths: %v", e.captureArgs)
	}
	if strings.Join(stopped.Receipt.Launch.IncludeIgnored, ",") != "needed.bin,cache/chosen.bin" {
		t.Fatal("trusted stop receipt dropped selection")
	}
}

func TestPrepareActivateStopRetainsExactExecutionAndUsage(t *testing.T) {
	p, kube, e, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := p.Prepare(ctx, l, data); err != nil || repeated != i || kube.runCreates != 1 {
		t.Fatalf("prepare replay: %+v %v", repeated, err)
	}
	e.lostActivate = true
	if err = p.Activate(ctx, l, i); err == nil {
		t.Fatal("expected lost activation acknowledgement")
	}
	o, err := p.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementActive || o.Incarnation != i {
		t.Fatalf("lost activation recovery: %+v %v", o, err)
	}
	r, err := p.ledger.Reserve(ctx, budget.AccountKey(i.ExecutionID+"/model.inference"), budget.UsageReservation{RequireExisting: true, MaxUnits: 100, MaxCostMicros: 1000, Units: 5, CostMicros: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ledger.Settle(ctx, r, budget.TrustedUsage{Units: 5, CostMicros: 7}); err != nil {
		t.Fatal(err)
	}
	s, err := p.Stop(ctx, l, i)
	if err != nil || !s.ActualStopped || string(s.Checkpoint) != string(data) || s.Receipt.Usage["model.inference"].Units != 5 || e.finishes != 1 {
		t.Fatalf("trusted stop: %+v %v", s, err)
	}
	if repeated, err := p.Stop(ctx, l, i); err != nil || repeated.CheckpointDigest != s.CheckpointDigest || e.finishes != 1 {
		t.Fatalf("stop replay: %+v %v", repeated, err)
	}
	config := p.config
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	o, err = reopened.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementStopped || o.Stopped.Receipt.Usage["model.inference"].CostMicros != 7 {
		t.Fatalf("retained receipt after reopen: %+v %v", o, err)
	}
}
func TestLostActivationBeforeNativeEffectRecoversSamePreparedIncarnation(t *testing.T) {
	p, kube, e, l, data := setup(t)
	i, err := p.Prepare(context.Background(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	e.lostActivate = true
	e.skipActivate = true
	if p.Activate(context.Background(), l, i) == nil {
		t.Fatal("expected failure")
	}
	o, err := p.Inspect(context.Background(), l)
	if err != nil || o.State != session.PlacementPrepared || o.Incarnation != i {
		t.Fatalf("inert recovery: %+v %v", o, err)
	}
	e.skipActivate = false
	if err = p.Activate(context.Background(), l, i); err != nil || kube.runCreates != 1 {
		t.Fatalf("same activation: %v", err)
	}
}
func TestMissingRunDoesNotProveLivePodStopped(t *testing.T) {
	p, kube, _, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.registry.Cancel(ctx, local.Reference); err != nil {
		t.Fatal(err)
	}
	var run api.AgentRun
	if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: runName(l)}, &run); err != nil {
		t.Fatal(err)
	}
	if err = kube.Delete(ctx, &run); err != nil {
		t.Fatal(err)
	}
	o, err := p.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementUnknown || o.Incarnation != i {
		t.Fatalf("missing Run fabricated physical stop: %+v %v", o, err)
	}
}
func TestAbortUsesExactPodAndSealsKnownZeroUsage(t *testing.T) {
	p, kube, _, l, data := setup(t)
	ctx := context.Background()
	i, err := p.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	o, err := p.Abort(ctx, l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || o.Stopped.Receipt == nil ||
		!o.Stopped.ActualStopped || o.Incarnation != i || o.Stopped.Receipt.Usage["model.inference"] != (budget.TrustedUsage{}) {
		t.Fatalf("forced stop: %+v %v", o, err)
	}
	state, err := p.ledger.State(ctx, budget.AccountKey(i.ExecutionID+"/model.inference"))
	if err != nil || state != (budget.State{}) {
		t.Fatalf("account changed on abort: %+v %v", state, err)
	}
	if _, err = p.ledger.Reserve(ctx, budget.AccountKey(i.ExecutionID+"/model.inference"), budget.UsageReservation{MaxUnits: 100, MaxCostMicros: 1000, Units: 1, CostMicros: 1, RequireExisting: true}); err == nil {
		t.Fatal("cancelled known-zero account admitted new reservation")
	}
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: p.config.AgentNamespace,
		Name: runtimekube.RunName(types.UID(i.ExecutionID)), UID: types.UID(uuid.NewString()),
		Labels: map[string]string{runtimekube.RunUIDLabel: i.ExecutionID, runtimekube.RunNamespaceLabel: p.config.Namespace}}}
	if err = kube.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Inspect(ctx, l); err == nil {
		t.Fatal("retained cancellation receipt replaced exact physical-fence observation")
	}
	if _, err = p.Abort(ctx, l, i); err == nil {
		t.Fatal("known receipt accepted or deleted a replacement Pod")
	}
	var observed corev1.Pod
	if err = kube.Get(ctx, client.ObjectKeyFromObject(replacement), &observed); err != nil || observed.UID != replacement.UID {
		t.Fatalf("receipt replay deleted replacement incarnation: %v", err)
	}
}

func TestCancellationSealsUsedAndOutstandingUsageAndReplaysAfterSettlement(t *testing.T) {
	p, _, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	key := budget.AccountKey(i.ExecutionID + "/model.inference")
	req := budget.UsageReservation{MaxUnits: 100, MaxCostMicros: 1000, Units: 5, CostMicros: 8, RequireExisting: true}
	used, err := p.ledger.Reserve(t.Context(), key, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ledger.Settle(t.Context(), used, budget.TrustedUsage{Units: 3, CostMicros: 5}); err != nil {
		t.Fatal(err)
	}
	req.Units, req.CostMicros = 4, 7
	pending, err := p.ledger.Reserve(t.Context(), key, req)
	if err != nil {
		t.Fatal(err)
	}
	expected := budget.TrustedUsage{Units: 7, CostMicros: 12}
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.Stopped == nil || o.Stopped.Receipt == nil ||
		o.Stopped.Receipt.Usage["model.inference"] != expected || len(o.Stopped.Checkpoint) != 0 || o.Stopped.CheckpointDigest != "" {
		t.Fatalf("conservative cancelled usage: %+v %v", o, err)
	}
	if err = p.ledger.Settle(t.Context(), pending, budget.TrustedUsage{Units: 1, CostMicros: 2}); err != nil {
		t.Fatal(err)
	}
	for _, observe := range []func() (session.Observation, error){
		func() (session.Observation, error) { return p.Abort(t.Context(), l, i) },
		func() (session.Observation, error) { return p.Inspect(t.Context(), l) },
	} {
		o, err = observe()
		if err != nil || o.Stopped == nil || o.Stopped.Receipt == nil || o.Stopped.Receipt.Usage["model.inference"] != expected {
			t.Fatalf("cancelled receipt shrank after settlement: %+v %v", o, err)
		}
	}
	config := p.config
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	o, err = reopened.Inspect(t.Context(), l)
	if err != nil || o.Stopped == nil || o.Stopped.Receipt == nil || o.Stopped.Receipt.Usage["model.inference"] != expected {
		t.Fatalf("reopened cancellation receipt changed: %+v %v", o, err)
	}
}

func TestCancellationMissingOrCorruptAccountRemainsUnknownAfterActualFence(t *testing.T) {
	for _, corruption := range []string{"missing", "corrupt"} {
		t.Run(corruption, func(t *testing.T) {
			p, kube, _, l, data := setup(t)
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			var accounts corev1.ConfigMapList
			if err = kube.List(t.Context(), &accounts, client.InNamespace(p.config.Namespace),
				client.MatchingLabels{"app.kubernetes.io/component": "budget-ledger"}); err != nil || len(accounts.Items) != 1 {
				t.Fatalf("expected initialized account: %v", err)
			}
			account := &accounts.Items[0]
			if corruption == "missing" {
				err = kube.Delete(t.Context(), account)
			} else {
				account.Data["state"] = "corrupt trusted accounting"
				err = kube.Update(t.Context(), account)
			}
			if err != nil {
				t.Fatal(err)
			}
			o, err := p.Abort(t.Context(), l, i)
			if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped || o.Stopped.Receipt != nil {
				t.Fatalf("unknown accounting became known zero or writer fence was lost: %+v %v", o, err)
			}
			local, err := p.load(l)
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.registry.Get(t.Context(), local.Reference)
			if err != nil || r.Phase != registry.Cancelled || r.Receipt != nil {
				t.Fatalf("unknown accounting stored a cancellation receipt: %+v %v", r, err)
			}
			if !local.WriterFenced {
				t.Fatal("unknown accounting erased independently observed writer fence")
			}
			if _, err = p.exactPod(t.Context(), r); !apierrors.IsNotFound(err) {
				t.Fatalf("unknown accounting leaked a known-terminated Pod finalizer: %v", err)
			}
			if _, err = p.ledger.State(t.Context(), budget.AccountKey(i.ExecutionID+"/model.inference")); err == nil {
				t.Fatal("cancellation recreated or repaired missing/corrupt accounting")
			}
		})
	}
}

func TestMissingPodWithoutRetainedTerminationCannotSealOrRefund(t *testing.T) {
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	local, r, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate API object loss without a kubelet termination report. The
	// administrator stripping a finalizer must not manufacture writer evidence.
	pod.Finalizers = nil
	if err = kube.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if err = kube.Client.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if fenced, fenceErr := p.physicalFence(t.Context(), &local, r); fenceErr != nil || fenced {
		t.Fatalf("API absence became actual writer stop: %v %v", fenced, fenceErr)
	}
	o, err := p.Abort(t.Context(), l, i)
	if err == nil || o.State == session.PlacementStopped || o.Stopped != nil {
		t.Fatalf("absent writer refunded: %+v %v", o, err)
	}
	o, err = p.Inspect(t.Context(), l)
	if err != nil || o.State != session.PlacementUnknown || o.Stopped != nil {
		t.Fatalf("absence replay became stopped: %+v %v", o, err)
	}
	local, r, err = p.retained(t.Context(), l)
	if err != nil || local.WriterFenced || r.Receipt != nil {
		t.Fatalf("absence retained fence/receipt: %+v %+v %v", local, r, err)
	}
	if _, err = p.ledger.Reserve(t.Context(), account(r, api.CapabilityModelInference), budget.UsageReservation{
		RequireExisting: true, MaxUnits: 100, MaxCostMicros: 1000, Units: 1, CostMicros: 1,
	}); err != nil {
		t.Fatalf("unknown writer account was sealed: %v", err)
	}
}

func TestDeleteRetainsPodUntilObservedTerminationAndPersistentFence(t *testing.T) {
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	kube.deferTermination = true
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err = p.Abort(ctx, l, i); err == nil {
		t.Fatal("delete without termination became stopped")
	}
	local, r, err := p.retained(t.Context(), l)
	if err != nil || local.WriterFenced || r.Receipt != nil {
		t.Fatalf("early fence or seal: %+v %+v %v", local, r, err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if err != nil || pod.DeletionTimestamp.IsZero() || !running(pod, nativeContainer) ||
		!slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
		t.Fatalf("delete lost writer observation: %+v %v", pod, err)
	}
	pod.Status.ContainerStatuses[0].State = fixtureTermination(pod.Status.ContainerStatuses[0].ContainerID, 137)
	if err = kube.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || o.Stopped.Receipt == nil {
		t.Fatalf("terminated writer not reconciled: %+v %v", o, err)
	}
	local, _, err = p.retained(t.Context(), l)
	if err != nil || !local.WriterFenced {
		t.Fatalf("physical evidence not persisted: %+v %v", local, err)
	}
	if _, err = p.exactPod(t.Context(), r); !apierrors.IsNotFound(err) {
		t.Fatalf("known termination finalizer leaked: %v", err)
	}
	config := p.config
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	o, err = reopened.Inspect(t.Context(), l)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || o.Stopped.Receipt == nil {
		t.Fatalf("retained terminated fence lost on reopen: %+v %v", o, err)
	}
}

func TestGracefulStopReleasesFenceOnlyAfterCheckpointAndReceipt(t *testing.T) {
	p, kube, e, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	e.onCommand = func(command string) error {
		if command != helperFinish {
			return nil
		}
		local, r, err := p.retained(t.Context(), l)
		if err != nil {
			return err
		}
		pod, err := p.exactPod(t.Context(), r)
		if err != nil {
			return err
		}
		if !local.WriterFenced || r.Receipt == nil || local.OutputDigest == "" ||
			!slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
			return errors.New("helper finished before retained fence/checkpoint/receipt")
		}
		return nil
	}
	if _, err = p.Stop(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	local, r, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if err != nil || slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
		t.Fatalf("normal stop retained finalizer: %+v %v", pod, err)
	}
	if err = kube.Client.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Stop(t.Context(), l, i); err != nil {
		t.Fatalf("graceful removed-Pod replay lost retained fence: %v", err)
	}
	if !local.WriterFenced {
		t.Fatal("graceful fence was not retained")
	}
}

func TestIncompleteRestorePinsPodAndRecoversThroughExplicitPrepare(t *testing.T) {
	p, kube, e, l, data := setup(t)
	commands := 0
	e.onCommand = func(string) error {
		commands++
		return errors.New("restore helper rejected owned directory permissions")
	}
	if _, err := p.Prepare(t.Context(), l, data); err == nil {
		t.Fatal("helper restore failure hidden")
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Get(t.Context(), local.Reference)
	if err != nil || r.Phase != registry.Bound || r.PodUID == "" {
		t.Fatalf("failed helper did not retain exact inert Pod: %+v %v", r, err)
	}
	before := commands
	o, err := p.Inspect(t.Context(), l)
	if err != nil || o.State != session.PlacementPreparing || o.Incarnation != inc(r) || commands != before {
		t.Fatalf("inspection executed restore or admitted failed preparation: %+v %v", o, err)
	}
	if err = p.Activate(t.Context(), l, o.Incarnation); err == nil || commands != before {
		t.Fatal("incomplete restoration admitted native activation")
	}
	if _, err = p.ledger.State(t.Context(), account(r, api.CapabilityModelInference)); !errors.Is(err, budget.ErrAccountNotFound) {
		t.Fatalf("failed restore or observation initialized authority account: %v", err)
	}
	e.onCommand = nil
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil || i != o.Incarnation {
		t.Fatalf("explicit restore retry changed resources: %+v %v", i, err)
	}
	var run api.AgentRun
	key := client.ObjectKey{Namespace: p.config.Namespace, Name: runName(l)}
	if err = kube.Get(t.Context(), key, &run); err != nil || string(run.UID) != i.ExecutionID {
		t.Fatalf("explicit restore retry replaced exact Run: %v", err)
	}
	r, err = p.registry.Get(t.Context(), local.Reference)
	if err != nil || r.Phase != registry.Prepared {
		t.Fatalf("verified restore failed to become prepared: %+v %v", r, err)
	}
}

func TestOldPartialPreparationObservesAndCancelsWithoutHelperOrAdmission(t *testing.T) {
	p, kube, e, l, data := setup(t)
	local, err := p.persistIntent(l, data)
	if err != nil {
		t.Fatal(err)
	}
	ceilings, err := p.liveProfile(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Create(t.Context(), p.registration(l, ceilings))
	if err != nil {
		t.Fatal(err)
	}
	local.Reference = r.Reference
	if err = p.save(local); err != nil {
		t.Fatal(err)
	}
	run, err := p.ensureRun(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = p.registry.BindRun(t.Context(), r.Reference, string(run.UID)); err != nil {
		t.Fatal(err)
	}
	if r.PodUID != "" {
		t.Fatal("old preparation fixture already observed Pod UID")
	}
	commands := 0
	e.onCommand = func(string) error {
		commands++
		return errors.New("old failed helper cannot prepare")
	}
	o, err := p.Inspect(t.Context(), l)
	if err != nil || o.State != session.PlacementPreparing || o.Incarnation.ResourceUID == "" || commands != 0 {
		t.Fatalf("partial preparation observation required helper success: %+v %v", o, err)
	}
	r, err = p.registry.Get(t.Context(), local.Reference)
	if err != nil || r.Phase != registry.Bound || inc(r) != o.Incarnation {
		t.Fatalf("observed partial preparation became admitted: %+v %v", r, err)
	}
	stopped, err := p.Abort(t.Context(), l, o.Incarnation)
	if err != nil || stopped.State != session.PlacementStopped || stopped.Stopped == nil ||
		stopped.Stopped.Receipt != nil || !stopped.Stopped.ActualStopped || commands != 0 {
		t.Fatalf("partial preparation cancellation required helper or invented usage: %+v %v", stopped, err)
	}
	if err = kube.Get(t.Context(), client.ObjectKeyFromObject(run), run); err != nil || !run.Spec.Cancel {
		t.Fatalf("exact Run cancellation not retained: %v", err)
	}
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: p.config.AgentNamespace, Name: runtimekube.RunName(run.UID)}
	if err = kube.Get(t.Context(), key, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("observed Pod was not physically fenced: %v", err)
	}
	if _, err = p.ledger.State(t.Context(), account(r, api.CapabilityModelInference)); !errors.Is(err, budget.ErrAccountNotFound) {
		t.Fatalf("failed preparation usage silently became initialized zero: %v", err)
	}
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name,
		UID: types.UID(uuid.NewString()), Labels: map[string]string{runtimekube.RunUIDLabel: string(run.UID),
			runtimekube.RunNamespaceLabel: p.config.Namespace}}}
	if err = kube.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Abort(t.Context(), l, o.Incarnation); err == nil {
		t.Fatal("replacement Pod accepted as stopped original incarnation")
	}
	if err = kube.Get(t.Context(), key, &pod); err != nil || pod.UID != replacement.UID {
		t.Fatalf("cancellation deleted replacement Pod: %v", err)
	}
}
func TestRemoteDiskLossAndEpochReplayDoNotCreateFreshAuthority(t *testing.T) {
	p, _, _, l, data := setup(t)
	ctx := context.Background()
	if _, err := p.Prepare(ctx, l, data); err != nil {
		t.Fatal(err)
	}
	other := l
	other.AllocationID = uuid.NewString()
	other.OperationID = uuid.NewString()
	if _, err := p.Prepare(ctx, other, data); err == nil {
		t.Fatal("same epoch accepted different allocation")
	}
	if err := os.Remove(filepath.Join(p.config.Directory, l.AllocationID+recordSuffix)); err != nil {
		t.Fatal(err)
	}
	o, err := p.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementUnknown {
		t.Fatalf("local disk loss fabricated absence: %+v %v", o, err)
	}
	if _, err = Open(p.config); err == nil {
		t.Fatal("second profile owner accepted")
	}
}

func TestClaudePlacementRetainsKindPinAndUsesClaudeCheckpointHome(t *testing.T) {
	p, _, e, l, data := setupKind(t, harness.NativeClaude)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	_, registered, err := p.retained(t.Context(), l)
	if err != nil || registered.Harness != harness.NativeClaude {
		t.Fatalf("wrong native registration: %+v %v", registered, err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || !stopped.ActualStopped || stopped.Receipt == nil {
		t.Fatalf("Claude stop: %+v %v", stopped, err)
	}
	meta, err := checkpoint.Inspect(stopped.Checkpoint, fixtureMax)
	if err != nil || meta.Harness != harness.NativeClaude {
		t.Fatalf("wrong artifact harness: %+v %v", meta, err)
	}
	flags := map[string]string{}
	for index := 2; index+1 < len(e.captureArgs); index += 2 {
		flags[e.captureArgs[index]] = e.captureArgs[index+1]
	}
	if flags["--harness"] != "claude-code" || flags["--home"] != "/workspace/session/claude" {
		t.Fatalf("wrong native capture selection: %v", e.captureArgs)
	}
	changed := l
	changed.Harness = harness.NativeCodex
	if _, err = p.Inspect(t.Context(), changed); err == nil {
		t.Fatal("retained Claude intent accepted changed native kind")
	}
}

func TestClaudeCannotRecoverUnacknowledgedQuiescenceFromTerminatedContainer(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "already-terminated"
		if lostReply {
			name = "lost-reply"
		}
		t.Run(name, func(t *testing.T) {
			p, kube, e, l, data := setupKind(t, harness.NativeClaude)
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			if lostReply {
				e.lostQuiesce = true
			} else {
				_, r, retainedErr := p.retained(t.Context(), l)
				if retainedErr != nil {
					t.Fatal(retainedErr)
				}
				pod, podErr := p.pod(t.Context(), r)
				if podErr != nil {
					t.Fatal(podErr)
				}
				pod.Status.ContainerStatuses[0].State = fixtureTermination(pod.Status.ContainerStatuses[0].ContainerID, 0)
				if err = kube.Status().Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if _, err = p.Stop(t.Context(), l, i); err == nil {
					t.Fatal("physical termination substituted for Claude completion")
				}
			}
			local, r, err := p.retained(t.Context(), l)
			if err != nil || local.NativeQuiet || local.OutputDigest != "" || r.Receipt != nil || len(e.captureArgs) != 0 {
				t.Fatalf("unacknowledged turn captured or sealed: %+v %+v %v", local, r, err)
			}
			o, err := p.Abort(t.Context(), l, i)
			if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || o.Stopped.Receipt == nil {
				t.Fatalf("forced cancellation unavailable: %+v %v", o, err)
			}
		})
	}
}

func TestPlacementRejectsEmptyMismatchedHarnessAndForeignCheckpoint(t *testing.T) {
	for _, kind := range []harness.NativeKind{"", harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) {
			p, kube, _, l, data := setup(t)
			l.Harness = kind
			if _, err := p.Prepare(t.Context(), l, data); err == nil {
				t.Fatal("mismatched harness admitted")
			}
			if kube.runCreates != 0 {
				t.Fatal("mismatched kind created a Run")
			}
		})
	}
	p, kube, _, l, _ := setupKind(t, harness.NativeClaude)
	codex, meta := fixtureArtifact(t)
	l.CheckpointDigest = meta.SHA256
	if _, err := p.Prepare(t.Context(), l, codex); err == nil {
		t.Fatal("Claude profile accepted Codex checkpoint")
	}
	if kube.runCreates != 0 {
		t.Fatal("foreign checkpoint created a Run")
	}
	var nativeRuntime api.AgentRuntime
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: fixtureNamespace, Name: fixtureRuntimeName}, &nativeRuntime); err != nil {
		t.Fatal(err)
	}
	nativeRuntime.Spec.ClientConfig.Harness = "codex"
	if err := kube.Update(t.Context(), &nativeRuntime); err != nil {
		t.Fatal(err)
	}
	if _, err := p.liveProfile(t.Context(), l); err == nil {
		t.Fatal("changed runtime kind admitted")
	}
}
