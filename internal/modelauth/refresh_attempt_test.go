package modelauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// settle waits until no reload or refresh is running, so a test may read
// what the last one left.
func settle(t *testing.T, auth *ChatGPT) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		auth.mu.Lock()
		idle := auth.inflight == nil
		auth.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a reload or refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// blockedEndpoint holds every refresh until release, then answers status.
func blockedEndpoint(status int, entered chan<- struct{}, release <-chan struct{}, calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(renewedBody))
	}
}

// TestChatGPTCallersLeaveAtTheirOwnDeadline: while a refresh is in flight,
// each caller, including the one that started it, returns at its own
// deadline or cancellation, and a caller that arrives joins the refresh in
// flight rather than starting another. Guards: Authorize's wait and the
// owned refresh in reloadOrRefresh.
func TestChatGPTCallersLeaveAtTheirOwnDeadline(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(blockedEndpoint(http.StatusServiceUnavailable, entered, release, &calls))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	auth := NewChatGPT(expiredStore(), &http.Client{Timeout: 30 * time.Second})
	auth.tokenEndpoint = server.URL

	authorize := func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil).WithContext(ctx)
		err := auth.Authorize(req)
		return time.Since(start), err
	}
	type result struct {
		took time.Duration
		err  error
	}
	starterCtx, cancelStarter := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelStarter()
	starter := make(chan result, 1)
	go func() { took, err := authorize(starterCtx); starter <- result{took, err} }()
	<-entered // the refresh is in flight

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	later, cancelLater := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelLater()
	var wg sync.WaitGroup
	results := make([]result, 2)
	for i, ctx := range []context.Context{cancelled, later} {
		wg.Go(func() { took, err := authorize(ctx); results[i] = result{took, err} })
	}
	patient := make(chan result, 1)
	go func() { took, err := authorize(context.Background()); patient <- result{took, err} }()

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	var first result
	select {
	case first = <-starter:
	case <-time.After(5 * time.Second):
		t.Fatal("the caller that started the refresh was held past its deadline")
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("waiters were held past their deadlines")
	}
	if !errors.Is(first.err, context.DeadlineExceeded) || first.took > time.Second {
		t.Fatalf("starter: %v after %s", first.err, first.took)
	}
	if !errors.Is(results[0].err, context.Canceled) || results[0].took > time.Second {
		t.Fatalf("cancelled waiter: %v after %s", results[0].err, results[0].took)
	}
	if !errors.Is(results[1].err, context.DeadlineExceeded) || results[1].took < 100*time.Millisecond || results[1].took > time.Second {
		t.Fatalf("waiter with a 150ms deadline: %v after %s", results[1].err, results[1].took)
	}
	close(release)
	got := <-patient
	var refreshErr *RefreshError
	if !errors.As(got.err, &refreshErr) || refreshErr.Refused {
		t.Fatalf("waiter without a deadline: %v", got.err)
	}
	settle(t, auth)
	if calls.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1: a waiter started a second refresh", calls.Load())
	}
}

// TestChatGPTNoLockIsHeldAcrossARefresh: while a refresh is in flight, the
// lock is free, so nothing that needs it waits on the network. Guards:
// reloadOrRefresh releasing mu before it waits.
func TestChatGPTNoLockIsHeldAcrossARefresh(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(blockedEndpoint(http.StatusOK, entered, release, &calls))
	defer server.Close()
	auth := NewChatGPT(expiredStore(), server.Client())
	auth.tokenEndpoint = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil).WithContext(ctx))
	}()
	<-entered
	locked := make(chan struct{})
	go func() { auth.mu.Lock(); close(locked); auth.mu.Unlock() }()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the lock was held across the refresh")
	}
	cancel()
	close(release)
	settle(t, auth)
}

// recordingStore records the deadline of every context it is called with.
type recordingStore struct {
	ctxStore
	mu        sync.Mutex
	deadlines []time.Duration
}

func (s *recordingStore) record(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		s.deadlines = append(s.deadlines, -1)
		return
	}
	s.deadlines = append(s.deadlines, time.Until(deadline))
}

func (s *recordingStore) Load(ctx context.Context) (Session, error) {
	s.record(ctx)
	return s.ctxStore.Load(ctx)
}

func (s *recordingStore) Replace(ctx context.Context, before, after Session) error {
	s.record(ctx)
	return s.ctxStore.Replace(ctx, before, after)
}

// TestChatGPTEachStepIsBoundedAndOutlivesItsCaller: a caller that has
// already left still gets its unsaved refresh saved, without a second
// refresh, and every store call runs under its own 30-second bound rather
// than the caller's context. Guards: the pending save and refreshTimeout.
func TestChatGPTEachStepIsBoundedAndOutlivesItsCaller(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(renewedBody))
	}))
	defer server.Close()
	inner := expiredStore()
	inner.fail = true // the first save fails, so the refresh is left unsaved
	store := &recordingStore{ctxStore: ctxStore{inner}}
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	if _, err := authorizeOnce(auth); err == nil {
		t.Fatal("an unsaved refresh was used")
	}
	// A caller that has already left starts the retry of the save.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	_ = auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil).WithContext(gone))
	settle(t, auth)
	if inner.session.RefreshToken != renewedRefresh || calls.Load() != 1 {
		t.Fatalf("stored %q after %d refreshes: the unsaved refresh was lost or repeated", inner.session.RefreshToken, calls.Load())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deadlines) < 4 {
		t.Fatalf("store calls = %d", len(store.deadlines))
	}
	for i, left := range store.deadlines {
		if left <= refreshTimeout-5*time.Second || left > 30*time.Second {
			t.Fatalf("store call %d had %s left, want a 30s bound of its own", i, left)
		}
	}
}
