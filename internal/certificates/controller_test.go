package certificates

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	systemNamespace = "sproozi-system"
	trustPrefix     = "sproozi-sandbox-trust"
)

var names = pki.Names{DNS: []string{"sproozi-gateway.sproozi-system.svc", "kubernetes.default.svc"}}

func setup(t *testing.T) (*Controller, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sproozi-certificate-authority", Namespace: systemNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sproozi-gateway-tls", Namespace: systemNamespace}},
	)
	return controller(t, client), client
}

func controller(t *testing.T, client *fake.Clientset) *Controller {
	return &Controller{Client: client, Config: Config{Namespace: systemNamespace, AuthoritySecret: "sproozi-certificate-authority",
		ServingSecret: "sproozi-gateway-tls", TrustNamespace: "sproozi-agents", TrustPrefix: trustPrefix,
		Names: names, Policy: pki.DefaultPolicy, Scratch: testutil.PrivateDir(t)}}
}

func serving(t *testing.T, client *fake.Clientset) tls.Certificate {
	t.Helper()
	s, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-gateway-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}
	cert.Leaf, _ = pki.ParseCertificate(s.Data[corev1.TLSCertKey])
	return cert
}

func trust(t *testing.T, client *fake.Clientset, name string) []byte {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("sproozi-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Immutable == nil || !*cm.Immutable {
		t.Fatalf("trust ConfigMap %s is mutable", name)
	}
	if strings.Contains(cm.Data[trustBundleKey], "PRIVATE KEY") {
		t.Fatal("trust ConfigMap contains private key material")
	}
	return []byte(cm.Data[trustBundleKey])
}

func request(t *testing.T, client *fake.Clientset, value string) {
	t.Helper()
	secrets := client.CoreV1().Secrets(systemNamespace)
	s, err := secrets.Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[RotationRequest] = value
	if _, err = secrets.Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapPublishesServingSecretAndImmutableTrust(t *testing.T) {
	c, client := setup(t)
	status, err := c.Reconcile(context.Background())
	if err != nil || !status.Changed || status.TrustConfigMap != trustPrefix {
		t.Fatalf("bootstrap %+v %v", status, err)
	}
	leaf := serving(t, client).Leaf
	if err = pki.VerifyServing(leaf, trust(t, client, trustPrefix), names, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A restarted controller with fresh scratch space reuses committed state.
	again, err := controller(t, client).Reconcile(context.Background())
	if err != nil || again.Changed || !serving(t, client).Leaf.Equal(leaf) {
		t.Fatalf("restart replaced valid material: %+v %v", again, err)
	}
}

func TestOperatorDrivenRotationCreatesNewTrustObjects(t *testing.T) {
	c, client := setup(t)
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	trustA := trust(t, client, trustPrefix)
	leafA := serving(t, client).Leaf
	request(t, client, "prepare")
	status, err := c.Reconcile(context.Background())
	if err != nil || status.TrustConfigMap != "sproozi-sandbox-trust-1-2" || !serving(t, client).Leaf.Equal(leafA) {
		t.Fatalf("prepare %+v %v", status, err)
	}
	trustAB := trust(t, client, "sproozi-sandbox-trust-1-2")
	if string(trust(t, client, trustPrefix)) != string(trustA) {
		t.Fatal("existing trust object changed")
	}
	request(t, client, "switch")
	if _, err = c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	leafB := serving(t, client).Leaf
	if leafB.Equal(leafA) || pki.VerifyServing(leafB, trustAB, names, time.Now()) != nil || pki.VerifyServing(leafB, trustA, names, time.Now()) == nil {
		t.Fatal("switch must serve a new-CA leaf trusted only by the combined bundle")
	}
	request(t, client, "retire")
	if status, err = c.Reconcile(context.Background()); err != nil || status.TrustConfigMap != "sproozi-sandbox-trust-2" {
		t.Fatalf("retire %+v %v", status, err)
	}
	if pki.VerifyServing(leafB, trust(t, client, "sproozi-sandbox-trust-2"), names, time.Now()) != nil {
		t.Fatal("retired bundle must still verify the B leaf")
	}
	s, _ := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if _, ok := s.Data["ca-1.key"]; ok || s.Annotations[RotationRequest] != "" || s.Annotations[RotationStage] != "none" {
		t.Fatalf("retired key or request retained: %v", s.Annotations)
	}
}

func TestRefusedRequestIsReportedWithoutChangingTrust(t *testing.T) {
	c, client := setup(t)
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	request(t, client, "switch")
	if _, err := c.Reconcile(context.Background()); err == nil {
		t.Fatal("switch without prepare accepted")
	}
	s, _ := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if !strings.Contains(s.Annotations[LastError], "refused") || s.Annotations[RotationRequest] != "" || s.Annotations[TrustAnnotation] != trustPrefix {
		t.Fatalf("annotations %v", s.Annotations)
	}
}

func TestMissingPrecreatedObjectsFailWithoutCreatingThem(t *testing.T) {
	client := fake.NewClientset()
	if _, err := controller(t, client).Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "install creates it") {
		t.Fatalf("missing authority Secret: %v", err)
	}
	if list, _ := client.CoreV1().Secrets(systemNamespace).List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatal("controller created a Secret")
	}
}

func TestRenewRequestReplacesTheLeafUnderTheSameAuthority(t *testing.T) {
	c, client := setup(t)
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := serving(t, client).Leaf
	request(t, client, "renew")
	status, err := c.Reconcile(context.Background())
	if err != nil || status.TrustConfigMap != trustPrefix {
		t.Fatalf("renew %+v %v", status, err)
	}
	after := serving(t, client).Leaf
	if after.Equal(before) || pki.VerifyServing(after, trust(t, client, trustPrefix), names, time.Now()) != nil {
		t.Fatal("renewal must serve a new leaf trusted by the unchanged bundle")
	}
}

func TestLostAuthorityStateIsNotReplacedByANewCA(t *testing.T) {
	c, client := setup(t)
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	leaf := serving(t, client).Leaf
	secrets := client.CoreV1().Secrets(systemNamespace)
	s, err := secrets.Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.Data = nil
	if _, err = secrets.Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "lost its certificate state") {
		t.Fatalf("emptied authority Secret bootstrapped again: %v", err)
	}
	if !serving(t, client).Leaf.Equal(leaf) {
		t.Fatal("serving Secret changed after the authority state was lost")
	}
}
