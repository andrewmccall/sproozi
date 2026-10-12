// Package kubernetes persists shared budget rules in CAS-updated ConfigMaps.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

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
	for range 8 {
		var cm corev1.ConfigMap
		name := client.ObjectKey{Namespace: s.namespace, Name: s.name(key)}
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
			cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace, Labels: map[string]string{"app.kubernetes.io/component": "budget-ledger"}}, Data: map[string]string{dataKey: data}}
			err = s.client.Create(ctx, &cm)
			if apierrors.IsAlreadyExists(err) {
				continue
			}
		} else {
			cm.Data = map[string]string{dataKey: data}
			err = s.client.Update(ctx, &cm)
			if apierrors.IsConflict(err) {
				continue
			}
		}
		return err
	}
	return fmt.Errorf("budget: budget ledger update conflicted after retries")
}
