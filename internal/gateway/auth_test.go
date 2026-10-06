/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway_test

import (
	"context"
	"errors"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	testManagedByLabel = "sproozi.com/managed-by"
)

const (
	testRunUIDLabel = "sproozi.com/agentrun-uid"
)

const (
	testRunNamespaceLabel = "sproozi.com/agentrun-namespace"
)

const (
	testRunLabel = "sproozi.com/agentrun"
)

const (
	testAgentsNamespace      = "sproozi-agents"
	testFirstServiceAccount  = "sproozi-uid001"
	testManagedByValue       = "sproozi-controller"
	testRunServiceAccount    = "sproozi-run"
	testSecondServiceAccount = "sproozi-uid002"
)

const (
	testNamespace    = "default"
	testTemplateName = "tmpl"
	testPolicyName   = "pol"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(sprooziv1alpha1.AddToScheme(s))
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

func TestParseServiceAccountUsername(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNS   string
		wantName string
		wantErr  bool
	}{
		{
			name:     "valid SA username",
			input:    "system:serviceaccount:sproozi-agents:sproozi-abc123",
			wantNS:   testAgentsNamespace,
			wantName: "sproozi-abc123",
		},
		{
			name:    "not a service account",
			input:   "system:node:mynode",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "only three parts",
			input:   "system:serviceaccount:sproozi-agents",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, name, err := gateway.ParseServiceAccountUsername(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns != tt.wantNS {
				t.Errorf("ns = %q, want %q", ns, tt.wantNS)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
		})
	}
}

func TestFindRunBySAReturnsRunningRun(t *testing.T) {
	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: testNamespace, UID: types.UID("run-uid-001")},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: testTemplateName},
			Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
			Task:         "investigate",
		},
		Status: sprooziv1alpha1.AgentRunStatus{
			Phase: sprooziv1alpha1.AgentRunPhaseRunning,
			Identity: sprooziv1alpha1.AgentRunIdentity{
				ServiceAccountName: testFirstServiceAccount,
			},
		},
	}
	tmpl := &sprooziv1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: testTemplateName, Namespace: testNamespace},
		Spec: sprooziv1alpha1.AgentTemplateSpec{
			PolicyRef:  sprooziv1alpha1.AgentPolicyReference{Name: testPolicyName},
			RuntimeRef: sprooziv1alpha1.AgentRuntimeReference{Name: "rt"},
		},
	}
	pol := &sprooziv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Namespace: testNamespace, Generation: 3},
		Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
			Budgets:             map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 50000}},
		},
	}

	// Do NOT use WithStatusSubresource — it prevents List from returning the status.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: testFirstServiceAccount, Namespace: testAgentsNamespace, UID: types.UID("service-account-uid-001"),
		Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: run.Name, testRunNamespaceLabel: run.Namespace, testRunUIDLabel: string(run.UID)},
	}}
	var listCalls int
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(run, tmpl, pol, sa).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, client client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listCalls++
			return errors.New("cluster-wide AgentRun list is forbidden")
		}}).
		Build()

	finder := gateway.NewK8sRunFinder(c, testAgentsNamespace)
	identity, err := finder.Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: testFirstServiceAccount, UID: "service-account-uid-001"})
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if identity.Run.Name != "my-run" {
		t.Errorf("Run.Name = %q, want %q", identity.Run.Name, "my-run")
	}
	if identity.Policy.Name != testPolicyName {
		t.Errorf("Policy.Name = %q, want %q", identity.Policy.Name, testPolicyName)
	}
	if listCalls != 0 {
		t.Fatalf("Find() performed %d cluster-wide list calls, want bounded direct reads", listCalls)
	}
}

func TestFindRunBySAReturnsErrRunNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	finder := gateway.NewK8sRunFinder(c, testAgentsNamespace)
	_, err := finder.Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: "sproozi-nobody", UID: "missing"})
	if err != gateway.ErrRunNotFound {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

func TestFindRunRejectsWrongNamespaceAndServiceAccountUID(t *testing.T) {
	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: testNamespace, UID: types.UID("run-uid")},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseRunning, Identity: sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: testRunServiceAccount}},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: testRunServiceAccount, Namespace: testAgentsNamespace, UID: types.UID("actual-service-account-uid"),
		Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: run.Name, testRunNamespaceLabel: run.Namespace, testRunUIDLabel: string(run.UID)},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(run, sa).Build()
	finder := gateway.NewK8sRunFinder(c, testAgentsNamespace)
	for _, test := range []struct {
		name     string
		identity gateway.ServiceAccountIdentity
		want     error
	}{
		{name: "same name elsewhere", identity: gateway.ServiceAccountIdentity{Namespace: "other", Name: sa.Name, UID: string(sa.UID)}, want: gateway.ErrWrongAgentNamespace},
		{name: "reused service account name", identity: gateway.ServiceAccountIdentity{Namespace: sa.Namespace, Name: sa.Name, UID: "old-service-account-uid"}, want: gateway.ErrServiceAccountMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := finder.Find(context.Background(), test.identity)
			if err != test.want {
				t.Fatalf("Find() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestFindRunUsesImmutableLabelsToResolveNameCollision(t *testing.T) {
	uid := types.UID("run-uid")
	makeRun := func(name string) *sprooziv1alpha1.AgentRun {
		return &sprooziv1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: uid},
			Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseRunning, Identity: sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: testRunServiceAccount}},
		}
	}
	first, second := makeRun("first"), makeRun("second")
	first.Spec.TemplateRef.Name, second.Spec.TemplateRef.Name = testTemplateName, testTemplateName
	tmpl := &sprooziv1alpha1.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: testTemplateName, Namespace: testNamespace}, Spec: sprooziv1alpha1.AgentTemplateSpec{PolicyRef: sprooziv1alpha1.AgentPolicyReference{Name: testPolicyName}}}
	policy := &sprooziv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Namespace: testNamespace}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: testRunServiceAccount, Namespace: testAgentsNamespace, UID: types.UID("service-account-uid"),
		Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: first.Name, testRunNamespaceLabel: first.Namespace, testRunUIDLabel: string(first.UID)},
	}}
	// The second run cannot pass the immutable label test, which is exactly the
	// intended collision defense; preserve an explicit assertion for that path.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(first, second, sa, tmpl, policy).Build()
	finder := gateway.NewK8sRunFinder(c, testAgentsNamespace)
	identity, err := finder.Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: sa.Namespace, Name: sa.Name, UID: string(sa.UID)})
	if err != nil || identity.Run.Name != first.Name {
		t.Fatalf("immutable labels must select exactly one run, identity=%#v err=%v", identity, err)
	}
}

func TestFindRunBySAReturnsErrRunNotRunning(t *testing.T) {
	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "done-run", Namespace: testNamespace, UID: types.UID("run-uid-002")},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: testTemplateName},
			Capabilities: []sprooziv1alpha1.CapabilityKind{},
			Task:         "done",
		},
		Status: sprooziv1alpha1.AgentRunStatus{
			Phase: sprooziv1alpha1.AgentRunPhaseSucceeded,
			Identity: sprooziv1alpha1.AgentRunIdentity{
				ServiceAccountName: testSecondServiceAccount,
			},
		},
	}
	// Do NOT use WithStatusSubresource — it prevents List from returning the status.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: testSecondServiceAccount, Namespace: testAgentsNamespace, UID: types.UID("service-account-uid-002"),
		Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: run.Name, testRunNamespaceLabel: run.Namespace, testRunUIDLabel: string(run.UID)},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(run, sa).Build()
	finder := gateway.NewK8sRunFinder(c, testAgentsNamespace)
	_, err := finder.Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: testSecondServiceAccount, UID: "service-account-uid-002"})
	if err != gateway.ErrRunNotRunning {
		t.Errorf("err = %v, want ErrRunNotRunning", err)
	}
}

func TestFindRunRejectsCancelledRunningRun(t *testing.T) {
	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cancelled-run", Namespace: testNamespace, UID: types.UID("cancelled-run-uid")},
		Spec:       sprooziv1alpha1.AgentRunSpec{Cancel: true},
		Status:     sprooziv1alpha1.AgentRunStatus{Phase: sprooziv1alpha1.AgentRunPhaseRunning, Identity: sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: "sproozi-cancelled"}},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: "sproozi-cancelled", Namespace: testAgentsNamespace, UID: types.UID("cancelled-service-account-uid"),
		Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: run.Name, testRunNamespaceLabel: run.Namespace, testRunUIDLabel: string(run.UID)},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(run, sa).Build()
	_, err := gateway.NewK8sRunFinder(c, testAgentsNamespace).Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: sa.Namespace, Name: sa.Name, UID: string(sa.UID)})
	if err != gateway.ErrRunNotRunning {
		t.Fatalf("Find() error = %v, want ErrRunNotRunning", err)
	}
}

func TestK8sTokenValidatorAuthenticates(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				User: authv1.UserInfo{
					Username: "system:serviceaccount:sproozi-agents:sproozi-abc123", UID: "service-account-uid",
				},
			},
		}
		return true, review, nil
	})

	v := gateway.NewK8sTokenValidator(clientset, "sproozi-model-gateway")
	identity, err := v.Validate(context.Background(), "valid-token")
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if identity.Namespace != testAgentsNamespace {
		t.Errorf("Namespace = %q, want %q", identity.Namespace, testAgentsNamespace)
	}
	if identity.Name != "sproozi-abc123" || identity.UID != "service-account-uid" {
		t.Errorf("identity = %#v, want complete ServiceAccount identity", identity)
	}
}

func TestK8sTokenValidatorReturnsErrTokenInvalidWhenNotAuthenticated(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: false,
			},
		}
		return true, review, nil
	})

	v := gateway.NewK8sTokenValidator(clientset, "sproozi-model-gateway")
	_, err := v.Validate(context.Background(), "invalid-token")
	if err != gateway.ErrTokenInvalid {
		t.Errorf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestK8sTokenValidatorReturnsErrNotServiceAccountForNonSAUser(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				User: authv1.UserInfo{
					Username: "system:node:mynode",
				},
			},
		}
		return true, review, nil
	})

	v := gateway.NewK8sTokenValidator(clientset, "sproozi-model-gateway")
	_, err := v.Validate(context.Background(), "node-token")
	if err != gateway.ErrNotServiceAccount {
		t.Errorf("err = %v, want ErrNotServiceAccount", err)
	}
}
