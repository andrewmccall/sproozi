package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const openAIHost = "api.openai.com"

var testNames = Names{DNS: []string{openAIHost, "api.anthropic.com"}, IPs: []string{"192.168.65.254"}}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openTest(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "pki")
	s, err := Open(dir, DefaultPolicy, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func serving(t *testing.T, s *Store) (*x509.Certificate, []byte, State) {
	t.Helper()
	cert, trust, st, err := s.Serving()
	if err != nil {
		t.Fatal(err)
	}
	return cert.Leaf, trust, st
}

func TestInitializeCreatesOnceAndEnsureReusesValidMaterial(t *testing.T) {
	s, c := openTest(t)
	if _, err := s.Ensure(testNames); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("ensure created an authority: %v", err)
	}
	err := s.Initialize(testNames)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Initialize(testNames); err == nil {
		t.Fatal("second initialization accepted")
	}
	leaf, trust, st := serving(t, s)
	if err = VerifyServing(leaf, trust, testNames, c.t); err != nil {
		t.Fatal(err)
	}
	if leaf.NotAfter.Sub(c.t) > DefaultPolicy.LeafValidity {
		t.Fatalf("leaf outlives policy: %s", leaf.NotAfter)
	}
	c.t = c.t.Add(time.Hour)
	changed, err := s.Ensure(testNames)
	if err != nil || changed {
		t.Fatalf("repeat ensure changed=%v err=%v", changed, err)
	}
	again, _, st2 := serving(t, s)
	if !again.Equal(leaf) || st2.Generations[0].Fingerprint != st.Generations[0].Fingerprint {
		t.Fatal("repeat ensure replaced valid material")
	}
}

func TestEnsureRenewsAfterTwoThirdsAndOnNameChange(t *testing.T) {
	s, c := openTest(t)
	if err := s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	first, _, _ := serving(t, s)
	c.t = DefaultPolicy.RenewAt(first).Add(time.Second)
	changed, err := s.Ensure(testNames)
	if err != nil || !changed {
		t.Fatalf("renewal changed=%v err=%v", changed, err)
	}
	second, _, st := serving(t, s)
	if second.Equal(first) || len(st.Generations) != 1 {
		t.Fatal("renewal must issue a new leaf under the same CA")
	}
	wider := Names{DNS: append([]string{"kubernetes.default.svc"}, testNames.DNS...), IPs: testNames.IPs}
	if changed, err = s.Ensure(wider); err != nil || !changed {
		t.Fatalf("name change changed=%v err=%v", changed, err)
	}
	third, trust, _ := serving(t, s)
	if err = VerifyServing(third, trust, wider, c.t); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Dir(), "leaf-"+Fingerprint(first)[:16]+".key")); !os.IsNotExist(err) {
		t.Fatal("replaced leaf key retained")
	}
}

func TestLeafNeverOutlivesIssuerAndExpiredIssuerFailsClosed(t *testing.T) {
	s, c := openTest(t)
	if err := s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	_, _, st := serving(t, s)
	caExpiry := st.Generations[0].NotAfter
	c.t = caExpiry.Add(-10 * 24 * time.Hour)
	if _, err := s.Ensure(testNames); err != nil {
		t.Fatal(err)
	}
	leaf, _, _ := serving(t, s)
	if leaf.NotAfter.After(caExpiry) {
		t.Fatalf("leaf %s outlives CA %s", leaf.NotAfter, caExpiry)
	}
	c.t = caExpiry.Add(-30 * time.Minute)
	if err := s.Renew(); err == nil || !strings.Contains(err.Error(), "rotate the CA") {
		t.Fatalf("near-expiry issuer accepted: %v", err)
	}
	if after, _, _ := serving(t, s); !after.Equal(leaf) {
		t.Fatal("failed renewal replaced the previous valid leaf")
	}
}

func TestRoutineRotationStagesAndRestart(t *testing.T) {
	s, c := openTest(t)
	if err := s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	_, trustA, _ := serving(t, s)
	if err := s.SwitchRotation(true); err == nil {
		t.Fatal("switch without prepare accepted")
	}
	if err := s.PrepareRotation(); err != nil {
		t.Fatal(err)
	}
	leaf, trustAB, st := serving(t, s)
	if st.Rotation.Stage != StagePrepared || len(st.Trusted) != 2 || leaf.CheckSignatureFrom(mustCA(t, s, 1)) != nil {
		t.Fatalf("prepare must keep serving A and trust A+B: %+v", st)
	}
	// An A-only consumer still verifies the A leaf while B is distributed.
	if err := VerifyServing(leaf, trustA, testNames, c.t); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareRotation(); err == nil {
		t.Fatal("second prepare accepted")
	}
	if err := s.SwitchRotation(false); err == nil {
		t.Fatal("switch without distribution evidence accepted")
	}
	// Restart: reopen the store and continue from the journaled stage.
	reopened, err := Open(s.Dir(), DefaultPolicy, c.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.SwitchRotation(true); err != nil {
		t.Fatal(err)
	}
	leafB, trustNow, st := serving(t, reopened)
	if leafB.CheckSignatureFrom(mustCA(t, reopened, 2)) != nil || st.Rotation.Stage != StageSwitched {
		t.Fatal("switch must serve a B leaf")
	}
	if err = VerifyServing(leafB, trustAB, testNames, c.t); err != nil {
		t.Fatalf("A+B consumer rejects B leaf: %v", err)
	}
	if VerifyServing(leafB, trustA, testNames, c.t) == nil {
		t.Fatal("A-only consumer unexpectedly trusts B")
	}
	if err = reopened.RollbackRotation(); err != nil {
		t.Fatal(err)
	}
	if rolled, _, _ := serving(t, reopened); rolled.CheckSignatureFrom(mustCA(t, reopened, 1)) != nil {
		t.Fatal("rollback must serve A again")
	}
	if err = reopened.SwitchRotation(true); err != nil {
		t.Fatal(err)
	}
	if err = reopened.RetireRotation(false); err == nil {
		t.Fatal("retire without drain evidence accepted")
	}
	if err = reopened.RetireRotation(true); err != nil {
		t.Fatal(err)
	}
	_, trustB, st := serving(t, reopened)
	if st.Rotation != nil || len(st.Trusted) != 1 || st.Trusted[0] != 2 || string(trustB) == string(trustNow) {
		t.Fatalf("retire must trust only B: %+v", st)
	}
	if _, err = os.Stat(filepath.Join(reopened.Dir(), "ca-1.key")); !os.IsNotExist(err) {
		t.Fatal("retired CA key retained")
	}
}

func TestMissingSigningKeyStopsIssuanceWithoutNewCA(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Dir(), "ca-1.key")); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(); err == nil || !strings.Contains(err.Error(), "replace the CA") {
		t.Fatalf("renew without CA key: %v", err)
	}
	if _, _, st := serving(t, s); len(st.Generations) != 1 {
		t.Fatal("missing key silently created a CA")
	}
	if err := s.ReplaceCA(); err != nil {
		t.Fatal(err)
	}
	leaf, trust, st := serving(t, s)
	if len(st.Trusted) != 1 || st.Trusted[0] != 2 || !st.Generations[0].Retired {
		t.Fatalf("emergency replacement state: %+v", st)
	}
	if err := VerifyServing(leaf, trust, testNames, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func TestStateCorruptionAndUncommittedGenerationAreSafe(t *testing.T) {
	s, _ := openTest(t)
	if _, err := s.State(); err != ErrNotInitialized {
		t.Fatalf("empty store state: %v", err)
	}
	// A crash after writing ca-1 but before committing state leaves orphans.
	orphan, err := NewCA("orphan", time.Now(), time.Hour*24*365)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writePair("ca-1", orphan); err != nil {
		t.Fatal(err)
	}
	// Uncommitted authority files are indistinguishable from a lost state
	// file, so neither initialization nor renewal may proceed.
	if err = s.Initialize(testNames); !errors.Is(err, ErrAuthorityMaterial) {
		t.Fatalf("initialized over existing authority files: %v", err)
	}
	if _, err = s.Ensure(testNames); !errors.Is(err, ErrAuthorityMaterial) {
		t.Fatalf("ensure over existing authority files: %v", err)
	}
	if ca := mustCA(t, s, 1); !strings.Contains(ca.Subject.CommonName, "orphan") {
		t.Fatal("existing authority files were replaced")
	}
	if err = os.Remove(filepath.Join(s.Dir(), "ca-1.pem")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(s.Dir(), "ca-1.key")); err != nil {
		t.Fatal(err)
	}
	if err = s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.Dir(), "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Ensure(testNames); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt state accepted: %v", err)
	}
}

func TestLostStateNeverMintsAnUnrelatedAuthority(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	original := mustCA(t, s, 1)
	if err := os.Remove(filepath.Join(s.Dir(), "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(testNames); !errors.Is(err, ErrAuthorityMaterial) {
		t.Fatalf("ensure after lost state: %v", err)
	}
	if err := s.Initialize(testNames); !errors.Is(err, ErrAuthorityMaterial) {
		t.Fatalf("initialize after lost state: %v", err)
	}
	if after := mustCA(t, s, 1); !after.Equal(original) {
		t.Fatal("lost state replaced the authority certificate")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "ca-1.key")); err != nil {
		t.Fatalf("authority key removed: %v", err)
	}
}

func TestNamesRejectWildcardsAndEmpty(t *testing.T) {
	for _, n := range []Names{{}, {DNS: []string{"*.example.com"}}, {IPs: []string{"0.0.0.0"}}, {DNS: []string{"10.0.0.1"}}} {
		if _, err := n.normalized(); err == nil {
			t.Fatalf("accepted %+v", n)
		}
	}
}

func TestServingCertificateHandshakes(t *testing.T) {
	s, c := openTest(t)
	c.t = time.Now()
	if err := s.Initialize(Names{DNS: []string{openAIHost}, IPs: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	cert, trust, _, err := s.Serving()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(trust)
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: roots, ServerName: openAIHost, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func mustCA(t *testing.T, s *Store, id int) *x509.Certificate {
	t.Helper()
	ca, err := s.caCertificate(id)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestTrustConsumersGateRotationEvidence(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Initialize(testNames); err != nil {
		t.Fatal(err)
	}
	trustA, err := s.TrustFor("alloc-a", []byte(`{"allocation":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PrepareRotation(); err != nil {
		t.Fatal(err)
	}
	trustAB, err := s.TrustFor("alloc-b", nil)
	if err != nil || len(trustAB) <= len(trustA) {
		t.Fatalf("new consumer must receive combined trust: %v", err)
	}
	lacking, err := s.Lacking(2)
	if err != nil || len(lacking) != 1 || lacking[0].ID != "alloc-a" || string(lacking[0].Reference) != `{"allocation":"a"}` {
		t.Fatalf("lacking = %+v, %v", lacking, err)
	}
	if err = s.Release("alloc-a"); err != nil {
		t.Fatal(err)
	}
	if lacking, err = s.Lacking(2); err != nil || len(lacking) != 0 {
		t.Fatalf("released consumer still blocks: %+v %v", lacking, err)
	}
	if _, err = s.TrustFor("../escape", nil); err == nil {
		t.Fatal("unsafe consumer id accepted")
	}
}
