package placement

import (
	"errors"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tombstone records l's preparation as aborted, as AbortPreparation does
// before it changes anything in the cluster, keeping the record's reference.
func tombstone(t *testing.T, p *Placement, l session.Launch) {
	t.Helper()
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	local.PreparationAborted = true
	if err = p.save(local, nil); err != nil {
		t.Fatal(err)
	}
}

// Inspect of a launch whose own preparation was aborted reports preparing
// whatever became of its registration and run, settled or not: only
// AbortPreparation proves that nothing runs, which a coordinator still
// pending on the launch reaches through Prepare's refusal (ADR 0067). A
// registration that may have had authority is the one exception: it stays a
// conflict, as before.
func TestInspectOfAnAbortedPreparationIsPreparingUnlessItMayHaveHadAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		build    func(*testing.T) (*Placement, session.Launch)
		conflict bool
	}{
		{name: "no reference", build: func(t *testing.T) (*Placement, session.Launch) {
			p, _, _, l, data := setup(t)
			if _, err := p.persistIntent(l, data); err != nil {
				t.Fatal(err)
			}
			tombstone(t, p, l)
			return p, l
		}},
		{name: "creating", build: func(t *testing.T) (*Placement, session.Launch) {
			p, _, _, l, data := setup(t)
			local, err := p.persistIntent(l, data)
			if err != nil {
				t.Fatal(err)
			}
			ceilings, err := p.liveProfile(t.Context(), l)
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.registry.Create(t.Context(), p.registration(l, ceilings))
			if err != nil || r.Phase != registry.Creating {
				t.Fatalf("registration: %+v %v", r, err)
			}
			if _, err = p.adopt(local, r); err != nil {
				t.Fatal(err)
			}
			tombstone(t, p, l)
			return p, l
		}},
		{name: "bound", build: func(t *testing.T) (*Placement, session.Launch) {
			p, _, l, _ := queuedPreparation(t)
			tombstone(t, p, l)
			return p, l
		}},
		{name: "prepared", build: func(t *testing.T) (*Placement, session.Launch) {
			p, _, _, l, data := setup(t)
			if _, err := p.Prepare(t.Context(), l, data); err != nil {
				t.Fatal(err)
			}
			tombstone(t, p, l)
			return p, l
		}},
		{name: "settled abort", build: func(t *testing.T) (*Placement, session.Launch) {
			p, kube, l, _ := queuedPreparation(t)
			kube.reconcile = cancelReconcile(t, kube)
			if err := p.AbortPreparation(t.Context(), l); err != nil {
				t.Fatal(err)
			}
			return p, l
		}},
		{name: "registration unreadable", build: func(t *testing.T) (*Placement, session.Launch) {
			p, kube, l, _ := queuedPreparation(t)
			tombstone(t, p, l)
			fail := func(obj client.Object) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return apierrors.NewServiceUnavailable("the API server cannot answer")
				}
				return nil
			}
			kube.getErr.Store(&fail)
			return p, l
		}},
		{name: "registration deleted", build: func(t *testing.T) (*Placement, session.Launch) {
			p, kube, l, _ := queuedPreparation(t)
			tombstone(t, p, l)
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: p.config.Namespace, Name: registry.NameForRun(runName(l))}}
			if err := kube.Client.Delete(t.Context(), cm); err != nil {
				t.Fatal(err)
			}
			return p, l
		}},
		{name: "run replaced", build: func(t *testing.T) (*Placement, session.Launch) {
			p, kube, l, _ := queuedPreparation(t)
			old := agentRun(t, kube, l)
			if err := kube.Client.Delete(t.Context(), old); err != nil {
				t.Fatal(err)
			}
			replacement := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: old.Namespace, Name: old.Name, UID: replacementUID},
				Spec: old.Spec}
			if err := kube.Client.Create(t.Context(), replacement); err != nil {
				t.Fatal(err)
			}
			// The abort cancels the registration and stops at the
			// replacement: its end stays unproven.
			var unproven session.UnprovenEnd
			if err := p.AbortPreparation(t.Context(), l); !errors.As(err, &unproven) {
				t.Fatalf("abort of a replaced run: %v", err)
			}
			if r := registration(t, p, l); r.Phase != registry.Cancelled {
				t.Fatalf("registration of the replaced run: %+v", r)
			}
			return p, l
		}},
		{name: "active visit", conflict: true, build: func(t *testing.T) (*Placement, session.Launch) {
			p, _, _, l, data := setup(t)
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			activate(t, p, l, i)
			tombstone(t, p, l)
			return p, l
		}},
		{name: "cancelled with usage", conflict: true, build: func(t *testing.T) (*Placement, session.Launch) {
			p, _, _, l, data := setup(t)
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			activate(t, p, l, i)
			spend(t, p, l)
			if _, err = p.Abort(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			tombstone(t, p, l)
			return p, l
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, l := test.build(t)
			o, err := p.Inspect(t.Context(), l)
			var rejection *profile.Rejection
			switch {
			case test.conflict && (!errors.As(err, &rejection) || rejection.Code != profile.CodeConflict):
				t.Fatalf("Inspect of an aborted preparation that may have had authority: %+v %v, want conflict", o.State, err)
			case !test.conflict && (err != nil || o.State != session.PlacementPreparing || o.Incarnation != (session.Incarnation{})):
				t.Fatalf("Inspect of an aborted preparation: %+v %v, want preparing", o, err)
			}
		})
	}
}
