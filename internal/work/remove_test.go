//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// assertRemoved checks that nothing of session v is kept: no record, no
// journal of any of its executions.
func assertRemoved(t *testing.T, c Config, o *Owner, id string, executions ...string) {
	t.Helper()
	if _, err := os.Lstat(o.path(id, ".json")); !os.IsNotExist(err) {
		t.Fatalf("record of %s kept: %v", id, err)
	}
	for _, execution := range executions {
		if _, err := c.Coordinator.Inspect(execution); !os.IsNotExist(err) {
			t.Fatalf("journal of execution %s kept: %v", execution, err)
		}
	}
	if _, err := o.Info(t.Context(), id); code(err) != profile.CodeInvalid {
		t.Fatalf("removed session still answers: %v", err)
	}
}

// remove refuses a session that is starting, running, stopping, has
// unfinished work or is being pulled, and keeps it.
func TestRemoveRefusesSessionsThatAreNotSafelyRemovable(t *testing.T) {
	t.Parallel()
	refuse := func(t *testing.T, o *Owner, id string, phase Phase) {
		t.Helper()
		if v, err := o.Info(t.Context(), id); err != nil || v.Phase != phase {
			t.Fatalf("set-up phase %s: %+v %v", phase, v, err)
		}
		if _, err := o.Remove(t.Context(), id); code(err) != profile.CodeConflict {
			t.Fatalf("removed a session that is %s: %v", phase, err)
		}
		if v, err := o.Info(t.Context(), id); err != nil || v.Phase != phase {
			t.Fatalf("a refused removal changed the session: %+v %v", v, err)
		}
	}
	t.Run("active", func(t *testing.T) {
		t.Parallel()
		c, _ := fixture(t)
		o := openOwner(t, c)
		refuse(t, o, startInteractive(t, o, interactiveRequest()).ID, phaseActive)
	})
	t.Run("stopping", func(t *testing.T) {
		t.Parallel()
		c, p := fixture(t)
		release := make(chan struct{})
		p.heldStop, p.stopStarted = release, make(chan struct{})
		o := openOwner(t, c)
		defer close(release)
		v := submit(t, o)
		<-p.stopStarted
		refuse(t, o, v.ID, phaseStopping)
	})
	t.Run("preparing", func(t *testing.T) {
		t.Parallel()
		c, p := fixture(t)
		p.refusePrepare = &profile.Rejection{Code: profile.CodeDenied}
		o := openOwner(t, c)
		v := submit(t, o)
		waitDriver(t, o, v.ID)
		refuse(t, o, v.ID, phasePreparing)
	})
	for name, phase := range map[string]Phase{"fence": phaseUnknownStop, "accounting": phaseUnknownUsage} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, p := fixture(t)
			p.missingFence = name == "fence"
			p.missingSeal = name == "accounting"
			o := openOwner(t, c)
			v := submit(t, o)
			waitPhase(t, o, v.ID, phase)
			refuse(t, o, v.ID, phase)
		})
	}
	t.Run("exporting", func(t *testing.T) {
		t.Parallel()
		c, _ := fixture(t)
		o := openOwner(t, c)
		v := submit(t, o)
		waitPhase(t, o, v.ID, phaseRetained)
		if _, err := o.BeginExport(t.Context(), v.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		refuse(t, o, v.ID, PhaseExporting)
	})
}

// Removing a session frees what it kept, so a submission the executor
// refused for space then starts. A repeated removal reports it removed, and
// a repeated submission of the removed session's request never starts it
// again.
func TestRemoveFreesCapacityAndIsReplaySafe(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.RetainedBytes = 2 * c.MaxCheckpointBytes
	o := openOwner(t, c)
	first := taskRequest()
	v, err := o.Submit(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	refused := taskRequest()
	if _, err = o.Submit(t.Context(), refused); code(err) != profile.CodeRetentionFull {
		t.Fatalf("submission beyond retainedBytes: %v", err)
	}

	removed, err := o.Remove(t.Context(), v.ID)
	if err != nil || removed.ID != v.ID {
		t.Fatalf("remove a retained task: %+v %v", removed, err)
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID)
	if again, err := o.Remove(t.Context(), v.ID); err != nil || again.ID != v.ID {
		t.Fatalf("repeated removal: %+v %v", again, err)
	}
	if _, err = o.Remove(t.Context(), uuid.NewString()); code(err) != profile.CodeInvalid {
		t.Fatalf("removed a session that never existed: %v", err)
	}
	if _, err = o.Submit(t.Context(), first); code(err) != profile.CodeConflict {
		t.Fatalf("a repeated submission restarted a removed session: %v", err)
	}
	next, err := o.Submit(t.Context(), refused)
	if err != nil {
		t.Fatalf("submission after removal: %v", err)
	}
	waitPhase(t, o, next.ID, phaseRetained)

	// A removal interrupted after the record went finishes when repeated.
	if err = o.markRemoved(removedMark{ID: next.ID, Executions: []string{next.ExecutionID}}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(o.path(next.ID, ".json")); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), next.ID); err != nil {
		t.Fatalf("repeat an interrupted removal: %v", err)
	}
	assertRemoved(t, c, o, next.ID, next.ExecutionID)
}

// Every execution of an interactive session goes, and a cancelled session
// and a pulled one are removable.
func TestRemoveDeletesEveryExecutionAndAcceptsCancelledAndPulled(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)

	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	resumed, err := o.Resume(t.Context(), v.ID, uuid.NewString(), "")
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseActive)
	if _, err = o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID, resumed.ExecutionID)

	cancelled := startInteractive(t, o, interactiveRequest())
	if _, err = o.Cancel(t.Context(), cancelled.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, cancelled.ID, phaseCancelled)
	if _, err = o.Remove(t.Context(), cancelled.ID); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, o, cancelled.ID, cancelled.ExecutionID)

	pulled := submit(t, o)
	waitPhase(t, o, pulled.ID, phaseRetained)
	op := uuid.NewString()
	if _, err = o.BeginExport(t.Context(), pulled.ID, op); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Claim(t.Context(), pulled.ID, op, p.meta.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), pulled.ID); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, o, pulled.ID, pulled.ExecutionID)
	if _, err = o.Claim(t.Context(), pulled.ID, op, p.meta.SHA256); err == nil {
		t.Fatal("a removed session was claimed again")
	}
	if kept, err := os.ReadDir(c.Directory); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range kept {
			if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".workspace") {
				t.Fatalf("catalogue keeps %s after every session was removed", entry.Name())
			}
		}
	}
}

// A stopped version 1 record is removable with its kept checkpoint; one
// being pulled is not.
func TestRemoveVersionOneRecords(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	export, err := o.BeginExport(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	exporting := writeLegacy(t, c, o, legacyExporting, export)
	if _, err = o.Remove(t.Context(), exporting); code(err) != profile.CodeConflict {
		t.Fatalf("removed a version 1 record being pulled: %v", err)
	}
	for _, phase := range []legacyPhase{legacyRetained, legacyCancelled, legacyPulled} {
		id := writeLegacy(t, c, o, phase, export)
		if _, err = o.Remove(t.Context(), id); err != nil {
			t.Fatalf("remove a %s version 1 record: %v", phase, err)
		}
		assertRemoved(t, c, o, id)
		if _, err = os.Lstat(o.path(id, ".checkpoint")); !os.IsNotExist(err) {
			t.Fatalf("kept the checkpoint of a removed %s version 1 record: %v", phase, err)
		}
	}
}

// Only the home principal removes a session; the work token is denied, as
// for every owner action Hermes does not hold.
func TestWorkTokenCannotRemove(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	taskToken, home := strings.Repeat("t", 32), strings.Repeat("h", 32)
	h, err := NewHandler(o, taskToken, homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{taskToken, http.StatusForbidden}, {home, http.StatusOK}, {home, http.StatusOK}} {
		b, _ := json.Marshal(map[string]string{"id": v.ID})
		r := httptest.NewRequest(http.MethodPost, "/work/remove", strings.NewReader(string(b)))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("remove with token %q…: %d %s, want %d", tc.token[:1], w.Code, w.Body.String(), tc.want)
		}
		if tc.want == http.StatusForbidden {
			if _, err = o.Info(t.Context(), v.ID); err != nil {
				t.Fatalf("a denied removal removed the session: %v", err)
			}
		}
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID)
}
