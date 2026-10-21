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

package kubernetes_test

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
	gateway "github.com/andrewmccall/sproozi/internal/gateway/kubernetes"
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

// The TokenReview extras that bind a token to its Pod.
const (
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"
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

	finder := gateway.NewAuthenticator(nil, "", c, testAgentsNamespace)
	identity, err := finder.Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: testFirstServiceAccount, UID: "service-account-uid-001"})
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if identity.Execution.Name != "my-run" {
		t.Errorf("Run.Name = %q, want %q", identity.Execution.Name, "my-run")
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
	finder := gateway.NewAuthenticator(nil, "", c, testAgentsNamespace)
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
	finder := gateway.NewAuthenticator(nil, "", c, testAgentsNamespace)
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
	finder := gateway.NewAuthenticator(nil, "", c, testAgentsNamespace)
	identity, err := finder.Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: sa.Namespace, Name: sa.Name, UID: string(sa.UID)})
	if err != nil || identity.Execution.Name != first.Name {
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
	finder := gateway.NewAuthenticator(nil, "", c, testAgentsNamespace)
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
	_, err := gateway.NewAuthenticator(nil, "", c, testAgentsNamespace).Find(context.Background(), gateway.ServiceAccountIdentity{Namespace: sa.Namespace, Name: sa.Name, UID: string(sa.UID)})
	if err != gateway.ErrRunNotRunning {
		t.Fatalf("Find() error = %v, want ErrRunNotRunning", err)
	}
}

func TestAuthenticateResolvesTokenReviewSubjectToItsRun(t *testing.T) {
	f := managedExecution(t, true)
	for _, tc := range []struct {
		name, uid string
		want      error
	}{{"complete subject", f.subject.UID, nil}, {"missing UID", "", gateway.ErrTokenInvalid}} {
		t.Run(tc.name, func(t *testing.T) {
			clientset := k8sfake.NewSimpleClientset()
			clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{
					Username: "system:serviceaccount:" + f.subject.Namespace + ":" + f.subject.Name, UID: tc.uid,
					Extra: map[string]authv1.ExtraValue{
						podNameExtra: {f.pod.Name},
						podUIDExtra:  {string(f.pod.UID)},
					},
				}}}, nil
			})
			grant, err := gateway.NewAuthenticator(clientset, "sproozi-gateway", f.kube, testAgentsNamespace).Authenticate(context.Background(), "valid-token")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (grant.Authority.Name != f.subject.Name || grant.Authority.UID != f.subject.UID || grant.Execution.UID != fixtureExecutionUID) {
				t.Errorf("grant = %#v, want the complete ServiceAccount subject and its run", grant)
			}
		})
	}
}

func TestAuthenticateReturnsErrTokenInvalidWhenNotAuthenticated(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: false,
			},
		}
		return true, review, nil
	})

	v := gateway.NewAuthenticator(clientset, "sproozi-model-gateway", fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), testAgentsNamespace)
	_, err := v.Authenticate(context.Background(), "invalid-token")
	if err != gateway.ErrTokenInvalid {
		t.Errorf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestAuthenticateReturnsErrNotServiceAccountForNonSAUser(t *testing.T) {
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

	v := gateway.NewAuthenticator(clientset, "sproozi-model-gateway", fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), testAgentsNamespace)
	_, err := v.Authenticate(context.Background(), "node-token")
	if err != gateway.ErrNotServiceAccount {
		t.Errorf("err = %v, want ErrNotServiceAccount", err)
	}
}

// A run token authorizes the run only when it is bound to the run's own
// sandbox Pod, matched by name and UID. A token with no Pod binding, such as
// one from "kubectl create token", is refused.
func TestAuthenticateRequiresTheRunsSandboxPod(t *testing.T) {
	f := managedExecution(t, true)
	sandbox, sandboxUID := f.run.Status.Identity.SandboxName, string(f.pod.UID)
	for _, tc := range []struct {
		name       string
		pods, uids []string
		want       error
	}{
		{"sandbox Pod", []string{sandbox}, []string{sandboxUID}, nil},
		{"unbound token", nil, nil, gateway.ErrTokenNotPodBound},
		{"Pod name without UID", []string{sandbox}, nil, gateway.ErrTokenNotPodBound},
		{"Pod UID without name", nil, []string{sandboxUID}, gateway.ErrTokenNotPodBound},
		{"another Pod", []string{"debug-pod"}, []string{"debug-uid"}, gateway.ErrSandboxPodMismatch},
		{"replaced sandbox Pod", []string{sandbox}, []string{"old-pod-uid"}, gateway.ErrSandboxPodMismatch},
		{"two Pods", []string{sandbox, "debug-pod"}, []string{sandboxUID, "debug-uid"}, gateway.ErrTokenInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientset := k8sfake.NewSimpleClientset()
			clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				user := authv1.UserInfo{Username: "system:serviceaccount:" + f.subject.Namespace + ":" + f.subject.Name, UID: f.subject.UID,
					Extra: map[string]authv1.ExtraValue{}}
				if tc.pods != nil {
					user.Extra[podNameExtra] = tc.pods
				}
				if tc.uids != nil {
					user.Extra[podUIDExtra] = tc.uids
				}
				return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: user}}, nil
			})
			_, err := gateway.NewAuthenticator(clientset, "sproozi-gateway", f.kube, testAgentsNamespace).Authenticate(context.Background(), "token")
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A sandbox Pod that cannot be read fails closed and keeps the cause.
func TestAuthenticateFailsClosedWhenTheSandboxPodCannotBeRead(t *testing.T) {
	f := managedExecution(t, true)
	forbidden := errors.New("pods is forbidden")
	kube := interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			return forbidden
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{
			Username: "system:serviceaccount:" + f.subject.Namespace + ":" + f.subject.Name, UID: f.subject.UID,
			Extra: map[string]authv1.ExtraValue{
				podNameExtra: {f.pod.Name},
				podUIDExtra:  {string(f.pod.UID)},
			}}}}, nil
	})
	_, err := gateway.NewAuthenticator(clientset, "sproozi-gateway", kube, testAgentsNamespace).Authenticate(context.Background(), "token")
	if !errors.Is(err, forbidden) {
		t.Fatalf("Authenticate() error = %v, want the Pod read failure", err)
	}
}
