package kubernetes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
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

// slowBody returns one event after a delay, or the request's context error.
type slowBody struct {
	ctx   context.Context
	delay time.Duration
}

func (b slowBody) Read(p []byte) (int, error) {
	select {
	case <-time.After(b.delay):
		return copy(p, "event"), io.EOF
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}
func (b slowBody) Close() error { return nil }

// A watch lasts its requested timeoutSeconds even when the upstream client's
// overall Timeout is shorter; other reads keep that Timeout.
func TestWatchOutlastsClientTimeoutButReadsDoNot(t *testing.T) {
	u, _ := url.Parse("https://kubernetes.test")
	deadlines := make(chan time.Time, 2)
	h := NewHandler(HandlerConfig{Upstream: u, Client: &http.Client{Timeout: 50 * time.Millisecond,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, _ := r.Context().Deadline()
			deadlines <- deadline
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: slowBody{ctx: r.Context(), delay: 300 * time.Millisecond}}, nil
		})}})
	started := time.Now()
	resp, err := h.authorizeAndForward(context.Background(), identity(),
		httptest.NewRequest("GET", "/api/v1/namespaces/demo/pods?watch=true&timeoutSeconds=300", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(resp.Body); err != nil {
		t.Fatalf("watch ended at the client Timeout: %v", err)
	}
	_ = resp.Body.Close()
	if got, want := (<-deadlines).Sub(started), 300*time.Second+watchMargin; got < want-time.Second || got > want+time.Second {
		t.Errorf("watch deadline %v after start, want %v", got, want)
	}
	resp, err = h.authorizeAndForward(context.Background(), identity(),
		httptest.NewRequest("GET", "/api/v1/namespaces/demo/pods", nil))
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("list outlasted the client Timeout")
	}
}

// A watch opened late on an inspected connection ends with the connection's
// request deadline rather than outliving it.
func TestWatchDeadlineIsCappedByTheRequestDeadline(t *testing.T) {
	u, _ := url.Parse("https://kubernetes.test")
	deadlines := make(chan time.Time, 1)
	h := NewHandler(HandlerConfig{Upstream: u, Client: &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, _ := r.Context().Deadline()
			deadlines <- deadline
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
		})}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	resp, err := h.authorizeAndForward(ctx, identity(),
		httptest.NewRequest("GET", "/api/v1/namespaces/demo/pods?watch=true&timeoutSeconds=300", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-deadlines; !got.Equal(want) {
		t.Errorf("watch deadline %v, want the request deadline %v", got, want)
	}
}
