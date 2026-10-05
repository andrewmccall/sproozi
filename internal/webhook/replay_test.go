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

package webhook_test

import (
	"context"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/webhook"
)

func TestReplayStore_FirstCallSucceeds(t *testing.T) {
	s := webhook.NewInMemoryReplayStore()
	expiry := time.Now().Add(5 * time.Minute)
	if err := s.CheckAndRecord(context.Background(), "id1", expiry); err != nil {
		t.Fatalf("expected first call to succeed, got: %v", err)
	}
}

func TestReplayStore_SecondCallReturnsErrReplay(t *testing.T) {
	s := webhook.NewInMemoryReplayStore()
	expiry := time.Now().Add(5 * time.Minute)
	_ = s.CheckAndRecord(context.Background(), "id1", expiry)
	if err := s.CheckAndRecord(context.Background(), "id1", expiry); err != webhook.ErrReplay {
		t.Fatalf("expected ErrReplay on second call, got: %v", err)
	}
}

func TestReplayStore_DifferentIDsAreFine(t *testing.T) {
	s := webhook.NewInMemoryReplayStore()
	expiry := time.Now().Add(5 * time.Minute)
	_ = s.CheckAndRecord(context.Background(), "id1", expiry)
	if err := s.CheckAndRecord(context.Background(), "id2", expiry); err != nil {
		t.Fatalf("different IDs should not collide, got: %v", err)
	}
}

func TestReplayStore_ExpiredEntryAllowsReuse(t *testing.T) {
	s := webhook.NewInMemoryReplayStore()
	// Record with a past expiry.
	pastExpiry := time.Now().Add(-1 * time.Second)
	_ = s.CheckAndRecord(context.Background(), "id1", pastExpiry)
	// Should succeed because the entry has expired.
	if err := s.CheckAndRecord(context.Background(), "id1", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("expired entry should allow re-use, got: %v", err)
	}
}

func TestReplayStore_ConcurrentSafeFirstWriterWins(t *testing.T) {
	s := webhook.NewInMemoryReplayStore()
	expiry := time.Now().Add(5 * time.Minute)

	const n = 100
	results := make([]error, n)
	done := make(chan int, n)

	for i := range n {
		go func(idx int) {
			results[idx] = s.CheckAndRecord(context.Background(), "shared-id", expiry)
			done <- idx
		}(i)
	}
	for range n {
		<-done
	}

	successCount := 0
	for _, err := range results {
		if err == nil {
			successCount++
		} else if err != webhook.ErrReplay {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successCount != 1 {
		t.Errorf("exactly one goroutine should succeed, got %d", successCount)
	}
}
