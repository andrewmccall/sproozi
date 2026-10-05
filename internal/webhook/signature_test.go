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
	"fmt"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/webhook"
)

func TestVerifySignature_Valid(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{"eventContext":{"alertname":"CrashLoopBackOff"}}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	sig := webhook.ComputeSignature(ts, body, key)

	_, err := webhook.VerifySignature(sig, ts, body, key, now, webhook.DefaultReplayWindow)
	if err != nil {
		t.Fatalf("expected valid signature to pass, got: %v", err)
	}
}

func TestVerifySignature_MissingSignature(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())

	_, err := webhook.VerifySignature("", ts, body, key, now, webhook.DefaultReplayWindow)
	if err != webhook.ErrMissingSignature {
		t.Fatalf("expected ErrMissingSignature, got: %v", err)
	}
}

func TestVerifySignature_BadFormat(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())

	_, err := webhook.VerifySignature("notsha256=abc", ts, body, key, now, webhook.DefaultReplayWindow)
	if err != webhook.ErrInvalidSignatureFormat {
		t.Fatalf("expected ErrInvalidSignatureFormat, got: %v", err)
	}
}

func TestVerifySignature_BadHex(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())

	_, err := webhook.VerifySignature("sha256=NOTVALIDHEX!!!", ts, body, key, now, webhook.DefaultReplayWindow)
	if err == nil {
		t.Fatal("expected error for invalid hex, got nil")
	}
}

func TestVerifySignature_WrongKey(t *testing.T) {
	body := []byte(`{}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	sig := webhook.ComputeSignature(ts, body, []byte("correct-key"))

	_, err := webhook.VerifySignature(sig, ts, body, []byte("wrong-key"), now, webhook.DefaultReplayWindow)
	if err != webhook.ErrSignatureMismatch {
		t.Fatalf("expected ErrSignatureMismatch, got: %v", err)
	}
}

func TestVerifySignature_MissingTimestamp(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()

	sig := webhook.ComputeSignature("", body, key)
	_, err := webhook.VerifySignature(sig, "", body, key, now, webhook.DefaultReplayWindow)
	if err != webhook.ErrMissingTimestamp {
		t.Fatalf("expected ErrMissingTimestamp, got: %v", err)
	}
}

func TestVerifySignature_InvalidTimestampFormat(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()

	sig := webhook.ComputeSignature("notanumber", body, key)
	_, err := webhook.VerifySignature(sig, "notanumber", body, key, now, webhook.DefaultReplayWindow)
	if err == nil {
		t.Fatal("expected error for invalid timestamp, got nil")
	}
}

func TestVerifySignature_StaleTimestamp(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()
	staleTime := now.Add(-10 * time.Minute)
	ts := fmt.Sprintf("%d", staleTime.Unix())
	sig := webhook.ComputeSignature(ts, body, key)

	_, err := webhook.VerifySignature(sig, ts, body, key, now, webhook.DefaultReplayWindow)
	if err != webhook.ErrStaleTimestamp {
		t.Fatalf("expected ErrStaleTimestamp, got: %v", err)
	}
}

func TestVerifySignature_FutureTimestamp(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{}`)
	now := time.Now()
	futureTime := now.Add(10 * time.Minute)
	ts := fmt.Sprintf("%d", futureTime.Unix())
	sig := webhook.ComputeSignature(ts, body, key)

	_, err := webhook.VerifySignature(sig, ts, body, key, now, webhook.DefaultReplayWindow)
	if err != webhook.ErrStaleTimestamp {
		t.Fatalf("expected ErrStaleTimestamp for far-future timestamp, got: %v", err)
	}
}

func TestVerifySignature_SignatureCoversBody(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{"eventContext":{"alertname":"CrashLoopBackOff"}}`)
	now := time.Now()
	ts := fmt.Sprintf("%d", now.Unix())
	sig := webhook.ComputeSignature(ts, body, key)

	// Tamper with body after signing.
	tamperedBody := []byte(`{"eventContext":{"alertname":"EVIL"}}`)
	_, err := webhook.VerifySignature(sig, ts, tamperedBody, key, now, webhook.DefaultReplayWindow)
	if err != webhook.ErrSignatureMismatch {
		t.Fatalf("expected ErrSignatureMismatch for tampered body, got: %v", err)
	}
}
