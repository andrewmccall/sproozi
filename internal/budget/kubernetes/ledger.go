// Package kubernetes persists shared budget rules in CAS-updated BudgetLedger
// resources (ADR 0045).
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type store struct {
	client    client.Client
	namespace string
}

func NewLedger(c client.Client, namespace string) budget.Ledger {
	return budget.NewLedger(&store{client: c, namespace: namespace})
}

func (s *store) name(key budget.AccountKey) string {
	hash := sha256.Sum256([]byte(key))
	return "sproozi-budget-" + hex.EncodeToString(hash[:])[:20]
}

func (s *store) Read(ctx context.Context, key budget.AccountKey) (*budget.Record, error) {
	var ledger api.BudgetLedger
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: s.name(key)}, &ledger)
	if apierrors.IsNotFound(err) {
		return nil, budget.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("budget: observe ledger: %w", err)
	}
	return budget.DecodeRecord(ledger.Spec.State)
}

func (s *store) Update(ctx context.Context, key budget.AccountKey, mutate func(*budget.Record, bool) error) error {
	return s.update(ctx, s.name(key), key, mutate)
}

// accountParts returns the run UID and capability of an account key, or ""
// for a part the key lacks. Kubernetes keys are "<run UID>/<capability>".
func accountParts(key budget.AccountKey) (run, capability string) {
	run, capability, found := strings.Cut(string(key), "/")
	if !found || len(validation.IsValidLabelValue(run)) != 0 {
		return "", ""
	}
	return run, capability
}

// conflictTimeout bounds retries for a caller whose context has no deadline.
// Settlement passes its own 30 s settlement timeout.
const conflictTimeout = 30 * time.Second

// update applies mutate with compare-and-swap and stamps the write time used
// by Prune. Concurrent settlements of one account conflict, so it retries with
// jittered exponential backoff until it succeeds or ctx ends, rather than
// after a fixed number of attempts. key names a newly created ledger's
// account and run; an existing ledger keeps them.
func (s *store) update(ctx context.Context, ledgerName string, key budget.AccountKey,
	mutate func(*budget.Record, bool) error,
) error {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, conflictTimeout)
		defer cancel()
	}
	backoff := 5 * time.Millisecond
	for {
		var ledger api.BudgetLedger
		name := client.ObjectKey{Namespace: s.namespace, Name: ledgerName}
		err := s.client.Get(ctx, name, &ledger)
		exists := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		record := budget.NewRecord()
		if exists {
			record, err = budget.DecodeRecord(ledger.Spec.State)
			if err != nil {
				return err
			}
		}
		if err = mutate(record, exists); err != nil {
			return err
		}
		data, err := record.Encode()
		if err != nil {
			return err
		}
		units, cost, finalized := record.Usage()
		written := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
		if !exists {
			run, capability := accountParts(key)
			labels := map[string]string{}
			if run != "" {
				labels[runLabel] = run
			}
			ledger = api.BudgetLedger{
				ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace, Labels: labels},
				Spec:       api.BudgetLedgerSpec{Account: string(key), RunUID: run, Capability: capability},
			}
		}
		ledger.Spec.State, ledger.Spec.UnitsUsed, ledger.Spec.CostMicros = data, units, cost
		ledger.Spec.Finalized, ledger.Spec.WrittenAt = finalized, written
		if exists {
			err = s.client.Update(ctx, &ledger)
		} else {
			err = s.client.Create(ctx, &ledger)
		}
		if !apierrors.IsAlreadyExists(err) && !apierrors.IsConflict(err) {
			return err
		}
		// Sleep between half and one and a half times the backoff.
		delay := backoff/2 + rand.N(backoff)
		backoff = min(2*backoff, time.Second)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return fmt.Errorf("budget: ledger update conflicted until its deadline: %w", ctx.Err())
		}
	}
}

// runLabel names the run whose accounts a ledger holds, so Prune lists only
// ledgers it can match to a run.
const runLabel = "sproozi.com/run-uid"

// SweepInterval is how often the shared gateway recovers expired leases.
const SweepInterval = time.Minute

// Sweep settles every expired reservation lease in every budget ledger, so
// an idle account does not keep an abandoned reservation until its next
// reservation or seal. legacy also charges in full every pending reservation
// without a lease: every current reservation carries one, so such a
// reservation can only come from a gateway from before leases. It returns the
// first API error and carries on past it; a corrupt ledger is logged and
// skipped.
func Sweep(ctx context.Context, c client.Client, namespace string, now time.Time, legacy bool) error {
	s := &store{client: c, namespace: namespace}
	var first error
	options := []client.ListOption{client.InNamespace(namespace), client.Limit(sweepPage)}
	for {
		var list api.BudgetLedgerList
		if err := c.List(ctx, &list, options...); err != nil {
			return fmt.Errorf("budget: list ledgers: %w", err)
		}
		for i := range list.Items {
			if err := s.sweep(ctx, &list.Items[i], now, legacy); err != nil && first == nil {
				first = err
			}
		}
		if list.Continue == "" {
			return first
		}
		options = []client.ListOption{client.InNamespace(namespace), client.Limit(sweepPage), client.Continue(list.Continue)}
	}
}

// sweepPage bounds one list of budget ledgers.
const sweepPage = 500

// sweep recovers one ledger. A ledger deleted since the list stays deleted:
// the update refuses to recreate it.
func (s *store) sweep(ctx context.Context, ledger *api.BudgetLedger, now time.Time, legacy bool) error {
	needed, err := budget.NeedsRecovery(ledger.Spec.State, now, legacy)
	if err != nil {
		// A corrupt ledger cannot be recovered by retrying; it is not a
		// failure of the sweep, which fails only on API errors.
		slog.Default().Error("Skipped corrupt BudgetLedger", "name", ledger.Name, "error", err.Error())
		return nil
	}
	if !needed {
		return nil
	}
	var committed budget.Recovered
	err = s.update(ctx, ledger.Name, budget.AccountKey(ledger.Spec.Account), func(record *budget.Record, exists bool) error {
		if !exists {
			return budget.ErrAccountNotFound
		}
		var recoverErr error
		committed, recoverErr = budget.RecoverExpired(record, ledger.Name, now, legacy)
		return recoverErr
	})
	if errors.Is(err, budget.ErrAccountNotFound) {
		return nil
	}
	if err == nil && committed != nil {
		committed()
	}
	return err
}

// DefaultRetention is how long a settled ledger is kept after its last write
// once its run no longer exists.
const DefaultRetention = 7 * 24 * time.Hour

// Prune deletes ledgers whose run no longer exists, that have no outstanding
// reservation and that were last written more than retention ago. A run that
// no longer exists cannot be authenticated by the gateway, so its accounts
// can take no further charge; their usage is final. runExists must answer
// from a complete, current list of runs. A precondition on the resource
// version makes a ledger written after the list a no-op. It returns the
// number deleted and the first API error.
func Prune(ctx context.Context, c client.Client, namespace string, now time.Time, retention time.Duration,
	runExists func(uid string) bool,
) (int, error) {
	deleted := 0
	var first error
	options := []client.ListOption{client.InNamespace(namespace), client.HasLabels{runLabel}, client.Limit(sweepPage)}
	for {
		var list api.BudgetLedgerList
		if err := c.List(ctx, &list, options...); err != nil {
			return deleted, fmt.Errorf("budget: list ledgers: %w", err)
		}
		for i := range list.Items {
			ledger := &list.Items[i]
			if !prunable(ledger, now, retention, runExists) {
				continue
			}
			err := c.Delete(ctx, ledger, client.Preconditions{UID: &ledger.UID, ResourceVersion: &ledger.ResourceVersion})
			switch {
			case err == nil:
				deleted++
			case apierrors.IsNotFound(err) || apierrors.IsConflict(err):
			default:
				if first == nil {
					first = fmt.Errorf("budget: delete ledger %s: %w", ledger.Name, err)
				}
			}
		}
		if list.Continue == "" {
			return deleted, first
		}
		options = []client.ListOption{client.InNamespace(namespace), client.HasLabels{runLabel}, client.Limit(sweepPage),
			client.Continue(list.Continue)}
	}
}

func prunable(ledger *api.BudgetLedger, now time.Time, retention time.Duration, runExists func(string) bool) bool {
	run := ledger.Labels[runLabel]
	if run == "" || runExists(run) {
		return false
	}
	if ledger.Spec.WrittenAt.IsZero() || now.Sub(ledger.Spec.WrittenAt.Time) < retention {
		return false
	}
	settled, err := budget.Settled(ledger.Spec.State)
	return err == nil && settled
}
