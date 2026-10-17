package proxytransport

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestProxyListenerRequiresTLSAndHijackableHTTP1(t *testing.T) {
	server := NewServer(":8443", tls.Certificate{}, http.NotFoundHandler())
	if server.TLSConfig == nil || server.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("proxy listener TLS config = %#v", server.TLSConfig)
	}
	if server.TLSNextProto == nil || len(server.TLSNextProto) != 0 {
		t.Fatalf("proxy listener must explicitly disable HTTP/2: %#v", server.TLSNextProto)
	}
}
