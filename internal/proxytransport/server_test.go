package proxytransport

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
)

func TestProxyListenerRequiresTLSAndHijackableHTTP1(t *testing.T) {
	server := NewServer(":8443", NewCertificate(tls.Certificate{}), http.NotFoundHandler())
	if server.TLSConfig == nil || server.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("proxy listener TLS config = %#v", server.TLSConfig)
	}
	if server.TLSNextProto == nil || len(server.TLSNextProto) != 0 {
		t.Fatalf("proxy listener must explicitly disable HTTP/2: %#v", server.TLSNextProto)
	}
}

func TestReplacedCertificateServesNewHandshakes(t *testing.T) {
	now := time.Now()
	ca, err := pki.NewCA("test", now, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	names := pki.Names{IPs: []string{"127.0.0.1"}}
	issue := func() tls.Certificate {
		leaf, err := pki.IssueLeaf(ca, names, now, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		c, err := tls.X509KeyPair(leaf.Certificate, leaf.Key)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := issue(), issue()
	holder := NewCertificate(first)
	server := NewServer("127.0.0.1:0", holder, http.NotFoundHandler())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	defer func() { _ = server.Close() }()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.Certificate)
	served := func() []byte {
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		return conn.ConnectionState().PeerCertificates[0].Raw
	}
	if string(served()) != string(first.Certificate[0]) {
		t.Fatal("initial certificate not served")
	}
	holder.Store(second)
	if string(served()) != string(second.Certificate[0]) {
		t.Fatal("replacement not served to a new handshake")
	}
}
