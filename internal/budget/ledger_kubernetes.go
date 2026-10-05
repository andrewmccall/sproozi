package budget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const budgetLedgerDataKey = "state"

// KubernetesLedger stores one accounting record per run in a ConfigMap.
// Updates use resourceVersion as a compare-and-swap and retry conflicts, so
// multiple gateway replicas cannot admit usage beyond the current ceiling.
type KubernetesLedger struct {
	client     client.Client
	namespace  string
	maxRetries int
}

func NewKubernetesLedger(c client.Client, namespace string) *KubernetesLedger {
	return &KubernetesLedger{client: c, namespace: namespace, maxRetries: 8}
}

func (l *KubernetesLedger) name(runID AccountKey) string {
	h := sha256.Sum256([]byte(runID))
	return "sproozi-budget-" + hex.EncodeToString(h[:])[:20]
}

func (l *KubernetesLedger) Reserve(ctx context.Context, runID AccountKey, req UsageReservation) (*LedgerReservation, error) {
	var result *LedgerReservation
	err := l.update(ctx, runID, func(s *State, exists bool) error {
		reservation, err := reserveState(s, req)
		if err != nil {
			return err
		}
		result = reservation
		return nil
	})
	if result != nil {
		result.runID = runID
	}
	return result, err
}

func (l *KubernetesLedger) Settle(ctx context.Context, r *LedgerReservation, usage TrustedUsage) error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := l.update(ctx, r.runID, func(s *State, _ bool) error { return settleState(s, r, usage) })
	if err == nil {
		r.settled = true
	}
	return err
}

func (l *KubernetesLedger) Release(ctx context.Context, r *LedgerReservation) error {
	return l.Settle(ctx, r, TrustedUsage{})
}

func (l *KubernetesLedger) State(ctx context.Context, runID AccountKey) State {
	var cm corev1.ConfigMap
	if err := l.client.Get(ctx, types.NamespacedName{Name: l.name(runID), Namespace: l.namespace}, &cm); err != nil {
		return State{}
	}
	var s State
	if json.Unmarshal([]byte(cm.Data[budgetLedgerDataKey]), &s) != nil {
		return State{}
	}
	return s
}

func (l *KubernetesLedger) update(ctx context.Context, runID AccountKey, mutate func(*State, bool) error) error {
	for attempt := 0; attempt < l.maxRetries; attempt++ {
		var cm corev1.ConfigMap
		nn := types.NamespacedName{Name: l.name(runID), Namespace: l.namespace}
		err := l.client.Get(ctx, nn, &cm)
		exists := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		s := State{}
		if exists && cm.Data != nil {
			if err := json.Unmarshal([]byte(cm.Data[budgetLedgerDataKey]), &s); err != nil {
				return fmt.Errorf("budget: invalid ledger state: %w", err)
			}
		}
		if err := mutate(&s, exists); err != nil {
			return err
		}
		data, _ := json.Marshal(s)
		if !exists {
			cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace, Labels: map[string]string{"app.kubernetes.io/component": "budget-ledger"}}, Data: map[string]string{budgetLedgerDataKey: string(data)}}
			if err := l.client.Create(ctx, &cm); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return err
			}
		} else {
			cm.Data = map[string]string{budgetLedgerDataKey: string(data)}
			if err := l.client.Update(ctx, &cm); err != nil {
				if apierrors.IsConflict(err) {
					continue
				}
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("budget: budget ledger update conflicted after retries")
}
