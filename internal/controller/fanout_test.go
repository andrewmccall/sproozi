package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestNextQueuedRequestsReturnsOneGlobalSuccessor(t *testing.T) {
	first := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "later", Namespace: "team-b", CreationTimestamp: metav1.Time{Time: time.Unix(2, 0)}},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseQueued},
	}
	second := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: "team-a", CreationTimestamp: metav1.Time{Time: time.Unix(1, 0)}},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseQueued},
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
	}).WithObjects(first, second).Build()

	r := &AgentRunReconciler{Client: c}
	got := r.nextQueuedRequests(context.Background())
	if len(got) != 1 || got[0].Name != "next" || got[0].Namespace != "team-a" {
		t.Fatalf("nextQueuedRequests() = %#v, want only team-a/next", got)
	}
}
