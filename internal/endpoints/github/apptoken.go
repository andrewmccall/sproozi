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

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenProvider provides a short-lived GitHub App installation access token.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// AppTokenProvider authenticates as a GitHub App and vends cached
// installation tokens, refreshing them before expiry.
type AppTokenProvider struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	httpClient     *http.Client
	apiBaseURL     string // e.g. "https://api.github.com"

	mu      sync.Mutex
	cached  string
	expires time.Time
}

// NewAppTokenProvider parses the PEM-encoded PKCS#1 RSA private key and
// returns a provider that generates GitHub App installation tokens.
func NewAppTokenProvider(appID, installationID int64, privateKeyPEM []byte, httpClient *http.Client) (*AppTokenProvider, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("capabilityproxy: no PEM block found in private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("capabilityproxy: failed to parse RSA private key: %w", err)
	}
	return &AppTokenProvider{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
		httpClient:     httpClient,
		apiBaseURL:     "https://api.github.com",
	}, nil
}

// Token returns a valid installation access token, refreshing 30 s before expiry.
func (a *AppTokenProvider) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cached != "" && time.Now().Before(a.expires.Add(-30*time.Second)) {
		return a.cached, nil
	}

	jwt, err := a.generateJWT()
	if err != nil {
		return "", err
	}

	token, expires, err := a.fetchInstallationToken(ctx, jwt)
	if err != nil {
		return "", err
	}

	a.cached = token
	a.expires = expires
	return token, nil
}

// generateJWT builds and signs a GitHub App RS256 JWT.
func (a *AppTokenProvider) generateJWT() (string, error) {
	now := time.Now()

	header, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "RS256"})
	payload, _ := json.Marshal(map[string]any{
		"iss": fmt.Sprintf("%d", a.appID),
		"iat": now.Unix() - 60,  // 60 s ago to handle clock skew
		"exp": now.Unix() + 540, // 9-minute window (max allowed is 10 min)
	})

	h := base64.RawURLEncoding.EncodeToString(header)
	p := base64.RawURLEncoding.EncodeToString(payload)
	sigInput := h + "." + p

	hashed := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("capabilityproxy: failed to sign JWT: %w", err)
	}

	return sigInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// fetchInstallationToken exchanges the App JWT for an installation access token.
func (a *AppTokenProvider) fetchInstallationToken(ctx context.Context, jwt string) (token string, expires time.Time, err error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBaseURL, a.installationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("capabilityproxy: failed to build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("capabilityproxy: installation token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("capabilityproxy: installation token returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("capabilityproxy: failed to decode token response: %w", err)
	}
	// Treat the provider response as untrusted input at this boundary. An empty,
	// expired, or header-breaking token must never become an Authorization value
	// on a subsequent request.
	if result.Token == "" || len(result.Token) > 4096 || strings.ContainsAny(result.Token, "\r\n") || result.ExpiresAt.Before(time.Now().Add(30*time.Second)) {
		return "", time.Time{}, fmt.Errorf("capabilityproxy: installation token response is invalid")
	}

	return result.Token, result.ExpiresAt, nil
}

// FakeTokenProvider is an exported test double for TokenProvider.
type FakeTokenProvider struct {
	// GitHubToken is returned by Token.
	GitHubToken string
	// Err is returned by Token when non-nil.
	Err error
}

// Token returns the pre-configured token or error.
func (f *FakeTokenProvider) Token(_ context.Context) (string, error) {
	return f.GitHubToken, f.Err
}
