package remote

import (
	"os"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/work"
)

// TestLiveExecutor reaches a real remote executor through a kubeconfig context
// or file. It runs only when SPROOZI_REMOTE_CONTEXT is set (use "default" for
// the sproozi context); SPROOZI_REMOTE_NAMESPACE overrides the namespace and
// SPROOZI_REMOTE_EXPECT_ERROR, when set, is text the failure must contain.
func TestLiveExecutor(t *testing.T) {
	value, ok := os.LookupEnv("SPROOZI_REMOTE_CONTEXT")
	if !ok {
		t.Skip("SPROOZI_REMOTE_CONTEXT is not set")
	}
	if value == "default" {
		value = ""
	}
	expect := os.Getenv("SPROOZI_REMOTE_EXPECT_ERROR")
	sel, err := Select(value, os.Getenv("SPROOZI_REMOTE_NAMESPACE"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("selected %s", sel)
	e, err := Discover(t.Context(), sel)
	if expect != "" {
		if err == nil || !strings.Contains(err.Error(), expect) {
			t.Fatalf("got %v, want an error containing %q", err, expect)
		}
		t.Logf("refused as expected: %v", err)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("executor %q (%s, checkpoints up to %d bytes)", e.Name, e.Harness, e.MaxCheckpointBytes)
	client, err := NewClient(e)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sessions, err := work.ClientFor(client).List(t.Context(), work.SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("remote work catalogue listed %d sessions as the home principal", len(sessions))
}
