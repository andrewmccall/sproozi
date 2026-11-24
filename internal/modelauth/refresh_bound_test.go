package modelauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// deadlineTransport never answers: it records the deadline of the request it
// is given and fails it, as a token endpoint that never replies would once
// that deadline passed.
type deadlineTransport struct {
	mu        sync.Mutex
	deadlines []time.Duration
}

func (d *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	left := time.Duration(-1)
	if deadline, ok := r.Context().Deadline(); ok {
		left = time.Until(deadline)
	}
	d.mu.Lock()
	d.deadlines = append(d.deadlines, left)
	d.mu.Unlock()
	return nil, errors.New("no reply")
}

// TestChatGPTRefreshStepIsBoundedWhateverTheClient: a client with no timeout
// of its own, as a caller may supply, still gets a refresh bounded at
// refreshTimeout. Guards: the refresh step's own bound.
func TestChatGPTRefreshStepIsBoundedWhateverTheClient(t *testing.T) {
	transport := &deadlineTransport{}
	auth := NewChatGPT(expiredStore(), &http.Client{Transport: transport})
	auth.tokenEndpoint = "https://token.invalid/oauth/token"
	if _, err := authorizeOnce(auth); err == nil {
		t.Fatal("authorized without a refresh")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.deadlines) != 1 {
		t.Fatalf("refresh requests = %d, want 1", len(transport.deadlines))
	}
	if left := transport.deadlines[0]; left <= refreshTimeout-5*time.Second || left > 30*time.Second {
		t.Fatalf("the refresh had %s left, want its own 30 s bound", left)
	}
}

// TestChatGPTRechecksTheSessionUnderTheLock: a request that found no usable
// session, then waited for the lock while an attempt finished and published
// one, uses it rather than reading the store again. Guards: the re-check in
// reloadOrRefresh.
func TestChatGPTRechecksTheSessionUnderTheLock(t *testing.T) {
	store := &memoryStore{session: fixtureSession()}
	auth := NewChatGPT(store, http.DefaultClient)
	auth.mu.Lock()
	done := make(chan error, 1)
	go func() {
		// Nothing is loaded yet, so this request goes for the lock.
		done <- auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil))
	}()
	time.Sleep(50 * time.Millisecond) // let it reach the lock
	// An attempt finishes while the request waits: it read the store once.
	if _, err := auth.reload(); err != nil {
		t.Fatal(err)
	}
	auth.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	settle(t, auth)
	if loads := store.loads.Load(); loads != 1 {
		t.Fatalf("store reads = %d, want 1: the waiting request read the store again", loads)
	}
}
