package certificates

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var errStopped = errors.New("test: controller stopped")

// stopWritesAfter fails the controller's nth API write (counting from zero)
// and every write after it, as if the controller had stopped there, until
// resume is called. It returns the number of writes attempted.
func stopWritesAfter(client *fake.Clientset, n int) (attempted *int, resume func()) {
	count, active := 0, true
	client.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !active || (action.GetVerb() != "create" && action.GetVerb() != "update") {
			return false, nil, nil
		}
		count++
		if count > n {
			return true, nil, errStopped
		}
		return false, nil, nil
	})
	return &count, func() { active = false }
}

var servingChange = regexp.MustCompile(`\| serving=\w+ `)

// settledCluster describes the cluster without whether the last reconcile
// changed the serving Secret.
func settledCluster(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	return servingChange.ReplaceAllString(describeCluster(t, client, nil), "| ")
}

// A controller stopped after any single API write of a rotation request's
// reconcile, then restarted with fresh scratch space, ends with the same
// authority Secret, status annotations and trust ConfigMaps as one that was
// not stopped.
func TestInterruptedReconcileConvergesAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		from     string
		requests []string
		request  string
	}{
		{"single", nil, ""},
		{"single", nil, "renew"},
		{"single", nil, "prepare"},
		{"prepared", []string{"prepare"}, "switch"},
		{"switched", []string{"prepare", "switch"}, "rollback"},
		{"switched", []string{"prepare", "switch"}, "retire"},
		{"switched", []string{"prepare", "switch"}, "replace"},
	} {
		c, built := setup(t)
		step(t, c, built, tc.requests...)
		reference := cloneCluster(t, built)
		if tc.request != "" {
			request(t, reference, tc.request)
		}
		counted, _ := stopWritesAfter(reference, 1<<30)
		reconcileOK(t, controller(t, reference))
		writes := *counted
		reconcileOK(t, controller(t, reference))
		want := settledCluster(t, reference)
		for n := range writes {
			t.Run(fmt.Sprintf("%s from %s stopped at write %d", tc.request, tc.from, n), func(t *testing.T) {
				client := cloneCluster(t, built)
				if tc.request != "" {
					request(t, client, tc.request)
				}
				attempted, resume := stopWritesAfter(client, n)
				_, _ = controller(t, client).Reconcile(context.Background())
				if *attempted <= n {
					t.Fatalf("write %d was not reached", n)
				}
				resume()
				reconcileOK(t, controller(t, client))
				reconcileOK(t, controller(t, client))
				if got := settledCluster(t, client); got != want {
					t.Errorf("did not converge:\n got %s\nwant %s", got, want)
				}
				serving, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-gateway-tls", metav1.GetOptions{})
				if err != nil || len(serving.Data[corev1.TLSCertKey]) == 0 {
					t.Errorf("no serving certificate after restart: %v", err)
				}
			})
		}
	}
}
