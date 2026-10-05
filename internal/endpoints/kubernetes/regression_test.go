package kubernetes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type reviewBody struct{ ctx context.Context }

func (b reviewBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return copy(p, "event"), io.EOF
}
func (b reviewBody) Close() error { return nil }
func TestWatchBodyRemainsReadableAfterReturn(t *testing.T) {
	u, _ := url.Parse("https://kubernetes.test")
	h := NewHandler(HandlerConfig{Upstream: u, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reviewBody{ctx: r.Context()}}, nil
	})}})
	resp, err := h.authorizeAndForward(context.Background(), identity(), httptest.NewRequest("GET", "/api/v1/namespaces/demo/pods?watch=true&timeoutSeconds=30", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err = io.ReadAll(resp.Body); err != nil {
		t.Fatalf("watch response body is already cancelled when returned: %v", err)
	}
}
