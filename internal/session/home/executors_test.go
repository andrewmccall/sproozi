package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/remote"
	"github.com/google/uuid"
)

const testExecutorName = "engineering"

// testKubeconfig is an absolute path on every platform; selections only record
// it, so the file need not exist.
var testKubeconfig = filepath.Join(os.TempDir(), "fixture", "kubeconfig")

func testSelection(name string) remote.Selection {
	return remote.Selection{Kubeconfig: []string{testKubeconfig}, Context: name, Namespace: "team-a"}
}

// describing returns a discover function that finds one executor per context.
func describing(executors map[string]remote.Executor) func(context.Context, remote.Selection) (remote.Executor, error) {
	return func(_ context.Context, sel remote.Selection) (remote.Executor, error) {
		e, ok := executors[sel.Context]
		if !ok {
			return remote.Executor{}, errors.New(`no remote executor in namespace "team-a"`)
		}
		e.Selection = sel
		return e, nil
	}
}

func TestReachExecutorRecordsAndReopensIt(t *testing.T) {
	t.Parallel()
	s, _, _, _ := testService(t)
	s.discover = describing(map[string]remote.Executor{
		remote.DefaultContext: {Name: testExecutorName, Harness: harness.NativeCodex, MaxCheckpointBytes: testLimit},
		"file":                {Name: testExecutorName, Harness: harness.NativeCodex, MaxCheckpointBytes: testLimit},
	})
	name, err := s.reachExecutor(t.Context(), testSelection(remote.DefaultContext))
	if err != nil || name != testExecutorName {
		t.Fatalf("reached %q: %v", name, err)
	}
	if _, ok := s.placement(testExecutorName).(*remote.Client); !ok || s.workSource(testExecutorName) == nil {
		t.Fatal("executor is not a placement and a work source")
	}
	path := filepath.Join(s.config.Directory, executorsFile)
	// The platform's own privacy rule: owner-only mode bits, or an owner-only ACL
	// on Windows.
	if err := privatefs.Check(path, false); err != nil {
		t.Fatalf("executors record is not private: %v", err)
	}
	// Another context that reaches the same executor re-points the record.
	if name, err = s.reachExecutor(t.Context(), testSelection("file")); err != nil || name != testExecutorName {
		t.Fatalf("second selection: %q %v", name, err)
	}
	reopened := &Service{config: s.config, placements: map[string]session.Placement{}}
	if err = reopened.openExecutors(); err != nil {
		t.Fatal(err)
	}
	client := reopened.remotes[testExecutorName]
	if client == nil || client.Executor().Context != "file" || reopened.placements[testExecutorName] != client {
		t.Fatalf("reopened records: %+v", reopened.remotes)
	}
	// A move to the recorded executor by name passes the destination check.
	if err = s.moveDestinationProblem(testExecutorName); err != nil {
		t.Fatal(err)
	}
}

func TestReachExecutorRefusesWhatCannotMoveBothWays(t *testing.T) {
	t.Parallel()
	s, _, _, _ := testService(t)
	s.discover = describing(map[string]remote.Executor{
		"small":  {Name: testExecutorName, Harness: harness.NativeCodex, MaxCheckpointBytes: testLimit / 2},
		"shadow": {Name: testLocal, Harness: harness.NativeCodex, MaxCheckpointBytes: testLimit},
		"alias":  {Name: LocalAlias, Harness: harness.NativeCodex, MaxCheckpointBytes: testLimit},
	})
	for name, want := range map[string]string{
		"small":   "they must be equal",
		"shadow":  "this machine's own name",
		"alias":   "this machine's own name",
		"missing": "no remote executor",
	} {
		_, err := s.reachExecutor(t.Context(), testSelection(name))
		if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, ErrRejected) {
			t.Errorf("%s: %v, want rejected %q", name, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(s.config.Directory, executorsFile)); !os.IsNotExist(err) {
		t.Fatalf("refused executor was recorded: %v", err)
	}
}

func TestControlRequestsCarryASelectionOnlyForMoveAndPull(t *testing.T) {
	t.Parallel()
	sel := testSelection(remote.DefaultContext)
	id, operation := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name string
		q    Request
		ok   bool
	}{
		{"move to a context", Request{Action: ActionMove, ID: id, OperationID: operation, Remote: &sel}, true},
		{"move to a name", Request{Action: ActionMove, ID: id, OperationID: operation, Destination: LocalAlias}, true},
		{"move to both", Request{Action: ActionMove, ID: id, OperationID: operation, Destination: "x", Remote: &sel}, false},
		{"move to neither", Request{Action: ActionMove, ID: id, OperationID: operation}, false},
		{"pull from a context", Request{Action: ActionPull, ID: id, OperationID: operation, Remote: &sel}, true},
		{"pull a retained session", Request{Action: ActionPull, ID: id, OperationID: operation}, true},
		{"stop with a context", Request{Action: ActionStop, ID: id, OperationID: operation, Remote: &sel}, false},
		{"relative kubeconfig", Request{Action: ActionPull, ID: id, OperationID: operation,
			Remote: &remote.Selection{Kubeconfig: []string{"kubeconfig"}, Context: "x", Namespace: "y"}}, false},
	} {
		if err := validateRequest(tc.q); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
