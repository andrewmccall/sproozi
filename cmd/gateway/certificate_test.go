package main

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

func writeLeaf(t *testing.T, dir string, ca pki.Pair, names pki.Names) {
	t.Helper()
	leaf, err := pki.IssueLeaf(ca, names, time.Now(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "tls.crt"), leaf.Certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "tls.key"), leaf.Key, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayReloadsARenewedSecretAndRejectsNarrowerCertificates(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.NewCA("gateway test", time.Now(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{"sproozi-gateway.sproozi-system.svc", "kubernetes.default.svc"}
	writeLeaf(t, dir, ca, pki.Names{DNS: hosts})
	holder := proxytransport.NewCertificate(tls.Certificate{})
	r := &certificateReloader{certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key"),
		hosts: hosts, holder: holder}
	if changed, err := r.reload(); err != nil || !changed {
		t.Fatalf("initial load changed=%v err=%v", changed, err)
	}
	first := holder.Load().Leaf
	if changed, err := r.reload(); err != nil || changed {
		t.Fatalf("unchanged files reloaded: %v %v", changed, err)
	}
	writeLeaf(t, dir, ca, pki.Names{DNS: hosts})
	if changed, err := r.reload(); err != nil || !changed || holder.Load().Leaf.Equal(first) {
		t.Fatalf("renewed Secret not served: %v %v", changed, err)
	}
	renewed := holder.Load().Leaf
	writeLeaf(t, dir, ca, pki.Names{DNS: hosts[:1]})
	if _, err := r.reload(); err == nil {
		t.Fatal("certificate missing an inspected host accepted")
	}
	if !holder.Load().Leaf.Equal(renewed) {
		t.Fatal("rejected replacement displaced the serving certificate")
	}
}

func TestGatewayRejectsALeafOutsideThePublishedTrust(t *testing.T) {
	dir := t.TempDir()
	trusted, err := pki.NewCA("trusted", time.Now(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := pki.NewCA("foreign", time.Now(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{"sproozi-gateway.sproozi-system.svc"}
	if err = os.WriteFile(filepath.Join(dir, "ca.crt"), trusted.Certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	writeLeaf(t, dir, trusted, pki.Names{DNS: hosts})
	holder := proxytransport.NewCertificate(tls.Certificate{})
	r := &certificateReloader{certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key"),
		hosts: hosts, holder: holder}
	if _, err = r.reload(); err != nil {
		t.Fatal(err)
	}
	good := holder.Load().Leaf
	writeLeaf(t, dir, foreign, pki.Names{DNS: hosts})
	if _, err = r.reload(); err == nil || !holder.Load().Leaf.Equal(good) {
		t.Fatalf("leaf from an unpublished authority accepted: %v", err)
	}
}
