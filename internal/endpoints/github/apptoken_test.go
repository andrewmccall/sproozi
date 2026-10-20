package github

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFetchInstallationTokenRejectsUnsafeProviderResponses(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "empty", body: `{"token":"","expires_at":"2030-01-01T00:00:00Z"}`},
		{name: "header-break", body: `{"token":"good\r\nX-Leak: yes","expires_at":"2030-01-01T00:00:00Z"}`},
		{name: "expired", body: fmt.Sprintf(`{"token":"good","expires_at":%q}`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))},
		{name: "valid", body: fmt.Sprintf(`{"token":"good","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &AppTokenProvider{appID: 1, installationID: 2, privateKey: key, httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header), Request: request}, nil
			})}, apiBaseURL: "https://api.fixture.invalid"}
			got, _, err := provider.fetchInstallationToken(t.Context(), "fixture-jwt")
			if tc.valid && (err != nil || got != "good") {
				t.Fatalf("valid token rejected: token=%q err=%v", got, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("unsafe token accepted: %q", got)
			}
		})
	}
}

const pkcs8PEMType = "PRIVATE KEY"

func TestNewAppTokenProviderAcceptsPKCS1AndPKCS8RSAKeys(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"PKCS#1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)},
		"PKCS#8": {Type: pkcs8PEMType, Bytes: pkcs8},
	} {
		provider, err := NewAppTokenProvider(1, 2, pem.EncodeToMemory(block), http.DefaultClient)
		if err != nil {
			t.Fatalf("%s key rejected: %v", name, err)
		}
		if !provider.privateKey.Equal(key) {
			t.Fatalf("%s key parsed to a different key", name)
		}
	}
}

func TestNewAppTokenProviderRejectsNonRSAKeys(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAppTokenProvider(1, 2, pem.EncodeToMemory(&pem.Block{Type: pkcs8PEMType, Bytes: der}), http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "must be RSA") {
		t.Fatalf("non-RSA key error = %v", err)
	}
	_, err = NewAppTokenProvider(1, 2, pem.EncodeToMemory(&pem.Block{Type: pkcs8PEMType, Bytes: []byte("junk")}), http.DefaultClient)
	if err == nil || strings.Contains(err.Error(), "ParsePKCS8PrivateKey") {
		t.Fatalf("malformed key error = %v", err)
	}
}
