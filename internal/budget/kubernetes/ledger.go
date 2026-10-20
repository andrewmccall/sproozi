// Package kubernetes persists shared budget rules in CAS-updated ConfigMaps.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const dataKey = "state"

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
	var cm corev1.ConfigMap
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: s.name(key)}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, budget.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("budget: observe ledger: %w", err)
	}
	return budget.DecodeRecord(cm.Data[dataKey])
}

func (s *store) Update(ctx context.Context, key budget.AccountKey, mutate func(*budget.Record, bool) error) error {
	return s.update(ctx, s.name(key), mutate)
}

// conflictTimeout bounds retries for a caller whose context has no deadline.
// Settlement passes its own 30 s settlement timeout.
const conflictTimeout = 30 * time.Second

// update applies mutate with compare-and-swap. Concurrent settlements of one
// account conflict, so it retries with jittered exponential backoff until it
// succeeds or ctx ends, rather than after a fixed number of attempts.
func (s *store) update(ctx context.Context, configMap string, mutate func(*budget.Record, bool) error) error {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, conflictTimeout)
		defer cancel()
	}
	backoff := 5 * time.Millisecond
	for {
		var cm corev1.ConfigMap
		name := client.ObjectKey{Namespace: s.namespace, Name: configMap}
		err := s.client.Get(ctx, name, &cm)
		exists := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		record := budget.NewRecord()
		if exists {
			record, err = budget.DecodeRecord(cm.Data[dataKey])
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
		if !exists {
			cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace, Labels: map[string]string{componentLabel: componentValue}}, Data: map[string]string{dataKey: data}}
			err = s.client.Create(ctx, &cm)
		} else {
			cm.Data = map[string]string{dataKey: data}
			err = s.client.Update(ctx, &cm)
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

const (
	componentLabel = "app.kubernetes.io/component"
	componentValue = "budget-ledger"
)

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
	options := []client.ListOption{client.InNamespace(namespace), client.MatchingLabels{componentLabel: componentValue},
		client.Limit(sweepPage)}
	for {
		var list corev1.ConfigMapList
		if err := c.List(ctx, &list, options...); err != nil {
			return fmt.Errorf("budget: list ledgers: %w", err)
		}
		for _, cm := range list.Items {
			if err := s.sweep(ctx, cm, now, legacy); err != nil && first == nil {
				first = err
			}
		}
		if list.Continue == "" {
			return first
		}
		options = []client.ListOption{client.InNamespace(namespace), client.MatchingLabels{componentLabel: componentValue},
			client.Limit(sweepPage), client.Continue(list.Continue)}
	}
}

// sweepPage bounds one list of budget ledgers; they are never deleted.
const sweepPage = 500

// sweep recovers one ledger ConfigMap. A ledger deleted since the list stays
// deleted: the update refuses to recreate it.
func (s *store) sweep(ctx context.Context, cm corev1.ConfigMap, now time.Time, legacy bool) error {
	needed, err := budget.NeedsRecovery(cm.Data[dataKey], now, legacy)
	if err != nil {
		// A corrupt ledger cannot be recovered by retrying; it is not a
		// failure of the sweep, which fails only on API errors.
		slog.Default().Error("Skipped corrupt budget ledger", "configMap", cm.Name, "error", err.Error())
		return nil
	}
	if !needed {
		return nil
	}
	var committed budget.Recovered
	err = s.update(ctx, cm.Name, func(record *budget.Record, exists bool) error {
		if !exists {
			return budget.ErrAccountNotFound
		}
		var recoverErr error
		committed, recoverErr = budget.RecoverExpired(record, cm.Name, now, legacy)
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
