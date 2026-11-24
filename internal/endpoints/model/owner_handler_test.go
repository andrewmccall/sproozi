package model_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
)

// A call the handler admits is claimed by the goroutine that serves it, so
// the owner check is on for every handler-driven test and not only for the
// calls the end tests build by hand.
func TestHandlerClaimsEachCallForItsGoroutine(t *testing.T) {
	var mu sync.Mutex
	var checks []func()
	var previous func() func()
	previous = model.SwapOwnerCheck(func() func() {
		check := previous()
		mu.Lock()
		defer mu.Unlock()
		checks = append(checks, check)
		return check
	})
	defer model.SwapOwnerCheck(previous)

	runModelCase(t, modelCase{name: "completed", path: chatPath, body: chatBody})

	mu.Lock()
	defer mu.Unlock()
	if len(checks) != 1 {
		t.Fatalf("the handler claimed %d calls, want 1", len(checks))
	}
	// This goroutine did not serve the call, so ending it here panics.
	defer func() {
		if p := fmt.Sprint(recover()); !strings.Contains(p, "was ended on goroutine") {
			t.Fatalf("a call ended off its goroutine without the check firing: %q", p)
		}
	}()
	checks[0]()
}
