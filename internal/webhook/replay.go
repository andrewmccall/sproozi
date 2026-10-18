/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrReplay = errors.New("webhook: replayed event")

const (
	replayLabel            = "sproozi.com/managed-by"
	replayLabelValue       = "sproozi-webhook-replay"
	replayExpiryAnnotation = "sproozi.com/expires-at"
	replaySweepPageSize    = 500
)

type ReplayStore interface {
	CheckAndRecord(context.Context, string, time.Time) error
}

type InMemoryReplayStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// K8sReplayStore uses an atomic ConfigMap create as the replay decision. The
// object is deliberately separate from AgentRuns so a failed run creation
// cannot make an event replayable, and survives handler restarts and replicas.
type K8sReplayStore struct {
	client    client.Client
	namespace string
}

func NewK8sReplayStore(c client.Client, namespace string) *K8sReplayStore {
	return &K8sReplayStore{client: c, namespace: namespace}
}

func (s *K8sReplayStore) CheckAndRecord(ctx context.Context, id string, expiry time.Time) error {
	if s == nil || s.client == nil || id == "" || s.namespace == "" {
		return errors.New("webhook: replay store unavailable")
	}
	sum := sha256.Sum256([]byte(id))
	name := "sproozi-webhook-replay-" + hex.EncodeToString(sum[:])[:32]
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: map[string]string{replayLabel: replayLabelValue}, Annotations: map[string]string{replayExpiryAnnotation: expiry.UTC().Format(time.RFC3339Nano)}}, Immutable: boolPtr(true), Data: map[string]string{"event": id}}
	if err := s.client.Create(ctx, obj); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("webhook: record replay: %w", err)
	}
	var existing corev1.ConfigMap
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return s.CheckAndRecord(ctx, id, expiry)
		}
		return err
	}
	if replayExpired(&existing, time.Now()) {
		_ = s.deleteRecord(ctx, &existing)
		return s.CheckAndRecord(ctx, id, expiry)
	}
	return ErrReplay
}

// SweepExpired deletes every replay record whose window has passed. Each
// accepted event leaves one record, so without a sweep unique event IDs would
// accumulate for ever. Replicas may sweep concurrently: preconditions make a
// record deleted or replaced by another writer a no-op.
func (s *K8sReplayStore) SweepExpired(ctx context.Context) error {
	now := time.Now()
	page := ""
	for {
		var records corev1.ConfigMapList
		if err := s.client.List(ctx, &records, client.InNamespace(s.namespace), client.MatchingLabels{replayLabel: replayLabelValue},
			client.Limit(replaySweepPageSize), client.Continue(page)); err != nil {
			return fmt.Errorf("webhook: list replay records: %w", err)
		}
		for i := range records.Items {
			if replayExpired(&records.Items[i], now) {
				if err := s.deleteRecord(ctx, &records.Items[i]); err != nil {
					return err
				}
			}
		}
		if page = records.Continue; page == "" {
			return nil
		}
	}
}

func (s *K8sReplayStore) deleteRecord(ctx context.Context, record *corev1.ConfigMap) error {
	err := s.client.Delete(ctx, record, client.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("webhook: delete replay record: %w", err)
	}
	return nil
}

// replayExpired reports whether a record's window has passed. A record with a
// missing or unreadable expiry is kept, so it keeps blocking its event.
func replayExpired(record *corev1.ConfigMap, now time.Time) bool {
	until, err := time.Parse(time.RFC3339Nano, record.Annotations[replayExpiryAnnotation])
	return err == nil && !until.After(now)
}

func NewInMemoryReplayStore() *InMemoryReplayStore {
	return &InMemoryReplayStore{
		seen: make(map[string]time.Time),
	}
}

func (s *InMemoryReplayStore) CheckAndRecord(_ context.Context, id string, expiry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for key, seenExpiry := range s.seen {
		if !seenExpiry.After(now) {
			delete(s.seen, key)
		}
	}

	if _, ok := s.seen[id]; ok {
		return ErrReplay
	}
	s.seen[id] = expiry
	return nil
}

func boolPtr(v bool) *bool { return &v }
