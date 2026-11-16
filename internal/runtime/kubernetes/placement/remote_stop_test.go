package placement

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const remoteStopToken = "remote-stop-home-principal-token-32-bytes"

// The stages of a graceful Kubernetes stop at which home can go away.
const (
	// stageTurn: the supervisor is waiting for the turn to complete. It
	// finishes on its own, quits the harness and exits, whatever happened to
	// the request that asked it to.
	stageTurn = "turn"
	// stagePod: the supervisor answered, and the placement waits for the
	// native container to end.
	stagePod = "pod"
	// stageCapture: the writer is fenced and the placement captures the
	// checkpoint.
	stageCapture = "capture"
)

type remoteStopAuthorizer struct{}

func (remoteStopAuthorizer) Authorize(_ context.Context, token string) error {
	if token != remoteStopToken {
		return profile.ErrUnauthenticated
	}
	return nil
}

// stageExecutor holds one stage of a stop until the test releases it, and
// counts captures. Everything else is the fixture supervisor and helper.
type stageExecutor struct {
	*protocolExecutor
	stage   string
	entered chan struct{}
	release chan struct{}

	mu       sync.Mutex
	captures int
}

func (e *stageExecutor) Execute(ctx context.Context, namespace, pod, container string, args []string, input io.Reader, output, errout io.Writer) error {
	command := args[1]
	if container == runtimekube.CaptureContainer && command == helperCapture {
		e.mu.Lock()
		e.captures++
		e.mu.Unlock()
		if e.stage == stageCapture {
			close(e.entered)
			<-e.release
			e.stage = ""
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	if container == nativeContainer && command == string(harness.NativeQuiesce) {
		switch e.stage {
		case stageTurn:
			e.stage = ""
			close(e.entered)
			<-e.release
			// The supervisor's quiesce does not depend on the exec stream:
			// it completes the turn, quits the harness and its container
			// ends. A reply that no one reads any more is lost.
			err := e.protocolExecutor.Execute(context.WithoutCancel(ctx), namespace, pod, container, args, input, output, errout)
			return errors.Join(ctx.Err(), err)
		case stagePod:
			e.stage = ""
			// The supervisor answers; its container ends a little later.
			e.state = harness.NativeStopped
			defer close(e.entered)
			return json.NewEncoder(output).Encode(harness.NativeReply{Op: harness.NativeQuiesce, State: harness.NativeStopped, ThreadID: fixtureThread})
		}
	}
	return e.protocolExecutor.Execute(ctx, namespace, pod, container, args, input, output, errout)
}

func (e *stageExecutor) captured() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.captures
}

// remoteStopHome serves p through the executor's placement protocol over TLS
// 1.3 and returns a home coordinator whose one placement is the protocol
// client, as home reaches an executor. ended receives once for each stop
// request whose context the executor saw end.
func remoteStopHome(t *testing.T, p *Placement, l session.Launch) (*session.Coordinator, <-chan struct{}) {
	t.Helper()
	h, err := profile.NewHandler(profile.Config{Harness: l.Harness, Principal: "developer", Authorizer: remoteStopAuthorizer{},
		Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour,
		MaxCheckpointBytes: fixtureMax, Backend: p})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{}, 16)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/stop" {
			go func() {
				<-r.Context().Done()
				ended <- struct{}{}
			}()
		}
		h.ServeHTTP(w, r)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	c, err := profile.Connect(profile.Connection{Origin: server.URL, Token: remoteStopToken, Roots: roots, MaxCheckpointBytes: fixtureMax})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: c}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return coordinator, ended
}

// remoteVisit starts a resumed visit of kind on the fixture cluster through
// home's coordinator. Its capture returns work, a checkpoint other than the
// one the visit started from.
func remoteVisit(t *testing.T, kind harness.NativeKind, stage string) (*Placement, *stageExecutor, *session.Coordinator, <-chan struct{}, session.Launch, string) {
	t.Helper()
	p, _, inner, l, data := setupKind(t, kind)
	e := &stageExecutor{protocolExecutor: inner, stage: stage, entered: make(chan struct{}), release: make(chan struct{})}
	p.exec = e
	l.Epoch = 1
	coordinator, ended := remoteStopHome(t, p, l)
	if _, err := coordinator.Create(l, data, ""); err != nil {
		t.Fatal(err)
	}
	if r, err := coordinator.Recover(t.Context(), l.ExecutionID); err != nil || r.Phase != session.PhaseActive {
		t.Fatalf("remote visit: %s %v", r.Phase, err)
	}
	work, meta := cachedFixtureArtifact(t, kind, "work of this visit\n")
	if meta.SHA256 == l.CheckpointDigest {
		t.Fatal("the visit's work is the checkpoint it started from")
	}
	inner.data = work
	return p, e, coordinator, ended, l, meta.SHA256
}

// awaitRemote waits for a signal the test needs before it goes on.
func awaitRemote(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// keptVisit checks that a stopped execution kept the checkpoint its visit's
// graceful stop captured, and nothing that an exit or a kept input would
// leave.
func keptVisit(t *testing.T, r session.Record, want string, e *stageExecutor) {
	t.Helper()
	if r.Phase != session.PhaseIdle || r.PendingOperation != "" || len(r.Visits) != 1 {
		t.Fatalf("stopped execution: phase %s, pending %q, %d visits", r.Phase, r.PendingOperation, len(r.Visits))
	}
	visit := r.Visits[0]
	if visit.Exit != nil || !visit.ActualStopped || visit.Receipt == nil || visit.CheckpointDigest != want ||
		r.Checkpoint.Metadata.SHA256 != want || r.ThreadID != fixtureThread {
		t.Fatalf("stopped visit kept %s with exit %+v, want the captured %s and no exit", visit.CheckpointDigest, visit.Exit, want)
	}
	if got := e.captured(); got != 1 && got != 2 {
		t.Fatalf("checkpoint captured %d times", got)
	}
}

// A remote stop interrupted at any stage, because home went away or its
// request timed out, ends with the same checkpoint as one that was not, for a
// harness whose state proves completion (Codex) and for one whose state does
// not (Claude Code). The executor carries the stop on to a recorded outcome,
// and home's retry of the same operation picks it up.
func TestRemoteStopInterruptedAtEachStageKeepsTheVisitsCheckpoint(t *testing.T) {
	t.Parallel()
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind)+"/uninterrupted", func(t *testing.T) {
			t.Parallel()
			_, e, coordinator, _, l, work := remoteVisit(t, kind, "")
			r, err := coordinator.Finalize(t.Context(), l.ExecutionID, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			keptVisit(t, r, work, e)
		})
		for _, stage := range []string{stageTurn, stagePod, stageCapture} {
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				t.Parallel()
				p, e, coordinator, ended, l, work := remoteVisit(t, kind, stage)
				ctx, leave := context.WithCancel(t.Context())
				defer leave()
				op := uuid.NewString()
				first := make(chan error, 1)
				go func() {
					_, err := coordinator.Finalize(ctx, l.ExecutionID, op)
					first <- err
				}()
				awaitRemote(t, e.entered, "the "+stage+" stage")
				if stage == stagePod {
					awaitQuietStop(t, p, l)
				}
				leave()
				if err := <-first; err == nil {
					t.Fatal("a stop home left was reported complete")
				}
				awaitRemote(t, ended, "the executor noticing that home left")
				if r, err := coordinator.Inspect(l.ExecutionID); err != nil || r.Phase != session.PhaseUnknownStop {
					t.Fatalf("home's journal after it left: %s %v", r.Phase, err)
				}
				switch stage {
				case stagePod:
					endNativeContainer(t, p, l)
				default:
					close(e.release)
				}
				r, err := coordinator.Finalize(t.Context(), l.ExecutionID, op)
				if err != nil {
					t.Fatal(err)
				}
				keptVisit(t, r, work, e)
			})
		}
	}
}

// A stop whose native container does not end after the supervisor's
// graceful handoff gives up at podStopWait, even without a deadline, and
// stays unproven: nothing is fenced, sealed or captured. A retry waits for the
// container again, and once it ends the stop completes. Not parallel: it
// shortens podStopWait for the package.
func TestStopWhoseContainerNeverEndsIsUnprovenAtTheBound(t *testing.T) {
	previous := podStopWait
	podStopWait = 200 * time.Millisecond
	t.Cleanup(func() { podStopWait = previous })
	// The launch's deadline is 30 minutes away, so only the bound ends the wait.
	p, _, inner, l, data := setup(t)
	e :=&stageExecutor{protocolExecutor: inner, stage: stagePod, entered: make(chan struct{}), release: make(chan struct{})}
	p.exec = e
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	sealed := 0
	p.ledger = &observedLedger{Ledger: p.ledger, beforeSeal: func(context.Context, budget.AccountKey) error { sealed++; return nil }}
	for attempt := range 2 {
		// The caller would wait 3 seconds; the bound must end it first.
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		started := time.Now()
		_, err = p.Stop(ctx, l, i)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d: stop of a container that never ends: %v", attempt, err)
		}
		if waited := time.Since(started); waited < podStopWait || waited > 2*time.Second {
			t.Fatalf("attempt %d waited %s for a bound of %s", attempt, waited, podStopWait)
		}
		local, err := p.load(l)
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.registry.Get(t.Context(), local.Reference)
		if err != nil {
			t.Fatal(err)
		}
		if !local.NativeQuiet || local.WriterFenced || local.OutputDigest != "" || r.Phase != registry.Stopping ||
			r.Receipt != nil || sealed != 0 || e.captured() != 0 {
			t.Fatalf("attempt %d proved too much: quiet %v fenced %v output %q phase %s seals %d captures %d", attempt,
				local.NativeQuiet, local.WriterFenced, local.OutputDigest, r.Phase, sealed, e.captured())
		}
	}
	endNativeContainer(t, p, l)
	s, err := p.Stop(t.Context(), l, i)
	if err != nil || s.Receipt == nil || sealed != 1 || e.captured() != 1 {
		t.Fatalf("stop once the container ended: %+v %v seals %d captures %d", s, err, sealed, e.captured())
	}
}

// awaitQuietStop waits until the placement has recorded the graceful handoff
// and revoked the visit's grant, so it now waits for the container.
func awaitQuietStop(t *testing.T, p *Placement, l session.Launch) {
	t.Helper()
	for range 500 {
		local, err := p.load(l)
		if err == nil && local.NativeQuiet {
			r, err := p.registry.Get(t.Context(), local.Reference)
			if err == nil && r.Phase == registry.Stopping {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the placement did not record the quiet handoff")
}

// endNativeContainer ends the visit's native container, as the supervisor's
// exit does after a graceful stop.
func endNativeContainer(t *testing.T, p *Placement, l session.Launch) {
	t.Helper()
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Get(t.Context(), local.Reference)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	var current corev1.Pod
	if err = p.config.Kube.Get(t.Context(), client.ObjectKeyFromObject(pod), &current); err != nil {
		t.Fatal(err)
	}
	current.Status.ContainerStatuses[0].State = fixtureTermination(current.Status.ContainerStatuses[0].ContainerID, 0)
	if err = p.config.Kube.Status().Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
}
