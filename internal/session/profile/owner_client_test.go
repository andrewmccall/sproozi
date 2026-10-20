package profile

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type ownerRoundTripper func(*http.Request) (*http.Response, error)

func (f ownerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOwnerClientPreservesSafeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   Code
	}{
		{400, `{"error":"invalid_request"}`, CodeInvalid},
		{403, `{"error":"denied"}`, CodeDenied},
		{409, `{"error":"conflict"}`, CodeConflict},
		{422, `{"error":"unsupported"}`, CodeUnsupported},
		{503, `{"error":"unavailable"}`, CodeUnavailable},
		{502, "upstream-private-secret", CodeUnavailable},
		{409, `{"error":"unavailable"}`, CodeUnavailable},
	} {
		c := Client{base: "https://owner.invalid", maxBody: 1024, http: &http.Client{Transport: ownerRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}}
		var result any
		err := c.CallOwner(context.Background(), "info", struct{}{}, &result)
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Code != tc.code || remote.Status != tc.status || strings.Contains(err.Error(), "upstream-private-secret") {
			t.Fatalf("status %d: %v", tc.status, err)
		}
	}
}

func TestOwnerClientKeepsTransportFailureOutOfTheMessage(t *testing.T) {
	refused := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	c := Client{base: "https://owner.invalid", maxBody: 1024, http: &http.Client{Transport: ownerRoundTripper(
		func(*http.Request) (*http.Response, error) { return nil, refused })}}
	var result any
	err := c.CallOwner(context.Background(), "info", struct{}{}, &result)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != CodeUnavailable || !errors.Is(remote.Transport, refused) ||
		strings.Contains(err.Error(), "connection refused") ||
		!strings.HasSuffix(err.Error(), "session profile: unavailable (no HTTP response)") {
		t.Fatalf("transport failure: %v", err)
	}
}
