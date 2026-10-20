package main

import (
	"bytes"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
)

const loopback = "127.0.0.1"

func TestReachReturnsTheAddressThatServedTheNonce(t *testing.T) {
	listener, err := net.Listen(tcp4, loopback+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("nonce-123"))
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	var out bytes.Buffer
	if err = probe([]string{probeReach, "localhost", port, "nonce-123"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != loopback {
		t.Fatalf("reach printed %q", out.String())
	}
	if probe([]string{probeReach, loopback, port, "other-nonce"}, nil, &out) == nil {
		t.Fatal("wrong nonce accepted")
	}
}

func TestTLSProbeVerifiesTheGatewayIP(t *testing.T) {
	now := time.Now()
	ca, err := pki.NewCA("probe", now, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := pki.IssueLeaf(ca, pki.Names{IPs: []string{loopback}}, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(leaf.Certificate, leaf.Key)
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	listener, err := tls.Listen(tcp4, loopback+":0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if err = probe([]string{probeTLS, loopback, port}, bytes.NewReader(ca.Certificate), nil); err != nil {
		t.Fatal(err)
	}
	other, err := pki.NewCA("other", now, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if probe([]string{probeTLS, loopback, port}, bytes.NewReader(other.Certificate), nil) == nil {
		t.Fatal("untrusted gateway accepted")
	}
}

func TestContractVersion(t *testing.T) {
	var out bytes.Buffer
	if err := probe([]string{"contract"}, nil, &out); err != nil || strings.TrimSpace(out.String()) != contractVersion {
		t.Fatalf("contract %q %v", out.String(), err)
	}
}
