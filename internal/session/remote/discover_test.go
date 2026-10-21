package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	testExecutor  = "engineering"
	roleHint      = "Role sproozi-cli"
	adminContext  = "admin"
	testNamespace = "team-a"
	testService   = "sproozi-work"
	testToken     = "sa-token-0123456789abcdef0123456789abcdef"
)

// executorServer is a TLS server for SERVICE.NAMESPACE.svc that answers
// describe the way the remote executor does.
type executorServer struct {
	address string
	caPEM   []byte
	status  atomic.Int32
	token   atomic.Value
}

func startExecutor(t *testing.T) *executorServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "executor"},
		DNSNames: []string{testService + "." + testNamespace + ".svc"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	s := &executorServer{address: listener.Addr().String(), caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	s.status.Store(http.StatusOK)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.token.Store(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		status := int(s.status.Load())
		w.WriteHeader(status)
		if status != http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "error": "denied"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "executor": map[string]any{
			"placement": testExecutor, "harness": "codex", "maxCheckpointBytes": 8 << 20}})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return s
}

func publishedObjects(caPEM []byte) []runtime.Object {
	ready, port, name := true, int32(8443), PortName
	return []runtime.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testService, Namespace: testNamespace,
			Labels: map[string]string{ExecutorLabel: "true"}},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: PortName, Port: 8443}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: TrustConfigMap, Namespace: testNamespace},
			Data: map[string]string{CAKey: string(caPEM)}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: testService + "-abc", Namespace: testNamespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: testService}},
			Ports: []discoveryv1.EndpointPort{{Name: &name, Port: &port}},
			Endpoints: []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "sproozi-work-0"}}}},
	}
}

// testCluster wires a fake API server, a port-forward allowance and a tunnel
// that dials the TLS server directly.
func testCluster(t *testing.T, s *executorServer, forward bool, objects ...runtime.Object) (*cluster, *fake.Clientset, *[]string) {
	t.Helper()
	kube := fake.NewClientset(objects...)
	kube.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		attributes := review.Spec.ResourceAttributes
		review.Status.Allowed = forward && attributes.Resource == "pods" && attributes.Subresource == "portforward" &&
			attributes.Verb == "create" && attributes.Namespace == testNamespace
		return true, review, nil
	})
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create := a.(k8stesting.CreateAction)
		if create.GetSubresource() != "token" {
			return false, nil, nil
		}
		request := create.GetObject().(*authenticationv1.TokenRequest).DeepCopy()
		request.Status = authenticationv1.TokenRequestStatus{Token: testToken,
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour))}
		return true, request, nil
	})
	var dialed []string
	sel := Selection{Kubeconfig: []string{"/fixture/kubeconfig"}, Context: DefaultContext, Namespace: testNamespace}
	return &cluster{sel: sel, kube: kube, tunnel: func(ctx context.Context, pod string, port int32) (net.Conn, error) {
		dialed = append(dialed, pod)
		if port != 8443 {
			t.Errorf("tunnel to port %d", port)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", s.address)
	}}, kube, &dialed
}

// The executor receives only a short-lived token minted for sproozi-cli and
// bound to the executor's audience, never the context's own credential.
func TestDiscoverReachesTheExecutorWithAMintedAudienceToken(t *testing.T) {
	s := startExecutor(t)
	c, kube, dialed := testCluster(t, s, true, publishedObjects(s.caPEM)...)
	e, err := c.discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != testExecutor || e.Harness != harness.NativeCodex || e.MaxCheckpointBytes != 8<<20 || e.Context != "sproozi" {
		t.Fatalf("executor %+v", e)
	}
	if got := s.token.Load(); got != testToken {
		t.Fatalf("executor saw token %v", got)
	}
	var minted []*authenticationv1.TokenRequest
	for _, a := range kube.Actions() {
		if create, ok := a.(k8stesting.CreateAction); ok && create.GetSubresource() == "token" {
			if a.GetNamespace() != testNamespace || create.GetResource().Resource != "serviceaccounts" {
				t.Fatalf("token minted in %s/%s", a.GetNamespace(), create.GetResource().Resource)
			}
			if name := a.(k8stesting.CreateActionImpl).Name; name != CLIServiceAccount {
				t.Fatalf("token minted for %q", name)
			}
			minted = append(minted, create.GetObject().(*authenticationv1.TokenRequest))
		}
	}
	if len(minted) != 1 || !slices.Equal(minted[0].Spec.Audiences, []string{TokenAudience}) ||
		minted[0].Spec.ExpirationSeconds == nil || *minted[0].Spec.ExpirationSeconds > 3600 {
		t.Fatalf("minted %+v, want one token for audience %q lasting at most an hour", minted, TokenAudience)
	}
	if time.Until(c.expires) < 50*time.Minute {
		t.Fatalf("token expiry not recorded: %v", c.expires)
	}
	if len(*dialed) != 1 || (*dialed)[0] != "sproozi-work-0" {
		t.Fatalf("dialed %v", *dialed)
	}
	if e.CheckpointProblem(8<<20) != nil || e.CheckpointProblem(1<<20) == nil ||
		!strings.Contains(e.CheckpointProblem(1<<20).Error(), "local.maxCheckpointBytes is 1048576") {
		t.Fatal("checkpoint limits not compared")
	}
}

func forbidden(resource string) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
	}
}

func TestDiscoverNamesTheFixForEachFailure(t *testing.T) {
	s := startExecutor(t)
	objects := publishedObjects(s.caPEM)
	unlabelled := objects[0].DeepCopyObject().(*corev1.Service)
	unlabelled.Labels = nil
	second := objects[0].DeepCopyObject().(*corev1.Service)
	second.Name = "another"
	noPort := objects[0].DeepCopyObject().(*corev1.Service)
	noPort.Spec.Ports[0].Name = "web"
	notReady := objects[2].DeepCopyObject().(*discoveryv1.EndpointSlice)
	no := false
	notReady.Endpoints[0].Conditions.Ready = &no
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		forward bool
		react   func(*fake.Clientset)
		status  int
		says    []string
	}{
		{name: "no executor in namespace", objects: []runtime.Object{unlabelled, objects[1], objects[2]}, forward: true,
			says: []string{`no remote executor in namespace "team-a"`, ExecutorLabel, "--namespace"}},
		{name: "two executors", objects: []runtime.Object{objects[0], second, objects[1], objects[2]}, forward: true,
			says: []string{"has 2 Services", "another", "sproozi-work"}},
		{name: "no https port", objects: []runtime.Object{noPort, objects[1], objects[2]}, forward: true,
			says: []string{`no port named "https"`}},
		{name: "service list denied", objects: objects, forward: true,
			react: func(k *fake.Clientset) { k.PrependReactor("list", "services", forbidden("services")) },
			says:  []string{"may not list Services", "ServiceAccount sproozi-cli", roleHint}},
		{name: "port-forward denied", objects: objects,
			says: []string{"may not port-forward", "sproozi-cli"}},
		{name: "CA missing", objects: []runtime.Object{objects[0], objects[2]}, forward: true,
			says: []string{"publishes no CA", TrustConfigMap}},
		{name: "CA read denied", objects: objects, forward: true,
			react: func(k *fake.Clientset) { k.PrependReactor("get", "configmaps", forbidden("configmaps")) },
			says:  []string{"may not read the ConfigMap sproozi-remote-executor", roleHint}},
		{name: "no ready pod", objects: []runtime.Object{objects[0], objects[1], notReady}, forward: true,
			says: []string{"has no ready Pod", "get pods"}},
		{name: "token not authenticated", objects: objects, forward: true, status: http.StatusUnauthorized,
			says: []string{"could not authenticate the sproozi-cli token", TokenAudience, "upgrade"}},
		{name: "token mint denied", objects: objects, forward: true,
			react: func(k *fake.Clientset) { k.PrependReactor("create", "serviceaccounts", forbidden("serviceaccounts")) },
			says:  []string{"may not create a token for the ServiceAccount sproozi-cli", roleHint}},
		{name: "no CLI ServiceAccount", objects: objects, forward: true,
			react: func(k *fake.Clientset) {
				k.PrependReactor("create", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, CLIServiceAccount)
				})
			},
			says: []string{`namespace "team-a" has no ServiceAccount sproozi-cli`, "config/cli-access"}},
		{name: "identity not allowed", objects: objects, forward: true, status: http.StatusForbidden,
			says: []string{"refused your Kubernetes user", `"use" on remoteexecutors.sproozi.com`, roleHint}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.status != 0 {
				s.status.Store(int32(tc.status))
				defer s.status.Store(http.StatusOK)
			}
			c, kube, _ := testCluster(t, s, tc.forward, tc.objects...)
			if tc.react != nil {
				tc.react(kube)
			}
			_, err := c.discover(t.Context())
			if err == nil {
				t.Fatal("discovery succeeded")
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
		})
	}
}

func TestDiscoverRejectsAnUntrustedCertificate(t *testing.T) {
	s := startExecutor(t)
	other := startExecutor(t)
	c, _, _ := testCluster(t, s, true, publishedObjects(other.caPEM)...)
	_, err := c.discover(t.Context())
	if err == nil || !strings.Contains(err.Error(), "certificate is not trusted") {
		t.Fatalf("got %v", err)
	}
}
