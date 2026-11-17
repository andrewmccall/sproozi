package home

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/session"
)

const outstandingLog = "Failed to finish gateway certificate store cleanup"

// pastStore opens the managed store with a clock 25 days back, so a leaf it
// issues is due for renewal now.
func pastStore(t *testing.T, c Config) *pki.Store {
	t.Helper()
	store, err := pki.Open(c.Gateway.PKIDirectory, pki.DefaultPolicy, func() time.Time {
		return time.Now().Add(-25 * 24 * time.Hour)
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func managedNames(c Config) pki.Names {
	return servingNames(c, []*localStack{{config: c.Local}})
}

// servedIsCurrent fails unless the gateway serves the store's committed leaf
// and that leaf is not due for renewal.
func servedIsCurrent(t *testing.T, certs *certificates, store *pki.Store) {
	t.Helper()
	served := certs.serving.Load().Leaf
	if !served.Equal(mustServing(t, store)) {
		t.Error("the gateway does not serve the committed leaf")
	}
	if !time.Now().Before(pki.DefaultPolicy.RenewAt(served)) {
		t.Errorf("the served leaf is due for renewal: renew at %s", pki.DefaultPolicy.RenewAt(served))
	}
}

// Cleanup the store reports as outstanding does not stop the service: with a
// stray, non-removable leaf-<hex>.key directory in the store, the service
// opens and serves a renewed leaf, and maintain renews a due leaf again and
// serves it from memory. Each pass logs the report. Guards the
// ErrOutstanding handling in ensure.
func TestAStrayFileDoesNotStopTheServiceOrItsRenewal(t *testing.T) {
	c, store := managedConfig(t)
	past := pastStore(t, c)
	if err := past.Initialize(managedNames(c)); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(c.Gateway.PKIDirectory, "leaf-0000000000000000.key")
	if err := os.Mkdir(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	certs, err := openCertificates(c, managedNames(c), &log)
	if err != nil {
		t.Fatalf("the service did not open: %v", err)
	}
	servedIsCurrent(t, certs, store)
	if !strings.Contains(log.String(), outstandingLog) || !strings.Contains(log.String(), "pki: destroy retired keys:") {
		t.Errorf("open did not log the report: %q", log.String())
	}
	if err = past.Renew(); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err = certs.maintain(); err != nil {
		t.Fatalf("maintain: %v", err)
	}
	servedIsCurrent(t, certs, store)
	if !strings.Contains(log.String(), outstandingLog) {
		t.Errorf("maintain did not log the report: %q", log.String())
	}
}

// An unreadable key the service does not use does not stop it either. In
// prepared, ca-2.key at mode 0644: the service opens, renews and serves, and
// while a consumer lacks the new trust maintain renews again without
// switching. In switched, ca-1.key at mode 0644: with its evidence present
// the rotation retires generation 1 and its key is destroyed, which ends the
// report. Guards the ErrOutstanding handling in ensure.
func TestAnUnusedUnreadableKeyDoesNotStopTheServiceOrItsRotation(t *testing.T) {
	t.Run("prepared", func(t *testing.T) {
		c, store := managedConfig(t)
		past := pastStore(t, c)
		if err := past.Initialize(managedNames(c)); err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		certs, err := openCertificates(c, managedNames(c), &log)
		if err != nil {
			t.Fatal(err)
		}
		launch := session.Launch{AllocationID: "6f1f9c55-1d36-4b10-9a5e-0d5b1a9c7c11", Placement: testLocal}
		if _, err = certs.trustFor(launch); err != nil {
			t.Fatal(err)
		}
		if err = store.PrepareRotation(); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(filepath.Join(c.Gateway.PKIDirectory, "ca-2.key"), 0o644); err != nil {
			t.Fatal(err)
		}
		log.Reset()
		if certs, err = openCertificates(c, managedNames(c), &log); err != nil {
			t.Fatalf("the service did not open: %v", err)
		}
		if !strings.Contains(log.String(), "ca-2.key is unreadable") {
			t.Errorf("open did not log the key: %q", log.String())
		}
		certs.fenced = func(session.Launch) (bool, error) { return false, nil }
		if err = past.Renew(); err != nil {
			t.Fatal(err)
		}
		if err = certs.maintain(); err != nil {
			t.Fatalf("maintain: %v", err)
		}
		servedIsCurrent(t, certs, store)
		if st, _ := store.State(); st.Phase() != pki.PhasePrepared {
			t.Errorf("phase %s, want prepared while a consumer lacks the new trust", st.Phase())
		}
	})
	t.Run("switched", func(t *testing.T) {
		c, store := managedConfig(t)
		if err := store.Initialize(managedNames(c)); err != nil {
			t.Fatal(err)
		}
		if err := store.PrepareRotation(); err != nil {
			t.Fatal(err)
		}
		if err := store.SwitchRotation(true); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(c.Gateway.PKIDirectory, "ca-1.key"), 0o644); err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		certs, err := openCertificates(c, managedNames(c), &log)
		if err != nil {
			t.Fatalf("the service did not open: %v", err)
		}
		if !strings.Contains(log.String(), "ca-1.key is unreadable") {
			t.Errorf("open did not log the key: %q", log.String())
		}
		if err = certs.maintain(); err != nil {
			t.Fatalf("maintain: %v", err)
		}
		st, err := store.State()
		if err != nil || st.Phase() != pki.PhaseSingle || st.Serving != 2 {
			t.Fatalf("rotation did not retire generation 1: %+v %v", st, err)
		}
		if _, err = os.Stat(filepath.Join(c.Gateway.PKIDirectory, "ca-1.key")); !os.IsNotExist(err) {
			t.Errorf("retired key: %v", err)
		}
		servedIsCurrent(t, certs, store)
	})
}

// With the new serving key broken, maintain does not retire the previous
// authority even when its evidence is present: the key report is logged,
// the retire step fails naming the key, and the outgoing key, the last that
// works, is kept. Guards the serving-key check in pki's RetireRotation.
func TestMaintainDoesNotRetireTheLastWorkingKey(t *testing.T) {
	c, store := managedConfig(t)
	if err := store.Initialize(managedNames(c)); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareRotation(); err != nil {
		t.Fatal(err)
	}
	if err := store.SwitchRotation(true); err != nil {
		t.Fatal(err)
	}
	certs, err := openCertificates(c, managedNames(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.Gateway.PKIDirectory, "ca-2.key"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = certs.maintain(); err == nil || !strings.Contains(err.Error(), "generation 2 key ca-2.key") {
		t.Errorf("maintain: %v", err)
	}
	if _, err = os.Stat(filepath.Join(c.Gateway.PKIDirectory, "ca-1.key")); err != nil {
		t.Errorf("the outgoing key: %v", err)
	}
	if st, _ := store.State(); st.Phase() != pki.PhaseSwitched {
		t.Errorf("phase %s, want switched", st.Phase())
	}
}

// A refused record still stops the service from opening, and fails
// maintain. Unchanged by the outstanding handling: State refuses it first.
func TestARefusedRecordStillStopsTheService(t *testing.T) {
	c, store := managedConfig(t)
	if err := store.Initialize(managedNames(c)); err != nil {
		t.Fatal(err)
	}
	certs, err := openCertificates(c, managedNames(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.Gateway.PKIDirectory, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st pki.State
	if err = json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	st.Serving = 9
	if data, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = openCertificates(c, managedNames(c), nil); err == nil || !strings.Contains(err.Error(), "pki: corrupt state:") {
		t.Errorf("open: %v", err)
	}
	if err = certs.maintain(); err == nil || !strings.Contains(err.Error(), "pki: corrupt state:") {
		t.Errorf("maintain: %v", err)
	}
}

// A renewal the store cannot make is an error, not outstanding cleanup: with
// the serving key lost and the leaf due, the service does not open, and
// maintain fails. Guards the errors.Is test in ensure.
func TestAFailedRenewalStillFails(t *testing.T) {
	c, store := managedConfig(t)
	past := pastStore(t, c)
	if err := past.Initialize(managedNames(c)); err != nil {
		t.Fatal(err)
	}
	certs, err := openCertificates(c, managedNames(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = past.Renew(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(c.Gateway.PKIDirectory, "ca-1.key")); err != nil {
		t.Fatal(err)
	}
	const want = "pki: serving CA key unavailable"
	if _, err = openCertificates(c, managedNames(c), nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("open: %v", err)
	}
	if err = certs.maintain(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("maintain: %v", err)
	}
	_ = store
}
