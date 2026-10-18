package docker_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/harness"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	"github.com/andrewmccall/sproozi/internal/gateway"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
)

const allocationID = "allocation-1"
const modelCapability = "model.inference"
const observedNativeUUID = "12345678-1234-1234-1234-123456789abc"
const selectedIgnoredFile = "needed.bin"

type observation struct {
	state    runtimedocker.Observation
	err      error
	blocked  chan struct{}
	observed chan struct{}
}

func (o *observation) Observe(ctx context.Context, _ runtimedocker.Instance) (runtimedocker.Observation, error) {
	if o.blocked != nil {
		close(o.observed)
		select {
		case <-o.blocked:
		case <-ctx.Done():
			return runtimedocker.Observation{}, ctx.Err()
		}
	}
	return o.state, o.err
}

type fixture struct {
	owner    *gatewaydocker.Owner
	ledger   *budgetfile.Ledger
	config   gatewaydocker.Config
	launch   session.Launch
	instance session.Incarnation
	observer *observation
}

func newOwner(t *testing.T) *fixture {
	t.Helper()
	dir := testutil.PrivateDir(t)
	ledger, err := budgetfile.NewLedger(filepath.Join(dir, "accounting"))
	if err != nil {
		t.Fatal(err)
	}
	instance := session.Incarnation{ExecutionID: allocationID, ResourceUID: strings.Repeat("a", 64)}
	observer := &observation{state: runtimedocker.Observation{Instance: runtimedocker.Instance{ID: instance.ResourceUID, Execution: instance.ExecutionID}, Running: true, Status: "running", PID: 42}}
	instructions := "trusted local instructions"
	config := gatewaydocker.Config{Directory: filepath.Join(dir, "grants"), Ledger: ledger, Observer: observer,
		Policy:   gateway.PolicySnapshot{Name: "local-policy", Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{api.CapabilityModelInference}, Budgets: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 10, MaxCostMicros: 100}}}},
		Template: gateway.TemplateSnapshot{Name: "codex-mobile", Spec: api.AgentTemplateSpec{Instructions: instructions}}}
	owner, err := gatewaydocker.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{owner: owner, ledger: ledger, config: config, observer: observer, instance: instance,
		launch: session.Launch{Harness: harness.NativeCodex, ExecutionID: "session-1", OperationID: "move-1", AllocationID: allocationID, Epoch: 1, Placement: "local", ThreadID: observedNativeUUID, InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(instructions))), Deadline: time.Now().Add(time.Hour), CheckpointDigest: strings.Repeat("b", 64), Allowances: map[string]session.Allowance{modelCapability: {Units: session.Limit{Bounded: true, Value: 5}, CostMicros: session.Limit{Bounded: true, Value: 50}}}}}
	t.Cleanup(func() { _ = f.owner.Close(); _ = f.ledger.Close() })
	return f
}

func (f *fixture) initialized(t *testing.T) string {
	t.Helper()
	token, err := f.owner.Initialize(t.Context(), f.launch, f.instance)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestLocalOwnerActivationReservationRevocationAndReceipt(t *testing.T) {
	f := newOwner(t)
	token := f.initialized(t)
	if _, err := f.owner.Authenticate(t.Context(), token); err == nil {
		t.Fatal("inactive allocation authenticated")
	}
	if retry, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err != nil || retry != token {
		t.Fatalf("initialize retry changed token: equal=%v err=%v", retry == token, err)
	}
	if err := f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	grant, err := f.owner.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Execution.Backend != gateway.BackendDocker || grant.Execution.UID != f.instance.ResourceUID {
		t.Fatal("grant did not bind actual Docker incarnation")
	}
	meter, err := grant.Meter(t.Context(), budget.NewTrackerWithLedger(f.ledger), api.CapabilityModelInference)
	if err != nil {
		t.Fatal(err)
	}
	r, err := meter.Reserve(t.Context(), 4, 40)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Authenticate(t.Context(), token); err == nil {
		t.Fatal("revoked token authenticated")
	}
	if err = f.owner.Revalidate(t.Context(), grant); !errors.Is(err, gateway.ErrGrantRevoked) {
		t.Fatalf("revoked grant remained valid: %v", err)
	}
	receipt, err := f.owner.Seal(t.Context(), f.launch, f.instance)
	if err != nil || receipt[modelCapability] != (budget.TrustedUsage{Units: 4, CostMicros: 40}) {
		t.Fatalf("conservative seal: %+v %v", receipt, err)
	}
	if err = r.Settle(2, 20); err != nil {
		t.Fatal(err)
	}
	retry, err := f.owner.Seal(t.Context(), f.launch, f.instance)
	if err != nil || retry[modelCapability] != receipt[modelCapability] {
		t.Fatalf("receipt changed after settlement: %+v %v", retry, err)
	}
	if err = f.owner.Activate(t.Context(), f.launch, f.instance); err == nil {
		t.Fatal("revoked execution reopened")
	}
	if _, err = meter.Reserve(t.Context(), 1, 1); err == nil {
		t.Fatal("sealed account admitted usage")
	}
}

func TestLocalOwnerRestartRetainsGrantAndAccounting(t *testing.T) {
	f := newOwner(t)
	token := f.initialized(t)
	if err := f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	grant, err := f.owner.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	meter, err := grant.Meter(t.Context(), budget.NewTrackerWithLedger(f.ledger), api.CapabilityModelInference)
	if err != nil {
		t.Fatal(err)
	}
	if err = meter.Spend(t.Context(), 2, 20); err != nil {
		t.Fatal(err)
	}
	if err = f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.ledger.Close(); err != nil {
		t.Fatal(err)
	}
	f.ledger, err = budgetfile.NewLedger(filepath.Dir(f.config.Directory) + "/accounting")
	if err != nil {
		t.Fatal(err)
	}
	f.config.Ledger = f.ledger
	f.owner, err = gatewaydocker.Open(f.config)
	if err != nil {
		t.Fatal(err)
	}
	grant, err = f.owner.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	meter, err = grant.Meter(t.Context(), budget.NewTrackerWithLedger(f.ledger), api.CapabilityModelInference)
	if err != nil {
		t.Fatal(err)
	}
	state, err := meter.State(t.Context())
	if err != nil || state.UnitsUsed != 2 || state.CostMicros != 20 {
		t.Fatalf("restart lost usage: %+v %v", state, err)
	}
}

func TestLocalOwnerDeniesChangedOrUnavailableExecution(t *testing.T) {
	for _, name := range []string{"wrong instance", "expired deadline", "exhausted allowance", "changed instructions", "observer unavailable", "stopped worker"} {
		t.Run(name, func(t *testing.T) {
			f := newOwner(t)
			switch name {
			case "wrong instance":
				f.observer.state.ID = strings.Repeat("c", 64)
			case "expired deadline":
				f.launch.Deadline = time.Now().Add(-time.Second)
			case "exhausted allowance":
				a := f.launch.Allowances[modelCapability]
				a.Units.Value = 0
				f.launch.Allowances[modelCapability] = a
			case "changed instructions":
				f.launch.InstructionDigest = strings.Repeat("c", 64)
			case "observer unavailable":
				f.observer.err = errors.New("engine unreachable")
			case "stopped worker":
				f.observer.state.Running = false
				f.observer.state.Status = "exited"
				f.observer.state.PID = 0
			}
			if _, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err == nil {
				t.Fatal("invalid launch initialized")
			}
		})
	}
}

func TestLocalOwnerRecordLossCannotResetAnAllocation(t *testing.T) {
	for _, name := range []string{"registry", "ledger"} {
		t.Run(name, func(t *testing.T) {
			f := newOwner(t)
			token := f.initialized(t)
			if err := f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
				t.Fatal(err)
			}
			grant, err := f.owner.Authenticate(t.Context(), token)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.config.Directory, allocationID+".json")
			if name == "ledger" {
				paths, err := filepath.Glob(filepath.Join(filepath.Dir(f.config.Directory), "accounting", "account-*.json"))
				if err != nil || len(paths) != 1 {
					t.Fatal("missing test ledger")
				}
				path = paths[0]
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err = f.owner.Initialize(t.Context(), f.launch, f.instance); err == nil {
				t.Fatal("lost allocation reset by initialization retry")
			}
			if name == "registry" {
				if _, err = f.owner.Authenticate(t.Context(), token); err == nil {
					t.Fatal("deleted registry authenticated")
				}
			} else if _, err = grant.Meter(t.Context(), budget.NewTrackerWithLedger(f.ledger), api.CapabilityModelInference); err == nil {
				t.Fatal("deleted ledger authorized")
			}
		})
	}
}

func TestLocalOwnerRejectsSecondWriterAndChangedRetry(t *testing.T) {
	f := newOwner(t)
	f.initialized(t)
	if owner, err := gatewaydocker.Open(f.config); err == nil {
		_ = owner.Close()
		t.Fatal("second owner opened registry")
	}
	changed := f.launch
	changed.OperationID = "different-operation"
	if _, err := f.owner.Initialize(t.Context(), changed, f.instance); err == nil {
		t.Fatal("changed retry replaced trusted intent")
	}
}

func TestLocalOwnerEmptyIgnoredSelectionRetriesAsNoSelection(t *testing.T) {
	f := newOwner(t)
	f.launch.IncludeIgnored = []string{}
	token := f.initialized(t)
	f.launch.IncludeIgnored = nil
	if retry, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err != nil || retry != token {
		t.Fatalf("empty selection retry: same token=%v err=%v", retry == token, err)
	}
	f.launch.IncludeIgnored = []string{}
	if err := f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	f.launch.IncludeIgnored = nil
	if err := f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Seal(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
}

func TestInitialOwnerBindsNativeWithEquivalentEmptyIgnoredSelection(t *testing.T) {
	f := newOwner(t)
	f.launch.ThreadID, f.launch.CheckpointDigest = "", ""
	f.initialized(t)
	f.launch.IncludeIgnored = []string{}
	bound, err := f.owner.BindNative(t.Context(), f.launch, f.instance, observedNativeUUID)
	if err != nil || bound != observedNativeUUID {
		t.Fatalf("empty ignored native binding: %q %v", bound, err)
	}
	if err = f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
}

func TestLocalOwnerIgnoredSelectionCannotChangeOnRetryOrNativeBinding(t *testing.T) {
	f := newOwner(t)
	f.launch.ThreadID, f.launch.CheckpointDigest = "", ""
	f.launch.IncludeIgnored = []string{selectedIgnoredFile}
	token := f.initialized(t)
	f.launch.IncludeIgnored[0] = "changed.bin"
	if _, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err == nil {
		t.Fatal("initialization changed ignored selection")
	}
	if err := f.owner.Activate(t.Context(), f.launch, f.instance); err == nil {
		t.Fatal("activation changed ignored selection")
	}
	actual := observedNativeUUID
	if _, err := f.owner.BindNative(t.Context(), f.launch, f.instance, actual); err == nil {
		t.Fatal("native binding changed ignored selection")
	}
	f.launch.IncludeIgnored[0] = selectedIgnoredFile
	if retry, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err != nil || retry != token {
		t.Fatalf("original ignored selection lost: equal=%v err=%v", retry == token, err)
	}
	bound, err := f.owner.BindNative(t.Context(), f.launch, f.instance, actual)
	if err != nil || bound != actual {
		t.Fatalf("observed native binding with original ignored selection: %q %v", bound, err)
	}
}

func TestLocalOwnerRejectsInvalidIgnoredSelection(t *testing.T) {
	f := newOwner(t)
	f.launch.IncludeIgnored = []string{"../outside"}
	if _, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err == nil {
		t.Fatal("unsafe ignored selection initialized")
	}
}

func TestInitialAllocationBindsOneObservedNativeUUID(t *testing.T) {
	f := newOwner(t)
	f.launch.ThreadID = ""
	f.launch.CheckpointDigest = ""
	original := f.launch
	token := f.initialized(t)
	if err := f.owner.Activate(t.Context(), original, f.instance); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("initial real process could not make first request: %v", err)
	}
	actual := observedNativeUUID
	bound, err := f.owner.BindNative(t.Context(), original, f.instance, actual)
	if err != nil || bound != actual {
		t.Fatalf("bind: %q %v", bound, err)
	}
	retry, err := f.owner.BindNative(t.Context(), original, f.instance, actual)
	if err != nil || retry != actual {
		t.Fatalf("bind retry: %q %v", retry, err)
	}
	if _, err = f.owner.BindNative(t.Context(), original, f.instance, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"); err == nil {
		t.Fatal("native UUID rebound")
	}
	if err = f.owner.Revoke(t.Context(), original, f.instance); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Seal(t.Context(), original, f.instance); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedEngineObservationCannotDelayRevocation(t *testing.T) {
	f := newOwner(t)
	token := f.initialized(t)
	if err := f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	f.observer.blocked = make(chan struct{})
	f.observer.observed = make(chan struct{})
	authenticated := make(chan error, 1)
	go func() { _, err := f.owner.Authenticate(t.Context(), token); authenticated <- err }()
	<-f.observer.observed
	revoked := make(chan error, 1)
	go func() { revoked <- f.owner.Revoke(t.Context(), f.launch, f.instance) }()
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(f.observer.blocked)
		t.Fatal("engine observation held the authority fence")
	}
	close(f.observer.blocked)
	if err := <-authenticated; err == nil {
		t.Fatal("stale observation authenticated after revocation")
	}
}
