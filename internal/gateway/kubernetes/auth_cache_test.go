package kubernetes_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	internalgateway "github.com/andrewmccall/sproozi/internal/gateway"
	gateway "github.com/andrewmccall/sproozi/internal/gateway/kubernetes"
)

const cacheTestServiceAccountUID = "service-account-uid-cache"

func cacheFixtureObjects() []client.Object {
	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cached-run", Namespace: testNamespace, UID: types.UID("run-uid-cache")},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: testTemplateName},
			Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
			Task:         "investigate",
		},
		Status: sprooziv1alpha1.AgentRunStatus{
			Phase:      sprooziv1alpha1.AgentRunPhaseRunning,
			Identity:   sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: testFirstServiceAccount},
			PolicyName: testPolicyName,
		},
	}
	return []client.Object{run,
		&sprooziv1alpha1.AgentTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: testTemplateName, Namespace: testNamespace},
			Spec: sprooziv1alpha1.AgentTemplateSpec{PolicyRef: sprooziv1alpha1.AgentPolicyReference{Name: testPolicyName},
				RuntimeRef: sprooziv1alpha1.AgentRuntimeReference{Name: "rt"}},
		},
		&sprooziv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Namespace: testNamespace, Generation: 1},
			Spec: sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference}},
		},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: testFirstServiceAccount, Namespace: testAgentsNamespace, UID: types.UID(cacheTestServiceAccountUID),
			Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: run.Name,
				testRunNamespaceLabel: run.Namespace, testRunUIDLabel: string(run.UID)},
		}},
	}
}

// countingClient builds a fake API reader that counts its Gets.
func countingClient(t *testing.T, gets *atomic.Int32, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
}

func tokenReviewer() *k8sfake.Clientset {
	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{
			Username: "system:serviceaccount:" + testAgentsNamespace + ":" + testFirstServiceAccount,
			UID:      cacheTestServiceAccountUID}}}, nil
	})
	return clientset
}

type cacheFixture struct {
	authenticator *gateway.Authenticator
	live, cached  client.Client
	liveGets      *atomic.Int32
	now           *time.Time
}

func newCacheFixture(t *testing.T) cacheFixture {
	t.Helper()
	f := cacheFixture{liveGets: &atomic.Int32{}, now: new(time.Time)}
	*f.now = time.Unix(1_000_000, 0)
	f.live = countingClient(t, f.liveGets, cacheFixtureObjects()...)
	f.cached = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cacheFixtureObjects()...).Build()
	f.authenticator = gateway.NewAuthenticator(tokenReviewer(), "sproozi-gateway", f.live, testAgentsNamespace).
		WithWatchCache(f.cached)
	f.authenticator.SetClock(func() time.Time { return *f.now })
	return f
}

func cancelRun(t *testing.T, c client.Client) {
	t.Helper()
	var run sprooziv1alpha1.AgentRun
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "cached-run"}, &run); err != nil {
		t.Fatal(err)
	}
	run.Spec.Cancel = true
	if err := c.Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticationReadsLiveAndRevalidationReadsTheWatchCache(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	afterConnect := f.liveGets.Load()
	if afterConnect == 0 {
		t.Fatal("authentication did not read live state")
	}
	for range 50 {
		*f.now = f.now.Add(100 * time.Millisecond) // five seconds of ticks
		if err := f.authenticator.Revalidate(context.Background(), grant); err != nil {
			t.Fatal(err)
		}
	}
	if f.liveGets.Load() != afterConnect {
		t.Fatalf("revalidation made %d live reads, want none within the live recheck interval", f.liveGets.Load()-afterConnect)
	}
	// A cached revocation closes the session once a live read confirms it.
	cancelRun(t, f.cached)
	cancelRun(t, f.live)
	if err := f.authenticator.Revalidate(context.Background(), grant); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("cached cancellation = %v, want revoked", err)
	}
}

func TestLaggingWatchCacheDoesNotRevokeALiveSession(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	// The run has not reached the cache yet, as just after it starts.
	f.cached = fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	f.authenticator.WithWatchCache(f.cached)
	if err := f.authenticator.Revalidate(context.Background(), grant); err != nil {
		t.Fatalf("lagging cache revoked a live session: %v", err)
	}
}

func TestStalledWatchCacheStillRevokesWithinTheLiveRecheckInterval(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	other := grant.Clone() // a second session of the same execution
	cancelRun(t, f.live)   // the cache never sees the cancellation
	*f.now = f.now.Add(gateway.LiveRecheckInterval - time.Millisecond)
	if err := f.authenticator.Revalidate(context.Background(), grant); err != nil {
		t.Fatalf("revalidation before the live recheck = %v", err)
	}
	*f.now = f.now.Add(time.Millisecond)
	if err := f.authenticator.Revalidate(context.Background(), grant); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("live recheck = %v, want revoked", err)
	}
	// The failed live check sends every other session of the execution live.
	if err := f.authenticator.Revalidate(context.Background(), other); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("second session after a failed live check = %v, want revoked", err)
	}
}

func TestPendingLiveCheckSendsOtherSessionsLive(t *testing.T) {
	f := newCacheFixture(t)
	// The first live Get after hold is armed blocks until release closes: that
	// is session A's claimed live read, stalled at the API server.
	var hold atomic.Bool
	held, release := make(chan struct{}), make(chan struct{})
	f.live = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cacheFixtureObjects()...).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption) error {
			if hold.CompareAndSwap(true, false) {
				close(held)
				<-release
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	f.authenticator = gateway.NewAuthenticator(tokenReviewer(), "sproozi-gateway", f.live, testAgentsNamespace).
		WithWatchCache(f.cached)
	f.authenticator.SetClock(func() time.Time { return *f.now })
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	other := grant.Clone() // session B of the same execution
	cancelRun(t, f.live)   // the stalled cache never sees the cancellation
	*f.now = f.now.Add(gateway.LiveRecheckInterval)
	hold.Store(true)
	sessionA := make(chan error, 1)
	go func() { sessionA <- f.authenticator.Revalidate(context.Background(), grant) }()
	<-held // A has claimed the live check and its read is in flight
	if err := f.authenticator.Revalidate(context.Background(), other); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("session B while A's live read is pending = %v, want revoked by its own live read", err)
	}
	close(release)
	if err := <-sessionA; !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("session A live read = %v, want revoked", err)
	}
}

func TestLiveCheckDueDuringACachedReadReadsLive(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	cancelRun(t, f.live) // the stalled cache never sees the cancellation
	// The cached read starts just before the recheck is due and finishes after.
	*f.now = f.now.Add(gateway.LiveRecheckInterval - time.Millisecond)
	f.cached = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cacheFixtureObjects()...).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption) error {
			*f.now = f.now.Add(time.Millisecond)
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	f.authenticator.WithWatchCache(f.cached)
	if err := f.authenticator.Revalidate(context.Background(), grant); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("cached pass that outlived the recheck = %v, want revoked by a live read", err)
	}
}

func editPolicy(t *testing.T, edit func(*sprooziv1alpha1.AgentPolicySpec), clients ...client.Client) {
	t.Helper()
	for _, c := range clients {
		var policy sprooziv1alpha1.AgentPolicy
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testPolicyName}, &policy); err != nil {
			t.Fatal(err)
		}
		edit(&policy.Spec)
		policy.Generation++ // the API server bumps it on every spec change
		if err := c.Update(context.Background(), &policy); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdmissionCapEditKeepsOpenSessions(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	editPolicy(t, func(spec *sprooziv1alpha1.AgentPolicySpec) { spec.MaxActiveRuns = ptr(int32(3)) }, f.live, f.cached)
	if err := f.authenticator.Revalidate(context.Background(), grant); err != nil {
		t.Fatalf("cached revalidation after a maxActiveRuns edit = %v, want the session kept", err)
	}
	*f.now = f.now.Add(gateway.LiveRecheckInterval)
	if err := f.authenticator.Revalidate(context.Background(), grant); err != nil {
		t.Fatalf("live revalidation after a maxActiveRuns edit = %v, want the session kept", err)
	}
}

func TestPermissionPolicyEditsStillRevoke(t *testing.T) {
	for name, edit := range map[string]func(*sprooziv1alpha1.AgentPolicySpec){
		"capability": func(spec *sprooziv1alpha1.AgentPolicySpec) {
			spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead}
		},
		"budget": func(spec *sprooziv1alpha1.AgentPolicySpec) {
			spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
				sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 10}}
		},
		"budget with a cap edit": func(spec *sprooziv1alpha1.AgentPolicySpec) {
			spec.MaxActiveRuns = ptr(int32(3))
			spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
				sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 10}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCacheFixture(t)
			grant, err := f.authenticator.Authenticate(context.Background(), "token")
			if err != nil {
				t.Fatal(err)
			}
			editPolicy(t, edit, f.live, f.cached)
			if err := f.authenticator.Revalidate(context.Background(), grant); !errors.Is(err, internalgateway.ErrGrantRevoked) {
				t.Fatalf("revalidation after a %s edit = %v, want revoked", name, err)
			}
		})
	}
}

// repointTemplate adds an AgentPolicy identical to the run's and points the
// template at it, as an administrator re-pointing a running run's template.
func repointTemplate(t *testing.T, clients ...client.Client) {
	t.Helper()
	for _, c := range clients {
		var policy sprooziv1alpha1.AgentPolicy
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testPolicyName}, &policy); err != nil {
			t.Fatal(err)
		}
		other := &sprooziv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "pol-b", Namespace: testNamespace},
			Spec: *policy.Spec.DeepCopy()}
		if err := c.Create(context.Background(), other); err != nil {
			t.Fatal(err)
		}
		var tmpl sprooziv1alpha1.AgentTemplate
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testTemplateName}, &tmpl); err != nil {
			t.Fatal(err)
		}
		tmpl.Spec.PolicyRef.Name = other.Name
		if err := c.Update(context.Background(), &tmpl); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepointedTemplateDeniesNewAndOpenSessions(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	repointTemplate(t, f.live, f.cached)
	identity := gateway.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: testFirstServiceAccount, UID: cacheTestServiceAccountUID}
	if _, err := f.authenticator.Find(context.Background(), identity); !errors.Is(err, gateway.ErrPolicyChanged) {
		t.Fatalf("live Find after re-pointing = %v, want ErrPolicyChanged", err)
	}
	if _, err := f.authenticator.Authenticate(context.Background(), "token"); !errors.Is(err, gateway.ErrPolicyChanged) {
		t.Fatalf("new connection after re-pointing = %v, want ErrPolicyChanged", err)
	}
	if err := f.authenticator.Revalidate(context.Background(), grant); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("open session after re-pointing = %v, want revoked", err)
	}
}

func TestRepointedTemplateInWatchCacheRevokes(t *testing.T) {
	f := newCacheFixture(t)
	grant, err := f.authenticator.Authenticate(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	identity := gateway.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: testFirstServiceAccount, UID: cacheTestServiceAccountUID}
	repointTemplate(t, f.cached)
	if _, err := f.authenticator.FindCached(context.Background(), identity); !errors.Is(err, gateway.ErrPolicyChanged) {
		t.Fatalf("cached find after re-pointing = %v, want ErrPolicyChanged", err)
	}
	repointTemplate(t, f.live)
	if err := f.authenticator.Revalidate(context.Background(), grant); !errors.Is(err, internalgateway.ErrGrantRevoked) {
		t.Fatalf("cached revalidation after re-pointing = %v, want revoked", err)
	}
}

func ptr[T any](value T) *T { return &value }
