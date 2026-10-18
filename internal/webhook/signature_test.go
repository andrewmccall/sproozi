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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/webhook"
)

func TestVerifySignature(t *testing.T) {
	key := []byte("test-secret")
	body := []byte(`{"eventContext":{"alertname":"CrashLoopBackOff"}}`)
	now := time.Now()
	at := func(offset time.Duration) string { return fmt.Sprintf("%d", now.Add(offset).Unix()) }
	current := at(0)
	errAny := errors.New("any error")

	for _, tc := range []struct {
		name      string
		signature string
		timestamp string
		body      []byte
		want      error
	}{
		{name: "valid", signature: webhook.ComputeSignature(current, body, key), timestamp: current},
		{name: "missing signature", timestamp: current, want: webhook.ErrMissingSignature},
		{name: "bad format", signature: "notsha256=abc", timestamp: current, want: webhook.ErrInvalidSignatureFormat},
		{name: "bad hex", signature: "sha256=NOTVALIDHEX!!!", timestamp: current, want: errAny},
		{name: "wrong key", signature: webhook.ComputeSignature(current, body, []byte("correct-key")), timestamp: current, want: webhook.ErrSignatureMismatch},
		{name: "missing timestamp", signature: webhook.ComputeSignature("", body, key), want: webhook.ErrMissingTimestamp},
		{name: "invalid timestamp", signature: webhook.ComputeSignature("notanumber", body, key), timestamp: "notanumber", want: errAny},
		{name: "stale timestamp", signature: webhook.ComputeSignature(at(-10*time.Minute), body, key), timestamp: at(-10 * time.Minute), want: webhook.ErrStaleTimestamp},
		{name: "future timestamp", signature: webhook.ComputeSignature(at(10*time.Minute), body, key), timestamp: at(10 * time.Minute), want: webhook.ErrStaleTimestamp},
		{name: "signature covers body", signature: webhook.ComputeSignature(current, body, key), timestamp: current,
			body: []byte(`{"eventContext":{"alertname":"EVIL"}}`), want: webhook.ErrSignatureMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.body == nil {
				tc.body = body
			}
			_, err := webhook.VerifySignature(tc.signature, tc.timestamp, tc.body, key, now, webhook.DefaultReplayWindow)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("expected valid signature to pass, got: %v", err)
			case tc.want == errAny && err == nil:
				t.Fatal("expected an error, got nil")
			case tc.want != nil && tc.want != errAny && err != tc.want:
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
