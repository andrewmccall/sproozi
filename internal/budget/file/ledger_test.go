package file_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
)

const accountKey budget.AccountKey = "visit/model.inference"

func TestDurableReservationAndImmutableSealSurviveRestart(t *testing.T) {
	dir := filepath.Join(testutil.PrivateDir(t), "ledger")
	ctx := context.Background()
	ledger, err := budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = ledger.Initialize(ctx, accountKey); err != nil {
		t.Fatal(err)
	}
	r, err := ledger.Reserve(ctx, accountKey, budget.UsageReservation{MaxUnits: 10, MaxCostMicros: 100, Units: 4, CostMicros: 20, RequireExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ledger.Seal(ctx, accountKey)
	if err != nil || receipt != (budget.TrustedUsage{Units: 4, CostMicros: 20}) {
		t.Fatalf("seal=%+v err=%v", receipt, err)
	}
	if err = ledger.Settle(ctx, r, budget.TrustedUsage{Units: 2, CostMicros: 10}); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err = budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	state, err := ledger.State(ctx, accountKey)
	if err != nil || state.UnitsUsed != 2 || state.ReservedUnits != 0 {
		t.Fatalf("restarted state=%+v err=%v", state, err)
	}
	got, err := ledger.Seal(ctx, accountKey)
	if err != nil || got != receipt {
		t.Fatalf("receipt changed: %+v %v", got, err)
	}
	if _, err = ledger.Reserve(ctx, accountKey, budget.UsageReservation{Units: 1, MaxUnits: 10, RequireExisting: true}); err == nil {
		t.Fatal("sealed account reopened")
	}
}

func TestFileLedgerReserveAndSealAreAtomic(t *testing.T) {
	ledger, err := budgetfile.NewLedger(filepath.Join(testutil.PrivateDir(t), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	ctx := context.Background()
	if err = ledger.Initialize(ctx, accountKey); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	reservations := make(chan *budget.LedgerReservation, 16)
	for range 16 {
		wg.Go(func() {
			<-start
			r, err := ledger.Reserve(ctx, accountKey, budget.UsageReservation{Units: 1, CostMicros: 1, MaxUnits: 16, MaxCostMicros: 16, RequireExisting: true})
			if err == nil {
				reservations <- r
			}
		})
	}
	close(start)
	receipt, err := ledger.Seal(ctx, accountKey)
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(reservations)
	count := int64(0)
	for r := range reservations {
		count++
		if err = ledger.Release(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if receipt.Units != count || receipt.CostMicros != count {
		t.Fatalf("reserve raced seal: accepted=%d receipt=%+v", count, receipt)
	}
	got, err := ledger.Seal(ctx, accountKey)
	if err != nil || got != receipt {
		t.Fatalf("seal changed: %+v %v", got, err)
	}
}

func TestFileLedgerPrivateOwnershipAndRecordLoss(t *testing.T) {
	dir := filepath.Join(testutil.PrivateDir(t), "ledger")
	ctx := context.Background()
	ledger, err := budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	if other, err := budgetfile.NewLedger(dir); err == nil {
		_ = other.Close()
		t.Fatal("second owner acquired ledger")
	}
	if err = ledger.Initialize(ctx, accountKey); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "account-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("records=%v err=%v", paths, err)
	}
	if err = testutil.WriteFile(paths[0], []byte(`{"UnitsUsed":-1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.State(ctx, accountKey); err == nil {
		t.Fatal("corrupt state became known zero")
	}
	if err = os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.State(ctx, accountKey); !errors.Is(err, budget.ErrAccountNotFound) {
		t.Fatalf("missing state=%v", err)
	}
	if _, err = ledger.Reserve(ctx, accountKey, budget.UsageReservation{Units: 1, MaxUnits: 10, RequireExisting: true}); err == nil {
		t.Fatal("missing initialized account recreated")
	}
	if _, err = ledger.Seal(ctx, accountKey); err == nil {
		t.Fatal("missing account sealed as zero")
	}
}

func TestFileLedgerRejectsSharedOrSymlinkStorage(t *testing.T) {
	dir := testutil.PrivateDir(t)
	if err := makeTestPathPublic(dir); err != nil {
		t.Fatal(err)
	}
	if ledger, err := budgetfile.NewLedger(dir); err == nil {
		_ = ledger.Close()
		t.Fatal("shared directory accepted")
	}
	link := filepath.Join(testutil.PrivateDir(t), "ledger")
	if err := os.Symlink(testutil.PrivateDir(t), link); err != nil {
		t.Fatal(err)
	}
	if ledger, err := budgetfile.NewLedger(link); err == nil {
		_ = ledger.Close()
		t.Fatal("symlink directory accepted")
	}
}

// A crashed gateway's unbounded reservation survives in the file as a lease.
// After a restart, once the lease has expired, sealing settles it at its
// recovery charge instead of failing on unbounded pending usage.
func TestAbandonedReservationIsRecoveredAfterRestart(t *testing.T) {
	dir := filepath.Join(testutil.PrivateDir(t), "ledger")
	ctx := context.Background()
	ledger, err := budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = ledger.Initialize(ctx, accountKey); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Reserve(ctx, accountKey, budget.UsageReservation{MaxUnits: 1000, RequireExisting: true,
		Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 250}}); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	receipt, err := restarted.Seal(ctx, accountKey)
	if err != nil || receipt != (budget.TrustedUsage{Units: 250}) {
		t.Fatalf("seal=%+v err=%v, want the recovery charge", receipt, err)
	}
}

// The home daemon's sweep recovers an idle account's expired lease without
// any further reservation or seal of that account.
func TestSweepRecoversIdleAccounts(t *testing.T) {
	ctx := context.Background()
	ledger, err := budgetfile.NewLedger(filepath.Join(testutil.PrivateDir(t), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	if _, err = ledger.Reserve(ctx, accountKey, budget.UsageReservation{MaxUnits: 1000,
		Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 250}}); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Sweep(time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if state, err := ledger.State(ctx, accountKey); err != nil || state.UnitsUsed != 250 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

// Home, the ledger's only owner, charges a pre-lease reservation in full when
// it opens the ledger, so the account can seal again.
func TestOpeningSweepSettlesLegacyReservationsInFull(t *testing.T) {
	ctx := context.Background()
	ledger, err := budgetfile.NewLedger(filepath.Join(testutil.PrivateDir(t), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	if err = ledger.Initialize(ctx, accountKey); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Reserve(ctx, accountKey, budget.UsageReservation{MaxUnits: 100, RequireExisting: true}); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Sweep(time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if receipt, err := ledger.Seal(ctx, accountKey); err != nil || receipt.Units != 100 {
		t.Fatalf("seal=%+v err=%v", receipt, err)
	}
}

// A corrupt record is skipped, not a failure: the legacy pass still settles
// the good record and succeeds.
func TestCorruptRecordDoesNotFailTheSweep(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(testutil.PrivateDir(t), "ledger")
	ledger, err := budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	if _, err = ledger.Reserve(ctx, accountKey, budget.UsageReservation{MaxUnits: 100}); err != nil {
		t.Fatal(err)
	}
	if err = testutil.WriteFile(filepath.Join(dir, "account-corrupt.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Sweep(time.Now(), true); err != nil {
		t.Fatalf("a corrupt record failed the sweep: %v", err)
	}
	if state, err := ledger.State(ctx, accountKey); err != nil || state.UnitsUsed != 100 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}
