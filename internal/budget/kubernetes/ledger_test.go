package kubernetes_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	if err := api.AddToScheme(scheme); err != nil {
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
	if err := api.AddToScheme(scheme); err != nil {
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
			var records api.BudgetLedgerList
			if err := c.List(ctx, &records); err != nil {
				t.Fatal(err)
			}
			records.Items[0].Spec.State = data
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
	var records api.BudgetLedgerList
	if err := c.List(ctx, &records); err != nil {
		t.Fatal(err)
	}
	records.Items[0].Spec.State = `{"UnitsUsed":20,"CostMicros":30,"ReservedUnits":0,"ReservedCostMicros":0}`
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
	var records api.BudgetLedgerList
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
	if err := api.AddToScheme(scheme); err != nil {
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
	var records api.BudgetLedgerList
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

// slowClient adds API latency so concurrent read-modify-write cycles overlap.
type slowClient struct{ client.Client }

func (c slowClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	time.Sleep(2 * time.Millisecond)
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c slowClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	time.Sleep(2 * time.Millisecond)
	return c.Client.Update(ctx, obj, opts...)
}

// Many calls settling one account at once conflict on the ledger; every
// settlement still lands, with exact totals.
func TestConcurrentSettlementsAllSucceed(t *testing.T) {
	ctx := context.Background()
	ledger := budgetkubernetes.NewLedger(slowClient{ledgerClient(t)}, "ledger")
	const calls = 16
	reservations := make([]*budget.LedgerReservation, calls)
	for i := range reservations {
		r, err := ledger.Reserve(ctx, "account", budget.UsageReservation{MaxUnits: 10_000, Units: 100})
		if err != nil {
			t.Fatal(err)
		}
		reservations[i] = r
	}
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for _, r := range reservations {
		wg.Go(func() {
			settleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			errs <- ledger.Settle(settleCtx, r, budget.TrustedUsage{Units: 57})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent settlement failed: %v", err)
		}
	}
	if state := observedLedgerState(t, ledger, ctx, "account"); state.UnitsUsed != calls*57 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v", state)
	}
}

// The periodic sweep recovers an idle account's expired lease without any
// further reservation or seal of that account.
func TestSweepRecoversIdleAccounts(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	if _, err := ledger.Reserve(ctx, "idle", budget.UsageReservation{MaxUnits: 1000,
		Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 250}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(ctx, "live", budget.UsageReservation{MaxUnits: 1000, Units: 10,
		Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(time.Hour), Units: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := budgetkubernetes.Sweep(ctx, c, "ledger", time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if state := observedLedgerState(t, ledger, ctx, "idle"); state.UnitsUsed != 250 || state.ReservedUnits != 0 {
		t.Fatalf("idle account state=%+v", state)
	}
	if state := observedLedgerState(t, ledger, ctx, "live"); state.UnitsUsed != 0 || state.ReservedUnits != 10 {
		t.Fatalf("live account state=%+v", state)
	}
}

// A reservation left by a gateway from before leases has none. The periodic
// sweep leaves it, but the startup sweep, run by the only reserving process
// before it serves, charges it in full, keeps leased reservations and lets
// the account seal again.
func TestStartupSweepSettlesLegacyReservationsInFull(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	if err := ledger.Initialize(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(ctx, "legacy", budget.UsageReservation{MaxUnits: 100}); err != nil {
		t.Fatal(err)
	}
	if err := budgetkubernetes.Sweep(ctx, c, "ledger", time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if state := observedLedgerState(t, ledger, ctx, "legacy"); state.ReservedUnits != 100 {
		t.Fatalf("periodic sweep settled a legacy reservation: %+v", state)
	}
	if err := budgetkubernetes.Sweep(ctx, c, "ledger", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if state := observedLedgerState(t, ledger, ctx, "legacy"); state.UnitsUsed != 100 || state.ReservedUnits != 0 {
		t.Fatalf("startup sweep state=%+v", state)
	}
	if receipt, err := ledger.Seal(ctx, "legacy"); err != nil || receipt.Units != 100 {
		t.Fatalf("seal=%+v err=%v", receipt, err)
	}
}

// deletingClient deletes the BudgetLedger right after the read named by
// afterGet (1 for the first Get), or after the first List.
type deletingClient struct {
	client.Client
	afterGet  int
	afterList bool
	gets      int
}

func (c *deletingClient) deleteLedgers(ctx context.Context) {
	var list api.BudgetLedgerList
	_ = c.Client.List(ctx, &list)
	for i := range list.Items {
		_ = c.Delete(ctx, &list.Items[i])
	}
}

func (c *deletingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	c.gets++
	if c.gets == c.afterGet {
		c.deleteLedgers(ctx)
	}
	return err
}

func (c *deletingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := c.Client.List(ctx, list, opts...)
	if c.afterList {
		c.afterList = false
		c.deleteLedgers(ctx)
	}
	return err
}

// expiredAccount initializes account with one expired lease.
func expiredAccount(t *testing.T, c client.Client) {
	t.Helper()
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	if err := ledger.Initialize(context.Background(), "account"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(context.Background(), "account", budget.UsageReservation{MaxUnits: 100, RequireExisting: true,
		Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 10}}); err != nil {
		t.Fatal(err)
	}
}

// A ledger deleted while a sweep or a recovery is under way stays deleted:
// neither recreates it as an empty record that later calls could spend.
func TestRecoveryNeverRecreatesADeletedAccount(t *testing.T) {
	ctx := context.Background()
	t.Run("sweep", func(t *testing.T) {
		base := ledgerClient(t)
		expiredAccount(t, base)
		c := &deletingClient{Client: base, afterList: true}
		if err := budgetkubernetes.Sweep(ctx, c, "ledger", time.Now(), true); err != nil {
			t.Fatal(err)
		}
		if _, err := budgetkubernetes.NewLedger(base, "ledger").State(ctx, "account"); !errors.Is(err, budget.ErrAccountNotFound) {
			t.Fatalf("deleted ledger was recreated: %v", err)
		}
	})
	t.Run("recover before reserve", func(t *testing.T) {
		base := ledgerClient(t)
		expiredAccount(t, base)
		c := &deletingClient{Client: base, afterGet: 1}
		_, err := budgetkubernetes.NewLedger(c, "ledger").Reserve(ctx, "account",
			budget.UsageReservation{MaxUnits: 100, Units: 1, RequireExisting: true})
		if err == nil {
			t.Fatal("a reservation succeeded against a deleted, recreated account")
		}
		if _, err := budgetkubernetes.NewLedger(base, "ledger").State(ctx, "account"); !errors.Is(err, budget.ErrAccountNotFound) {
			t.Fatalf("deleted ledger was recreated: %v", err)
		}
	})
}

// A corrupt ledger is skipped, not a failure: the legacy pass still settles
// the good ledger and succeeds, so it is not retried for ever.
func TestCorruptLedgerDoesNotFailTheSweep(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	if _, err := ledger.Reserve(ctx, "legacy", budget.UsageReservation{MaxUnits: 100}); err != nil {
		t.Fatal(err)
	}
	corrupt := &api.BudgetLedger{ObjectMeta: metav1.ObjectMeta{Name: "sproozi-budget-corrupt", Namespace: "ledger"},
		Spec: api.BudgetLedgerSpec{Account: "corrupt", State: "{", WrittenAt: metav1.Now()}}
	if err := c.Create(ctx, corrupt); err != nil {
		t.Fatal(err)
	}
	if err := budgetkubernetes.Sweep(ctx, c, "ledger", time.Now(), true); err != nil {
		t.Fatalf("a corrupt ledger failed the sweep: %v", err)
	}
	if state := observedLedgerState(t, ledger, ctx, "legacy"); state.UnitsUsed != 100 || state.ReservedUnits != 0 {
		t.Fatalf("legacy ledger state=%+v", state)
	}
}

// A ledger is removed only when its run is gone, nothing is reserved and it
// has not been written for the retention period.
func TestPruneRemovesOnlySettledLedgersOfGoneRuns(t *testing.T) {
	ctx := context.Background()
	c := ledgerClient(t)
	ledger := budgetkubernetes.NewLedger(c, "ledger")
	settle := func(key budget.AccountKey) {
		r, err := ledger.Reserve(ctx, key, budget.UsageReservation{MaxUnits: 100, Units: 10})
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.Settle(ctx, r, budget.TrustedUsage{Units: 5}); err != nil {
			t.Fatal(err)
		}
	}
	settle("gone-run/model.inference")
	settle("live-run/model.inference")
	settle("recent-run/model.inference")
	settle("unlabelled")
	if _, err := ledger.Reserve(ctx, "pending-run/model.inference", budget.UsageReservation{MaxUnits: 100, Units: 10}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := metav1.NewTime(now.Add(-budgetkubernetes.DefaultRetention - time.Hour).UTC())
	var all api.BudgetLedgerList
	if err := c.List(ctx, &all); err != nil {
		t.Fatal(err)
	}
	for i := range all.Items {
		stored := &all.Items[i]
		if stored.Labels["sproozi.com/run-uid"] != "recent-run" {
			stored.Spec.WrittenAt = old
			if err := c.Update(ctx, stored); err != nil {
				t.Fatal(err)
			}
		}
	}

	deleted, err := budgetkubernetes.Prune(ctx, c, "ledger", now, budgetkubernetes.DefaultRetention,
		func(uid string) bool { return uid == "live-run" })
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if _, err := ledger.State(ctx, "gone-run/model.inference"); !errors.Is(err, budget.ErrAccountNotFound) {
		t.Fatalf("gone run's ledger: err = %v, want deleted", err)
	}
	for _, kept := range []budget.AccountKey{"live-run/model.inference", "recent-run/model.inference",
		"pending-run/model.inference", "unlabelled"} {
		if _, err := ledger.State(ctx, kept); err != nil {
			t.Fatalf("%s: %v, want kept", kept, err)
		}
	}
}
