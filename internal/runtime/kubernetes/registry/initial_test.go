package registry

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

func TestInitialWorkspaceBindsObservedNativeIdentityOnce(t *testing.T) {
	s, _ := registry(t, nil)
	wanted := registration()
	native := wanted.NativeUUID
	wanted.NativeUUID, wanted.CheckpointDigest = "", ""
	wanted.WorkspaceDigest, wanted.Task = strings.Repeat("c", 64), "Fix the fixture"
	r, err := s.Create(t.Context(), wanted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindNative(t.Context(), r.Reference, native); !errors.Is(err, ErrConflict) {
		t.Fatal("inert native binding accepted", err)
	}
	r, err = s.BindRun(t.Context(), r.Reference, fixtureRunUID)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Prepared(t.Context(), r.Reference, fixtureObservedPodUID)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Activate(t.Context(), r.Reference)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.BindNative(t.Context(), r.Reference, native)
	if err != nil || r.NativeUUID != native {
		t.Fatal("genuine identity not bound", r, err)
	}
	if _, err = s.BindNative(t.Context(), r.Reference, native); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindNative(t.Context(), r.Reference, "22345678-1234-1234-1234-123456789abc"); !errors.Is(err, ErrConflict) {
		t.Fatal("native identity replaced", err)
	}
	replay, err := s.Create(t.Context(), wanted)
	if err != nil || replay.NativeUUID != native {
		t.Fatal("original empty identity replay failed", replay, err)
	}
	wanted.Task = "Different task"
	if _, err = s.Create(t.Context(), wanted); !errors.Is(err, ErrConflict) {
		t.Fatal("changed task replay accepted", err)
	}
}

// activeInitial is an active first visit from a Git workspace that has bound
// no native identity yet.
func activeInitial(t *testing.T, s *Store) Record {
	t.Helper()
	wanted := registration()
	wanted.NativeUUID, wanted.CheckpointDigest = "", ""
	wanted.WorkspaceDigest = strings.Repeat("c", 64)
	r, err := s.Create(t.Context(), wanted)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = s.BindRun(t.Context(), r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Prepared(t.Context(), r.Reference, fixtureObservedPodUID); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Activate(t.Context(), r.Reference); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTurnlessInitialVisitStopsWithoutCheckpoint(t *testing.T) {
	s, _ := registry(t, nil)
	r := activeInitial(t, s)
	if _, err := s.Stop(t.Context(), r.Reference); err != nil {
		t.Fatal(err)
	}
	seals := map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {Units: 3, CostMicros: 4}}
	stopped, err := s.CompleteStop(t.Context(), r.Reference, "", seals)
	if err != nil || stopped.Phase != Stopped || stopped.Receipt == nil || stopped.Receipt.CheckpointDigest != "" || stopped.NativeUUID != "" {
		t.Fatalf("turnless stop: %+v %v", stopped, err)
	}
	read, err := s.Get(t.Context(), r.Reference)
	if err != nil || !reflect.DeepEqual(read.Receipt, stopped.Receipt) {
		t.Fatalf("stored turnless stop: %+v %v", read, err)
	}
	if _, err = s.CompleteStop(t.Context(), r.Reference, "", seals); err != nil {
		t.Fatal("turnless replay", err)
	}
	if _, err = s.CompleteStop(t.Context(), r.Reference, strings.Repeat("d", 64), seals); err == nil {
		t.Fatal("turnless receipt amended with a checkpoint")
	}
	if _, err = s.BindNative(t.Context(), r.Reference, registration().NativeUUID); !errors.Is(err, ErrConflict) {
		t.Fatal("native identity bound after a turnless stop", err)
	}
}

func TestStopWithoutCheckpointNeedsUnboundInitialVisit(t *testing.T) {
	seals := map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {Units: 1}}
	t.Run("bound initial", func(t *testing.T) {
		s, _ := registry(t, nil)
		r := activeInitial(t, s)
		if _, err := s.BindNative(t.Context(), r.Reference, registration().NativeUUID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stop(t.Context(), r.Reference); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteStop(t.Context(), r.Reference, "", seals); err == nil {
			t.Fatal("a visit with a native conversation stopped without a checkpoint")
		}
	})
	t.Run("unbound initial with checkpoint", func(t *testing.T) {
		s, _ := registry(t, nil)
		r := activeInitial(t, s)
		if _, err := s.Stop(t.Context(), r.Reference); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteStop(t.Context(), r.Reference, strings.Repeat("d", 64), seals); err == nil {
			t.Fatal("a checkpoint was kept with no native identity")
		}
	})
	t.Run("checkpoint visit", func(t *testing.T) {
		s, _ := registry(t, nil)
		ref := prepared(t, s)
		if _, err := s.Activate(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stop(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteStop(t.Context(), ref, "", seals); err == nil {
			t.Fatal("a resumed visit stopped without a checkpoint")
		}
	})
}
