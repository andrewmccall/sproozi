package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/harness"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var testNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

const fixtureTemplate = "codex-mobile"
const fixtureRunName = "visit-1"

const fixtureRunUID = "run-uid"
const fixtureObservedPodUID = "observed-pod"
const fixtureNamespace = "registry"

func TestIgnoredSelectionIsImmutableRegistration(t *testing.T) {
	s, _ := registry(t, nil)
	wanted := registration()
	wanted.IncludeIgnored = []string{"needed.bin"}
	r, err := s.Create(t.Context(), wanted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.IncludeIgnored, ",") != "needed.bin" {
		t.Fatal("registration dropped the approved ignored file")
	}
	if _, err = s.Create(t.Context(), wanted); err != nil {
		t.Fatal(err)
	}
	wanted.IncludeIgnored = []string{"other.bin"}
	if _, err = s.Create(t.Context(), wanted); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed selection retry did not conflict: %v", err)
	}
	wanted.IntendedRunName = "invalid-selection"
	wanted.IncludeIgnored = []string{".git/config"}
	if _, err = s.Create(t.Context(), wanted); err == nil {
		t.Fatal("registration accepted Git authority selection")
	}
	wanted = registration()
	wanted.IntendedRunName = "empty-selection"
	wanted.IncludeIgnored = []string{}
	if _, err = s.Create(t.Context(), wanted); err != nil {
		t.Fatal(err)
	}
	wanted.IncludeIgnored = nil
	if _, err = s.Create(t.Context(), wanted); err != nil {
		t.Fatalf("empty and nil selection differ: %v", err)
	}
}

func registration() Registration {
	return Registration{Harness: harness.NativeCodex, Principal: "alice", SessionID: "spr-1", Epoch: 2, OperationID: "move-1",
		NativeUUID: "12345678-1234-1234-1234-123456789abc", InstructionDigest: strings.Repeat("a", 64), OriginalDeadline: testNow.Add(time.Hour),
		TemplateName: fixtureTemplate, Ceilings: map[api.CapabilityKind]api.EndpointBudget{
			api.CapabilityModelInference: {MaxUnits: 100, MaxCostMicros: 200}},
		CheckpointDigest: strings.Repeat("b", 64), IntendedRunName: fixtureRunName}
}

func registry(t *testing.T, update func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error) (*Store, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	uid := 0
	kube := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			uid++
			if obj.GetUID() == "" {
				obj.SetUID(types.UID(fmt.Sprintf("record-%d", uid)))
			}
			return c.Create(ctx, obj, opts...)
		}, Update: update,
	}).Build()
	s, err := NewStore(kube, fixtureNamespace)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return testNow }
	return s, kube
}

func prepared(t *testing.T, s *Store) Reference {
	t.Helper()
	ctx := context.Background()
	r, err := s.Create(ctx, registration())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindRun(ctx, r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prepared(ctx, r.Reference, "pod-uid"); err != nil {
		t.Fatal(err)
	}
	return r.Reference
}

func TestRegistryLifecycleAndReceiptReplay(t *testing.T) {
	s, _ := registry(t, nil)
	ctx := context.Background()
	r, err := s.Create(ctx, registration())
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "creating" || r.Reference.UID != "record-1" || r.Reference.Epoch != 2 {
		t.Fatalf("unexpected creation: %+v", r)
	}
	ref := r.Reference
	for _, bind := range []func() (Record, error){
		func() (Record, error) { return s.BindRun(ctx, ref, fixtureRunUID) },
		func() (Record, error) { return s.BindRun(ctx, ref, fixtureRunUID) },
	} {
		r, err = bind()
		if err != nil || r.Phase != "bound" || r.RunUID != fixtureRunUID {
			t.Fatalf("bind: %+v %v", r, err)
		}
	}
	r, err = s.Prepared(ctx, ref, "pod-uid")
	if err != nil || r.Phase != "prepared" || r.PodUID != "pod-uid" {
		t.Fatalf("prepare: %+v %v", r, err)
	}
	for range 2 {
		r, err = s.Activate(ctx, ref)
		if err != nil || r.Phase != "active" {
			t.Fatalf("activate: %+v %v", r, err)
		}
	}
	for range 2 {
		r, err = s.Stop(ctx, ref)
		if err != nil || r.Phase != "stopping" {
			t.Fatalf("stop: %+v %v", r, err)
		}
	}
}

func TestObservedPodBindingDoesNotMarkRestorePreparedOrAllowActivation(t *testing.T) {
	s, _ := registry(t, nil)
	r, err := s.Create(t.Context(), registration())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindPod(t.Context(), r.Reference, fixtureObservedPodUID); !errors.Is(err, ErrConflict) {
		t.Fatalf("Pod bound before Run: %v", err)
	}
	if _, err = s.BindRun(t.Context(), r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r, err = s.BindPod(t.Context(), r.Reference, fixtureObservedPodUID)
		if err != nil || r.Phase != Bound || r.PodUID != fixtureObservedPodUID || r.Receipt != nil {
			t.Fatalf("observational Pod binding: %+v %v", r, err)
		}
	}
	if _, err = s.Activate(t.Context(), r.Reference); !errors.Is(err, ErrConflict) {
		t.Fatalf("observed Pod admitted without successful restore: %v", err)
	}
	if _, err = s.BindPod(t.Context(), r.Reference, "replacement-pod"); !errors.Is(err, ErrConflict) {
		t.Fatalf("observed Pod rebound: %v", err)
	}
	r, err = s.Prepared(t.Context(), r.Reference, fixtureObservedPodUID)
	if err != nil || r.Phase != Prepared {
		t.Fatalf("successful restore failed to promote observed Pod: %+v %v", r, err)
	}
}

func TestCancellationReceiptRequiresFencedBindingAndRetainsImmutableSeals(t *testing.T) {
	s, _ := registry(t, nil)
	r, err := s.Create(t.Context(), registration())
	if err != nil {
		t.Fatal(err)
	}
	seals := map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {Units: 7, CostMicros: 12}}
	if _, err = s.CompleteCancel(t.Context(), r.Reference, seals); !errors.Is(err, ErrConflict) {
		t.Fatalf("unbound cancellation receipt accepted: %v", err)
	}
	if _, err = s.BindRun(t.Context(), r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindPod(t.Context(), r.Reference, fixtureObservedPodUID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteCancel(t.Context(), r.Reference, seals); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrevoked cancellation receipt accepted: %v", err)
	}
	if _, err = s.Cancel(t.Context(), r.Reference); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteCancel(t.Context(), r.Reference, nil); err == nil {
		t.Fatal("incomplete cancelled accounting accepted")
	}
	for range 2 {
		r, err = s.CompleteCancel(t.Context(), r.Reference, seals)
		if err != nil || r.Phase != Cancelled || r.Receipt == nil || r.Receipt.CheckpointDigest != "" ||
			r.Receipt.Seals[api.CapabilityModelInference] != (budget.TrustedUsage{Units: 7, CostMicros: 12}) {
			t.Fatalf("cancellation receipt retention: %+v %v", r, err)
		}
	}
	seals[api.CapabilityModelInference] = budget.TrustedUsage{Units: 1, CostMicros: 2}
	if _, err = s.CompleteCancel(t.Context(), r.Reference, seals); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed cancelled usage amended receipt: %v", err)
	}
	if _, err = s.Activate(t.Context(), r.Reference); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled receipt reactivated execution: %v", err)
	}
}

func TestRegistryReceiptReplayAndRestart(t *testing.T) {
	s, kube := registry(t, nil)
	ctx := context.Background()
	ref := prepared(t, s)
	if _, err := s.Activate(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	var r Record
	var err error
	seals := map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {Units: 40, CostMicros: 60}}
	for range 2 {
		r, err = s.CompleteStop(ctx, ref, strings.Repeat("c", 64), seals)
		if err != nil || r.Phase != "stopped" || r.Receipt.CheckpointDigest != strings.Repeat("c", 64) || r.Receipt.Seals[api.CapabilityModelInference].Units != 40 {
			t.Fatalf("receipt: %+v %v", r, err)
		}
	}
	// A restart reads retained authority/receipt; Run deletion cannot collect it.
	restarted, err := NewStore(kube, fixtureNamespace)
	if err != nil {
		t.Fatal(err)
	}
	r, err = restarted.Get(ctx, ref)
	if err != nil || r.RunUID != fixtureRunUID || r.PodUID != "pod-uid" || r.Receipt.Seals[api.CapabilityModelInference].CostMicros != 60 {
		t.Fatalf("restart: %+v %v", r, err)
	}
	var cm corev1.ConfigMap
	if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: ref.Name}, &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Fatal("registry is owned by disposable Run")
	}
	seals[api.CapabilityModelInference] = budget.TrustedUsage{Units: 1, CostMicros: 1}
	if _, err = s.CompleteStop(ctx, ref, strings.Repeat("c", 64), seals); !errors.Is(err, ErrConflict) {
		t.Fatalf("amended receipt accepted: %v", err)
	}
	if _, err = s.Activate(ctx, ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped execution reopened: %v", err)
	}
}

func TestImmutableReplayAndRebinding(t *testing.T) {
	s, _ := registry(t, nil)
	ctx := context.Background()
	request := registration()
	// Strip a monotonic clock and normalize the public time at creation.
	request.OriginalDeadline = request.OriginalDeadline.In(time.FixedZone("other", 3600))
	r, err := s.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.Create(ctx, request)
	if err != nil || replayed.Reference != r.Reference {
		t.Fatalf("same request did not replay: %+v %v", replayed, err)
	}
	request.OperationID = "different"
	if _, err = s.Create(ctx, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed request accepted: %v", err)
	}
	if _, err = s.BindRun(ctx, r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindRun(ctx, r.Reference, "replacement"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Run rebinding accepted: %v", err)
	}
	if _, err = s.Prepared(ctx, r.Reference, "pod-uid"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prepared(ctx, r.Reference, "replacement"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Pod rebinding accepted: %v", err)
	}
}

func TestStaleReferencesAndReplacement(t *testing.T) {
	s, kube := registry(t, nil)
	ctx := context.Background()
	ref := prepared(t, s)
	stale := ref
	stale.Epoch++
	if _, err := s.Activate(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale epoch accepted: %v", err)
	}
	var cm corev1.ConfigMap
	if err := kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: ref.Name}, &cm); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	cm.ResourceVersion = ""
	cm.UID = "replacement-record"
	if err := kube.Create(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, ref); !errors.Is(err, ErrGone) {
		t.Fatalf("replacement inherited reference: %v", err)
	}
	current := ref
	current.UID = "replacement-record"
	r, err := s.Get(ctx, current)
	if err != nil || r.Phase != "prepared" {
		t.Fatalf("replacement fixture unavailable: %+v %v", r, err)
	}
}

func TestExpiryAndCancellation(t *testing.T) {
	s, _ := registry(t, nil)
	ctx := context.Background()
	ref := prepared(t, s)
	s.now = func() time.Time { return testNow.Add(time.Hour) }
	if _, err := s.Activate(ctx, ref); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired activation accepted: %v", err)
	}
	r, err := s.Cancel(ctx, ref)
	if err != nil || r.Phase != Cancelled {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	s.now = func() time.Time { return testNow }
	if _, err = s.Activate(ctx, ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled execution activated: %v", err)
	}
	r, err = s.Cancel(ctx, ref)
	if err != nil || r.Phase != Cancelled {
		t.Fatalf("cancel replay: %+v %v", r, err)
	}
	// An expired first registration cannot create another execution.
	fresh := registration()
	fresh.IntendedRunName = "visit-expired"
	fresh.OriginalDeadline = testNow
	if _, err = s.Create(ctx, fresh); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired create accepted: %v", err)
	}
}

func TestCancelRacingActivationWinsCAS(t *testing.T) {
	var s *Store
	var ref Reference
	intervened := false
	update := func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		cm := obj.(*corev1.ConfigMap)
		r, err := read(cm)
		if err != nil {
			return err
		}
		if r.Phase == Active && !intervened {
			intervened = true
			if _, err = s.Cancel(ctx, ref); err != nil {
				return err
			}
		}
		return c.Update(ctx, obj, opts...)
	}
	s, _ = registry(t, update)
	ref = prepared(t, s)
	if _, err := s.Activate(context.Background(), ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("activation raced past cancellation: %v", err)
	}
	r, err := s.Get(context.Background(), ref)
	if err != nil || r.Phase != Cancelled {
		t.Fatalf("final fence: %+v %v", r, err)
	}
}

func TestActivationRacingCancelCannotResurrect(t *testing.T) {
	var s *Store
	var ref Reference
	intervened := false
	update := func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		r, err := read(obj.(*corev1.ConfigMap))
		if err != nil {
			return err
		}
		if r.Phase == Cancelled && !intervened {
			intervened = true
			if _, err = s.Activate(ctx, ref); err != nil {
				return err
			}
		}
		return c.Update(ctx, obj, opts...)
	}
	s, _ = registry(t, update)
	ref = prepared(t, s)
	r, err := s.Cancel(context.Background(), ref)
	if err != nil || r.Phase != Cancelled {
		t.Fatalf("cancel did not retry over activation: %+v %v", r, err)
	}
	if _, err = s.Activate(context.Background(), ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled execution resurrected: %v", err)
	}
}

func TestInvalidRegistrationAndSeals(t *testing.T) {
	for name, mutate := range map[string]func(*Registration){
		"unbounded units": func(r *Registration) { r.Ceilings[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 0} },
		"negative cost": func(r *Registration) {
			r.Ceilings[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 1, MaxCostMicros: -1}
		},
		"invalid native id": func(r *Registration) { r.NativeUUID = "not-a-uuid" },
		"missing digest":    func(r *Registration) { r.CheckpointDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := registry(t, nil)
			r := registration()
			mutate(&r)
			if _, err := s.Create(context.Background(), r); err == nil {
				t.Fatal("invalid registration accepted")
			}
			valid, err := s.Create(context.Background(), registration())
			if err != nil || valid.Phase != "creating" {
				t.Fatalf("valid registration: %+v %v", valid, err)
			}
		})
	}
	s, _ := registry(t, nil)
	ctx := context.Background()
	ref := prepared(t, s)
	if _, err := s.Activate(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	for _, seals := range []map[api.CapabilityKind]budget.TrustedUsage{
		nil,
		{api.CapabilityModelInference: {Units: 101}},
		{api.CapabilityModelInference: {Units: 1, CostMicros: 201}},
		{api.CapabilityModelInference: {Units: -1}},
	} {
		if _, err := s.CompleteStop(ctx, ref, strings.Repeat("c", 64), seals); err == nil {
			t.Fatal("invalid seal accepted")
		}
	}
	r, err := s.CompleteStop(ctx, ref, strings.Repeat("c", 64), map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {Units: 10, CostMicros: 20}})
	if err != nil || r.Phase != "stopped" || r.Receipt.Seals[api.CapabilityModelInference].Units != 10 {
		t.Fatalf("valid seal: %+v %v", r, err)
	}
}

func TestCorruptRegistryFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.ConfigMap){
		"unknown JSON": func(cm *corev1.ConfigMap) {
			cm.Data[dataKey] = strings.TrimSuffix(cm.Data[dataKey], "}") + `,"unexpected":true}`
		},
		"trailing JSON": func(cm *corev1.ConfigMap) { cm.Data[dataKey] += " {}" },
		"oversized":     func(cm *corev1.ConfigMap) { cm.Data[dataKey] = strings.Repeat(" ", maxRecordBytes+1) },
		"missing binding": func(cm *corev1.ConfigMap) {
			cm.Data[dataKey] = strings.Replace(cm.Data[dataKey], `"runUid":"`+fixtureRunUID+`"`, `"runUid":""`, 1)
		},
		"Run owner": func(cm *corev1.ConfigMap) {
			cm.OwnerReferences = []metav1.OwnerReference{{UID: fixtureRunUID, Name: fixtureRunName}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, kube := registry(t, nil)
			ctx := context.Background()
			ref := prepared(t, s)
			var cm corev1.ConfigMap
			if err := kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: ref.Name}, &cm); err != nil {
				t.Fatal(err)
			}
			mutate(&cm)
			if err := kube.Update(ctx, &cm); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Activate(ctx, ref); err == nil {
				t.Fatal("corrupt registry activated")
			}
		})
	}
}

func TestRegistryNativeKindIsRequiredAndImmutable(t *testing.T) {
	for _, kind := range []harness.NativeKind{"", "unknown"} {
		t.Run(string(kind), func(t *testing.T) {
			s, _ := registry(t, nil)
			intent := registration()
			intent.Harness = kind
			if _, err := s.Create(t.Context(), intent); err == nil {
				t.Fatal("invalid kind accepted")
			}
		})
	}
	s, _ := registry(t, nil)
	intent := registration()
	intent.Harness = harness.NativeClaude
	record, err := s.Create(t.Context(), intent)
	if err != nil || record.Harness != harness.NativeClaude {
		t.Fatalf("Claude registration: %+v %v", record, err)
	}
	intent.Harness = harness.NativeCodex
	if _, err = s.Create(t.Context(), intent); !errors.Is(err, ErrConflict) {
		t.Fatalf("native kind mutation: %v", err)
	}
}

func TestRetiredVersionFieldDoesNotBlockLifecycleRecovery(t *testing.T) {
	store, kube := registry(t, nil)
	record, err := store.Create(t.Context(), registration())
	if err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err = kube.Get(t.Context(), types.NamespacedName{Namespace: fixtureNamespace, Name: record.Reference.Name}, &cm); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cm.Data[dataKey], "nativePin") {
		t.Fatal("new registration pins a client version")
	}
	cm.Data[dataKey] = strings.Replace(cm.Data[dataKey], `"registration":{`, `"registration":{"nativePin":"historical-client-version",`, 1)
	if err = kube.Update(t.Context(), &cm); err != nil {
		t.Fatal(err)
	}
	observed, err := store.Get(t.Context(), record.Reference)
	if err != nil || observed.Harness != harness.NativeCodex || observed.Phase != Creating {
		t.Fatalf("historical record cannot recover: %+v %v", observed, err)
	}
}
