package kubernetes_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/andrewmccall/sproozi/internal/budget"
	budgetkubernetes "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
)

func observedLedgerState(t *testing.T, ledger budget.Ledger, ctx context.Context, key budget.AccountKey) budget.State {
	t.Helper()
	state, err := ledger.State(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestKubernetesLedgerSurvivesNewInstance(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	l1 := budgetkubernetes.NewLedger(c, "ledger")
	r, err := l1.Reserve(context.Background(), "run-restart", budget.UsageReservation{MaxUnits: 100, Units: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Settle(context.Background(), r, budget.TrustedUsage{Units: 60}); err != nil {
		t.Fatal(err)
	}
	l2 := budgetkubernetes.NewLedger(c, "ledger")
	if got := observedLedgerState(t, l2, context.Background(), "run-restart").UnitsUsed; got != 60 {
		t.Fatalf("restarted ledger usage = %d, want 60", got)
	}
}

type failedLedgerClient struct {
	client.Client
	err error
}

func (c failedLedgerClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return c.err
}

func ledgerClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func TestKubernetesObservationDoesNotEraseUnavailableUsage(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	reservation, err := ledger.Reserve(ctx, "account", budget.UsageReservation{MaxUnits: 100, Units: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Settle(ctx, reservation, budget.TrustedUsage{Units: 42}); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("fixture API unavailable")
	broken := budgetkubernetes.NewLedger(failedLedgerClient{Client: c, err: unavailable}, "ledger")
	if _, err := broken.State(ctx, "account"); !errors.Is(err, unavailable) {
		t.Fatalf("lost observation error: %v", err)
	}
	if _, err := broken.Seal(ctx, "account"); !errors.Is(err, unavailable) {
		t.Fatalf("unavailable account sealed: %v", err)
	}
	if got := observedLedgerState(t, ledger, ctx, "account"); got != (budget.State{UnitsUsed: 42}) {
		t.Fatalf("usage after API recovery = %+v", got)
	}
}

func TestKubernetesCorruptAccountingFailsObservationAndMutation(t *testing.T) {
	valid := `"UnitsUsed":0,"CostMicros":0,"ReservedUnits":0,"ReservedCostMicros":0`
	for name, data := range map[string]string{
		"missing": "", "malformed": "{", "null": "null", "empty": "{}",
		"missing-counter":      `{"UnitsUsed":0,"CostMicros":0,"ReservedUnits":0}`,
		"negative":             `{"UnitsUsed":-1,"CostMicros":0,"ReservedUnits":0,"ReservedCostMicros":0}`,
		"overflow":             `{"UnitsUsed":9223372036854775807,"CostMicros":0,"ReservedUnits":1,"ReservedCostMicros":0}`,
		"unknown":              "{" + valid + `,"unexpected":0}`,
		"trailing":             "{" + valid + "} {}",
		"incomplete-pending":   "{" + valid + `,"pendingReservations":1}`,
		"negative-pending":     "{" + valid + `,"pendingReservations":-1,"pendingUnboundedReservations":0}`,
		"inconsistent-pending": "{" + valid + `,"pendingReservations":0,"pendingUnboundedReservations":1}`,
		"inconsistent-receipt": `{"UnitsUsed":1,"CostMicros":0,"ReservedUnits":0,"ReservedCostMicros":0,"sealed":{"Units":0,"CostMicros":0},"pendingReservations":0,"pendingUnboundedReservations":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c := ledgerClient(t)
			ledger := budgetkubernetes.NewLedger(c, "ledger")
			if err := ledger.Initialize(ctx, "account"); err != nil {
				t.Fatal(err)
			}
			var records corev1.ConfigMapList
			if err := c.List(ctx, &records); err != nil {
				t.Fatal(err)
			}
			records.Items[0].Data = map[string]string{"state": data}
			if err := c.Update(ctx, &records.Items[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := ledger.State(ctx, "account"); err == nil {
				t.Fatal("corrupt accounting observed as zero")
			}
			if _, err := ledger.Seal(ctx, "account"); err == nil {
				t.Fatal("corrupt accounting produced a trusted receipt")
			}
			if _, err := ledger.Reserve(ctx, "account", budget.UsageReservation{MaxUnits: 100, Units: 1}); err == nil {
				t.Fatal("corrupt accounting admitted more usage")
			}
		})
	}
}

func TestLegacyAccountingRemainsObservableButCannotSealUnknownReservations(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	if err := ledger.Initialize(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	var records corev1.ConfigMapList
	if err := c.List(ctx, &records); err != nil {
		t.Fatal(err)
	}
	records.Items[0].Data["state"] = `{"UnitsUsed":20,"CostMicros":30,"ReservedUnits":0,"ReservedCostMicros":0}`
	if err := c.Update(ctx, &records.Items[0]); err != nil {
		t.Fatal(err)
	}
	if got := observedLedgerState(t, ledger, ctx, "account"); got != (budget.State{UnitsUsed: 20, CostMicros: 30}) {
		t.Fatalf("legacy usage changed: %+v", got)
	}
	if err := ledger.Initialize(ctx, "account"); err == nil {
		t.Fatal("legacy unknown reservation history initialized as known")
	}
	reservation, err := ledger.Reserve(ctx, "account", budget.UsageReservation{MaxUnits: 100, Units: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Settle(ctx, reservation, budget.TrustedUsage{Units: 4}); err != nil {
		t.Fatal(err)
	}
	if got := observedLedgerState(t, ledger, ctx, "account"); got != (budget.State{UnitsUsed: 24, CostMicros: 30}) {
		t.Fatalf("legacy accounting reset: %+v", got)
	}
	if _, err := ledger.Seal(ctx, "account"); err == nil {
		t.Fatal("ordinary settlement erased unknown legacy reservations")
	}
}

func TestMissingExpectedKubernetesAccountCannotBecomeZeroSeal(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	if err := ledger.Initialize(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	var records corev1.ConfigMapList
	if err := c.List(ctx, &records); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &records.Items[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Seal(ctx, "account"); err == nil {
		t.Fatal("deleted expected account reset to a zero receipt")
	}
}

func TestReserveAndSealAreAtomicAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	for _, tc := range []struct {
		name          string
		first, second budget.Ledger
	}{
		{"kubernetes", budgetkubernetes.NewLedger(c, "ledger"), budgetkubernetes.NewLedger(c, "ledger")},
		{"memory", budget.NewMemoryLedger(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := tc.second
			if second == nil {
				second = tc.first
			}
			for i := range 30 {
				key := budget.AccountKey(fmt.Sprintf("race-%d", i))
				if err := tc.first.Initialize(ctx, key); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				var reservation *budget.LedgerReservation
				var reserveErr, sealErr error
				var receipt budget.TrustedUsage
				wg.Go(func() {
					<-start
					reservation, reserveErr = tc.first.Reserve(ctx, key, budget.UsageReservation{MaxUnits: 100, MaxCostMicros: 200, Units: 10, CostMicros: 20})
				})
				wg.Go(func() { <-start; receipt, sealErr = second.Seal(ctx, key) })
				close(start)
				wg.Wait()
				if sealErr != nil {
					t.Fatal(sealErr)
				}
				want := budget.TrustedUsage{}
				if reserveErr == nil {
					want = budget.TrustedUsage{Units: 10, CostMicros: 20}
					if err := tc.first.Settle(ctx, reservation, budget.TrustedUsage{Units: 4, CostMicros: 8}); err != nil {
						t.Fatal(err)
					}
				}
				if receipt != want {
					t.Fatalf("seal missed admitted usage: got %+v, want %+v", receipt, want)
				}
				if _, err := tc.first.Reserve(ctx, key, budget.UsageReservation{Units: 1}); err == nil {
					t.Fatal("sealed account admitted new usage")
				}
				if again, err := second.Seal(ctx, key); err != nil || again != receipt {
					t.Fatalf("retry changed immutable receipt: %+v, %v", again, err)
				}
			}
		})
	}
}

func TestLedgerRejectsOverflowRatherThanResettingObservation(t *testing.T) {
	for _, ledger := range []budget.Ledger{budget.NewMemoryLedger(), budgetkubernetes.NewLedger(ledgerClient(t), "ledger")} {
		ctx := context.Background()
		reservation, err := ledger.Reserve(ctx, "overflow", budget.UsageReservation{Units: math.MaxInt64})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Reserve(ctx, "overflow", budget.UsageReservation{Units: 1}); err == nil {
			t.Fatal("overflowed outstanding usage")
		}
		if err := ledger.Settle(ctx, reservation, budget.TrustedUsage{Units: math.MaxInt64}); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Reserve(ctx, "overflow", budget.UsageReservation{Units: 1}); err == nil {
			t.Fatal("overflowed consumed usage")
		}
		if got := observedLedgerState(t, ledger, ctx, "overflow"); got != (budget.State{UnitsUsed: math.MaxInt64}) {
			t.Fatalf("overflow changed accounting: %+v", got)
		}
	}
}

func TestKubernetesLedgerCompetingReplicasCannotOversubscribe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	a := budgetkubernetes.NewLedger(c, "ledger")
	b := budgetkubernetes.NewLedger(c, "ledger")
	if _, err := a.Reserve(context.Background(), "run-replicas", budget.UsageReservation{MaxUnits: 100, Units: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(context.Background(), "run-replicas", budget.UsageReservation{MaxUnits: 100, Units: 60}); err == nil {
		t.Fatal("second replica oversubscribed the hard ceiling")
	}
}

func TestInitializedAllocationCannotRecreateDeletedUsage(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	request := budget.UsageReservation{MaxUnits: 100, MaxCostMicros: 100, Units: 60, CostMicros: 20, RequireExisting: true}
	if _, err := ledger.Reserve(ctx, "expected", request); err == nil {
		t.Fatal("expected allocation created itself before initialization")
	}
	if err := ledger.Initialize(ctx, "expected"); err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(ctx, "expected", request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ledger.Settle(ctx, reservation, budget.TrustedUsage{Units: 40, CostMicros: 10}); err != nil {
		t.Fatal(err)
	}
	var records corev1.ConfigMapList
	if err = c.List(ctx, &records, client.InNamespace("ledger")); err != nil || len(records.Items) != 1 {
		t.Fatalf("expected one allocation record: %v", err)
	}
	if err = c.Delete(ctx, &records.Items[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Reserve(ctx, "expected", request); err == nil {
		t.Fatal("deleted consumption became a new initialized allowance")
	}
	if _, err = ledger.Seal(ctx, "expected"); err == nil {
		t.Fatal("deleted allocation returned a zero receipt")
	}
}
