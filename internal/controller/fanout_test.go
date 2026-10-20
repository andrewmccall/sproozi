package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestNonTerminalRequestsExcludesTerminalRuns(t *testing.T) {
	runs := []sprooziv1alpha1.AgentRun{
		{ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: "a"}, Status: sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseQueued}},
		{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: "a"}, Status: sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseSucceeded}},
	}

	got := nonTerminalRequests(runs)
	if len(got) != 1 || got[0].Name != "queued" || got[0].Namespace != "a" {
		t.Fatalf("nonTerminalRequests() = %#v, want only a/queued", got)
	}
}

// Freed capacity can admit any queued run under a per-policy cap, not only the
// queue head, so a release wakes every queued run across namespaces.
func TestQueuedRequestsReturnsEveryQueuedRun(t *testing.T) {
	first := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "later", Namespace: "team-b", CreationTimestamp: metav1.Time{Time: time.Unix(2, 0)}},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseQueued},
	}
	second := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: "team-a", CreationTimestamp: metav1.Time{Time: time.Unix(1, 0)}},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseQueued},
	}
	running := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "team-a"},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseRunning},
	}
	scheme := runtime.NewScheme()
	if err := sprooziv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&sprooziv1alpha1.AgentRun{}, agentRunPhaseIndex, func(obj client.Object) []string {
		return []string{string(obj.(*sprooziv1alpha1.AgentRun).Status.Phase)}
	}).WithObjects(first, second, running).Build()

	r := &AgentRunReconciler{Client: c}
	got := r.queuedRequests(context.Background())
	names := make([]string, 0, len(got))
	for _, req := range got {
		names = append(names, req.String())
	}
	slices.Sort(names)
	if want := []string{"team-a/next", "team-b/later"}; !slices.Equal(names, want) {
		t.Fatalf("queuedRequests() = %v, want %v", names, want)
	}
}

func TestMergeRequestsDropsDuplicates(t *testing.T) {
	a := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "a", Name: "one"}}
	b := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "a", Name: "two"}}
	got := mergeRequests([]reconcile.Request{a}, []reconcile.Request{b, a})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("mergeRequests() = %v, want [a/one a/two]", got)
	}
}
