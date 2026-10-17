package model_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"

	"github.com/andrewmccall/sproozi/internal/modelauth"
)

func TestConcurrentInferenceWaitsForSharedAllowance(t *testing.T) {
	for name, body := range map[string]string{"unbounded": `{"model":"test"}`, "bounded": `{"model":"test","max_tokens":60}`} {
		t.Run(name, func(t *testing.T) {
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(releaseFirst) })
			var calls atomic.Int32
			h := newHandler(t, testExecutionGrant(), testModelUpstream)
			h.Config.Identity.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 100}
			h.Config.ProviderAuth = modelauth.APIKey{Key: testOpenAIKey}
			h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					close(firstStarted)
					<-releaseFirst
				}
				return modelResponse(r, `{"usage":{"total_tokens":12}}`), nil
			})}
			invoke := func(done chan<- *httptest.ResponseRecorder) {
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				done <- response
			}
			first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			go invoke(first)
			<-firstStarted
			go invoke(second)
			select {
			case result := <-second:
				t.Fatalf("concurrent inference returned before settlement: %d %s", result.Code, result.Body)
			case <-time.After(50 * time.Millisecond):
			}
			release.Do(func() { close(releaseFirst) })
			for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
				select {
				case result := <-done:
					if result.Code != http.StatusOK {
						t.Fatalf("inference failed: %d %s", result.Code, result.Body)
					}
				case <-time.After(time.Second):
					t.Fatal("inference did not resume after settlement")
				}
			}
			state := observedState(t, h.Config.BudgetTracker, testRunUID)
			if calls.Load() != 2 || state.UnitsUsed != 24 || state.ReservedUnits != 0 {
				t.Fatalf("concurrent usage lost: calls=%d state=%+v", calls.Load(), state)
			}
		})
	}
}
