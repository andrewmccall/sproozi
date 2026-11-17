package certificates

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// clusterObjects is every object the controller reads or writes.
func clusterObjects(t *testing.T, client *fake.Clientset) (*corev1.SecretList, *corev1.ConfigMapList) {
	t.Helper()
	secrets, err := client.CoreV1().Secrets(systemNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cms, err := client.CoreV1().ConfigMaps("sproozi-agents").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return secrets, cms
}

func refusedCluster(t *testing.T) *fake.Clientset {
	t.Helper()
	c, client := setup(t)
	step(t, c, client, requestPrepare)
	editStoredState(t, client, func(st *pki.State) { st.Rotation.Stage = "bogus" })
	return client
}

// A reconcile that finds a record no phase describes, with no request or
// with any request but replace, fails naming what is wrong and writes
// nothing: the authority Secret, the serving Secret and the trust ConfigMaps
// are byte-identical, resource versions included, and the request stays for
// the operator to see. Guards the refusal in pki's load and the order in
// Reconcile that commits nothing before Ensure succeeds.
func TestAReconcileOfARefusedRecordWritesNothing(t *testing.T) {
	for _, req := range []string{"", requestRenew, requestPrepare, requestSwitch, requestRollback, requestRetire} {
		client := refusedCluster(t)
		if req != "" {
			request(t, client, req)
		}
		secrets, cms := clusterObjects(t, client)
		_, err := controller(t, client).Reconcile(context.Background())
		if err == nil || !strings.Contains(err.Error(), `pki: corrupt state: rotation stage "bogus" is not prepared or switched`) {
			t.Errorf("request %q: %v", req, err)
		}
		afterSecrets, afterCMs := clusterObjects(t, client)
		if !reflect.DeepEqual(secrets, afterSecrets) || !reflect.DeepEqual(cms, afterCMs) {
			t.Errorf("request %q changed the cluster", req)
		}
	}
}

// A generation's files that a prepare wrote in scratch space and an earlier
// controller committed to the authority Secret without its record are
// removed from the Secret by the next reconcile. Guards the
// uncommitted-generation loop in pki's destroy pass, through Reconcile.
func TestAReconcileRemovesAnUncommittedGenerationFromTheSecret(t *testing.T) {
	c, client := setup(t)
	step(t, c, client)
	ca, err := pki.NewCA("uncommitted", time.Now(), pki.DefaultPolicy.CAValidity)
	if err != nil {
		t.Fatal(err)
	}
	setAuthorityData(t, client, func(data map[string][]byte) { data["ca-2.pem"], data["ca-2.key"] = ca.Certificate, ca.Key })
	if _, err = controller(t, client).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	data := authorityData(t, client)
	for _, name := range []string{"ca-2.pem", "ca-2.key"} {
		if _, kept := data[name]; kept {
			t.Errorf("%s stayed in the authority Secret", name)
		}
	}
	if _, kept := data["ca-1.key"]; !kept {
		t.Error("the serving key was removed")
	}
}

// A key the reconcile does not need does not stop it: with the new
// generation's key broken while the old one serves, a renew request still
// renews and publishes the leaf and clears the request, and the reconcile
// then fails naming the key, so the log reports it every interval. Guards
// the ErrOutstanding handling in Reconcile and pki's checkKeys.
func TestABrokenUnusedKeyIsReportedAfterTheReconcile(t *testing.T) {
	c, client := setup(t)
	step(t, c, client, requestPrepare)
	setAuthorityData(t, client, func(data map[string][]byte) { data["ca-2.key"] = []byte("not a key\n") })
	request(t, client, requestRenew)
	before := serving(t, client).Leaf
	_, err := controller(t, client).Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "generation 2 key ca-2.key does not match its certificate") {
		t.Errorf("reconcile: %v", err)
	}
	if serving(t, client).Leaf.Equal(before) {
		t.Error("the renew request did not renew the serving leaf")
	}
	s, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil || s.Annotations[RotationRequest] != "" {
		t.Errorf("request not cleared: %v %v", s.Annotations, err)
	}
}

// A retire request with the new serving key broken is refused, and the
// outgoing key, the last that works, stays in the authority Secret. Guards
// the serving-key check in pki's RetireRotation, through Reconcile.
func TestARetireRequestKeepsTheLastWorkingKey(t *testing.T) {
	c, client := setup(t)
	step(t, c, client, requestPrepare, requestSwitch)
	setAuthorityData(t, client, func(data map[string][]byte) { data["ca-2.key"] = []byte("not a key\n") })
	request(t, client, requestRetire)
	_, err := controller(t, client).Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "generation 2 key ca-2.key") {
		t.Errorf("reconcile: %v", err)
	}
	s, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := s.Data["ca-1.key"]; !kept {
		t.Error("the outgoing key was destroyed")
	}
	if !strings.Contains(s.Annotations[LastError], `rotation request "retire" refused`) {
		t.Errorf("last error %q", s.Annotations[LastError])
	}
}

// The operator's way out of a refused record is the replace request, the
// sproozi.com/rotation-request=replace annotation on the authority Secret.
// It reaches the store, which leaves a single phase on a new generation:
// the Secret keeps no earlier CA key or leaf, the request is cleared, and a
// new trust ConfigMap names only the new generation. Guards the replace
// path in Reconcile and apply, and pki's ReplaceCA on a refused record.
func TestAReplaceRequestRecoversARefusedRecord(t *testing.T) {
	client := refusedCluster(t)
	before := authorityData(t, client)
	request(t, client, requestReplace)
	status, err := controller(t, client).Reconcile(context.Background())
	if err != nil || status.TrustConfigMap != trustPrefix+"-3" {
		t.Fatalf("replace: %+v %v", status, err)
	}
	s, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var st pki.State
	if err = json.Unmarshal(s.Data["state.json"], &st); err != nil {
		t.Fatal(err)
	}
	if st.Phase() != pki.PhaseSingle || st.Serving != 3 {
		t.Errorf("replace left %+v", st)
	}
	for name := range before {
		if strings.HasSuffix(name, ".key") {
			if _, kept := s.Data[name]; kept {
				t.Errorf("%s survived replace", name)
			}
		}
	}
	if s.Annotations[RotationRequest] != "" || s.Annotations[LastError] != "" {
		t.Errorf("annotations %v", s.Annotations)
	}
	leaf := serving(t, client).Leaf
	if err = pki.VerifyServing(leaf, trust(t, client, trustPrefix+"-3"), names, leaf.NotBefore.Add(time.Hour)); err != nil {
		t.Error(err)
	}
}
