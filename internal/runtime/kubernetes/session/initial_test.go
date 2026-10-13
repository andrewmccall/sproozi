package session

import (
	"errors"
	"strings"
	"testing"
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
