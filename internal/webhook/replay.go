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
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: map[string]string{"sproozi.com/managed-by": "sproozi-webhook-replay"}, Annotations: map[string]string{"sproozi.com/expires-at": expiry.UTC().Format(time.RFC3339Nano)}}, Immutable: boolPtr(true), Data: map[string]string{"event": id}}
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
	if raw := existing.Annotations["sproozi.com/expires-at"]; raw != "" {
		if until, err := time.Parse(time.RFC3339Nano, raw); err == nil && !until.After(time.Now()) {
			_ = s.client.Delete(ctx, &existing, client.Preconditions{UID: &existing.UID, ResourceVersion: &existing.ResourceVersion})
			return s.CheckAndRecord(ctx, id, expiry)
		}
	}
	return ErrReplay
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
