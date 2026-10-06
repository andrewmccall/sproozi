package github

import (
	"crypto/rand"
	"crypto/rsa"
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
