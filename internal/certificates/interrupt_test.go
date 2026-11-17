package certificates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/pki"
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

// consistent describes what is wrong between the published objects and the
// authority's record: the serving Secret must hold the record's named leaf,
// its ca.crt, the authority's trust.pem and the annotated trust ConfigMap
// must all equal the record's trusted certificates in order, and no retired
// generation's key may remain in the authority Secret. After the reviewer's
// check.
func consistent(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	ctx := context.Background()
	a, err := client.CoreV1().Secrets(systemNamespace).Get(ctx, "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var st pki.State
	if err = json.Unmarshal(a.Data["state.json"], &st); err != nil {
		t.Fatal(err)
	}
	s, err := client.CoreV1().Secrets(systemNamespace).Get(ctx, "sproozi-gateway-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	if len(s.Data[corev1.TLSCertKey]) == 0 || !bytes.Equal(s.Data[corev1.TLSCertKey], a.Data[st.Leaf+".pem"]) ||
		!bytes.Equal(s.Data[corev1.TLSPrivateKeyKey], a.Data[st.Leaf+".key"]) {
		problems = append(problems, "serving Secret does not hold the record's leaf")
	}
	var bundle []byte
	for _, id := range st.Trusted {
		bundle = append(bundle, a.Data[fmt.Sprintf("ca-%d.pem", id)]...)
	}
	if !bytes.Equal(a.Data["trust.pem"], bundle) {
		problems = append(problems, "trust.pem is not the record's bundle")
	}
	if !bytes.Equal(s.Data[CABundleKey], bundle) {
		problems = append(problems, "ca.crt is not the record's bundle")
	}
	cm, err := client.CoreV1().ConfigMaps("sproozi-agents").Get(ctx, a.Annotations[TrustAnnotation], metav1.GetOptions{})
	if err != nil || cm.Data[trustBundleKey] != string(bundle) {
		problems = append(problems, fmt.Sprintf("trust ConfigMap %q is not the record's bundle (%v)", a.Annotations[TrustAnnotation], err))
	}
	for _, g := range st.Generations {
		if _, ok := a.Data[fmt.Sprintf("ca-%d.key", g.ID)]; ok && g.Retired {
			problems = append(problems, fmt.Sprintf("retired key ca-%d.key remains", g.ID))
		}
	}
	return strings.Join(problems, "; ")
}

// A controller stopped after any single API write of a rotation request's
// reconcile, then restarted with fresh scratch space, ends with the same
// authority Secret, status annotations and trust ConfigMaps as one that was
// not stopped, and consistent: the published leaf and bundles are the
// record's.
func TestInterruptedReconcileConvergesAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		from     string
		requests []string
		request  string
	}{
		{stateSingle, nil, ""},
		{stateSingle, nil, requestRenew},
		{stateSingle, nil, requestPrepare},
		{statePrepared, []string{requestPrepare}, requestSwitch},
		{stateSwitched, []string{requestPrepare, requestSwitch}, requestRollback},
		{stateSwitched, []string{requestPrepare, requestSwitch}, requestRetire},
		{stateSwitched, []string{requestPrepare, requestSwitch}, requestReplace},
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
		if problems := consistent(t, reference); problems != "" {
			t.Errorf("%s from %s, uninterrupted: %s", tc.request, tc.from, problems)
		}
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
				if problems := consistent(t, client); problems != "" {
					t.Error(problems)
				}
			})
		}
	}
}
