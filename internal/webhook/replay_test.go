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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/andrewmccall/sproozi/internal/webhook"
)

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

// TestReplayRecordOutlivesAcceptanceAcrossClockSkew verifies that a replica
// whose clock runs behind cannot accept an event again after a replica whose
// clock runs ahead has swept its record. The sweeping store judges expiry by
// the test's clock; the accepting handler's clock is almost a window behind.
func TestReplayRecordOutlivesAcceptanceAcrossClockSkew(t *testing.T) {
	window := webhook.DefaultReplayWindow
	skew := window - 10*time.Second
	behind := time.Now().Add(-skew)
	eventTime := behind.Add(-skew)
	ts := fmt.Sprintf("%d", eventTime.Unix())
	key := []byte(testHMACKey)

	c := webhookClient(t, webhookPolicy(), webhookTemplate(), webhookSecret(key))
	store := webhook.NewK8sReplayStore(c, "replay-state")
	// Advance the handler's clock slightly on each call so run names differ.
	var ticks time.Duration
	handler := &webhook.Handler{Config: webhook.HandlerConfig{
		Client: c, Replay: store, AuditLogger: &captureWebhookLogger{},
		ReplayWindow: window, Now: func() time.Time { ticks++; return behind.Add(ticks) },
	}}
	deliver := func() int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(testAlertBody))
		req.Header.Set(webhook.TimestampHeader, ts)
		req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(testAlertBody), key))
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp.Code
	}

	if code := deliver(); code != http.StatusCreated {
		t.Fatalf("first delivery status = %d, want 201", code)
	}
	if err := store.SweepExpired(t.Context()); err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if code := deliver(); code != http.StatusConflict {
		t.Fatalf("redelivery after sweep status = %d, want 409", code)
	}
	if runs := listRuns(t, c); len(runs) != 1 {
		t.Fatalf("expected one AgentRun, got %d", len(runs))
	}
}

func TestK8sReplayStoreSweepRemovesOnlyExpiredRecords(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := webhook.NewK8sReplayStore(c, "replay-state")
	ctx := context.Background()
	if err := s.CheckAndRecord(ctx, "expired", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAndRecord(ctx, "live", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := s.SweepExpired(ctx); err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}

	var records corev1.ConfigMapList
	if err := c.List(ctx, &records, client.InNamespace("replay-state")); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 1 || records.Items[0].Data["event"] != "live" {
		t.Fatalf("records after sweep = %v, want only the live record", records.Items)
	}
	if err := s.CheckAndRecord(ctx, "live", time.Now().Add(5*time.Minute)); err != webhook.ErrReplay {
		t.Fatalf("live record after sweep: got %v, want ErrReplay", err)
	}
}
