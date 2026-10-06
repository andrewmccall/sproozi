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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureHeader     = "X-Sproozi-Signature"
	TimestampHeader     = "X-Sproozi-Timestamp"
	DefaultReplayWindow = 5 * time.Minute
	hmacKeyField        = "hmac-key"
)

var (
	ErrMissingSignature       = errors.New("webhook: missing X-Sproozi-Signature header")
	ErrInvalidSignatureFormat = errors.New("webhook: signature header must be 'sha256=<hex>'")
	ErrSignatureMismatch      = errors.New("webhook: signature mismatch")
	ErrMissingTimestamp       = errors.New("webhook: missing X-Sproozi-Timestamp header")
	ErrInvalidTimestamp       = errors.New("webhook: invalid X-Sproozi-Timestamp header")
	ErrStaleTimestamp         = errors.New("webhook: event timestamp is outside replay window")
	ErrNoHMACKey              = errors.New("webhook: secret does not contain 'hmac-key' entry")
)

func VerifySignature(signatureHeader, timestampHeader string, rawBody, key []byte, now time.Time, replayWindow time.Duration) (time.Time, error) {
	if signatureHeader == "" {
		return time.Time{}, ErrMissingSignature
	}
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return time.Time{}, ErrInvalidSignatureFormat
	}
	sigHex := strings.TrimPrefix(signatureHeader, "sha256=")
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrInvalidSignatureFormat, err)
	}

	if timestampHeader == "" {
		return time.Time{}, ErrMissingTimestamp
	}
	tsUnix, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrInvalidTimestamp, err)
	}

	eventTime := time.Unix(tsUnix, 0)
	age := now.Sub(eventTime)
	if age > replayWindow || age < -replayWindow {
		return time.Time{}, ErrStaleTimestamp
	}

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(timestampHeader))
	_, _ = mac.Write(rawBody)
	if !hmac.Equal(mac.Sum(nil), sigBytes) {
		return time.Time{}, ErrSignatureMismatch
	}

	return eventTime, nil
}

func ComputeSignature(timestampHeader string, rawBody, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(timestampHeader))
	_, _ = mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
