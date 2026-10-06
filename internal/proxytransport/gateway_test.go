package proxytransport

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	testServiceAuthority = "service.test:443"
)

const (
	testProxyAuthorization = "Bearer token"
	testServiceHost        = "service.test"
)

type pipeResponseWriter struct {
	header http.Header
	conn   net.Conn
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }
func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	return w.conn.Write(p)
}
func (w *pipeResponseWriter) WriteHeader(int) {}
func (w *pipeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: testServiceHost}, DNSNames: []string{testServiceHost}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func testIdentity() *gateway.RunIdentity {
	return &gateway.RunIdentity{SAName: "run-sa", SANamespace: "agents", SAUID: "sa-uid"}
}

type proxySession struct {
	client    net.Conn
	serveDone chan struct{}
}

func startProxySession(t *testing.T, g *Gateway, auth string, serverName string) proxySession {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	request := &http.Request{Method: http.MethodConnect, Host: testServiceAuthority, URL: &url.URL{Host: testServiceAuthority}, Header: http.Header{proxyAuthorizationHeader: []string{auth}}}
	writer := &pipeResponseWriter{header: make(http.Header), conn: serverConn}
	done := make(chan struct{})
	go func() { defer close(done); g.ServeHTTP(writer, request) }()
	clientReader := bufio.NewReader(clientConn)
	if _, err := clientReader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientReader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	tlsClient := tls.Client(&bufferedConn{Conn: clientConn, reader: clientReader}, &tls.Config{InsecureSkipVerify: true, ServerName: serverName, MinVersion: tls.VersionTLS12})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatal(err)
	}
	return proxySession{client: tlsClient, serveDone: done}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func TestGatewayProxyTokenModes(t *testing.T) {
	identity := testIdentity()
	cert := testCertificate(t)
	for _, tc := range []struct{ name, auth string }{
		{"bearer", testProxyAuthorization},
		{"basic", "Basic " + base64.StdEncoding.EncodeToString([]byte("sproozi:token"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *gateway.RunIdentity
			g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: cert}, Authenticate: func(_ context.Context, token string) (*gateway.RunIdentity, error) {
				if token != "token" {
					t.Fatalf("token = %q", token)
				}
				return identity, nil
			}, Revalidate: func(context.Context, *gateway.RunIdentity) error { return nil }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = Identity(r.Context())
				if r.Header.Get("Authorization") != "" || r.Header.Get(proxyAuthorizationHeader) != "" {
					t.Error("proxy credentials reached semantic handler")
				}
				_, _ = io.WriteString(w, "ok")
			}), RevocationInterval: 20 * time.Millisecond, SessionLifetime: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			s := startProxySession(t, g, tc.auth, testServiceHost)
			defer func() { _ = s.client.Close() }()
			if _, err := io.WriteString(s.client, "GET / HTTP/1.1\r\nHost: service.test\r\nAuthorization: Bearer upstream\r\nProxy-Authorization: Basic leaked\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(s.client), nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if got != identity {
				t.Fatal("handler did not receive authenticated identity")
			}
		})
	}
}

func TestStandardClientUsesTLSAuthenticatedProxyAndInspectedTLS(t *testing.T) {
	for _, host := range []string{testServiceHost, "service.test:8443"} {
		t.Run(host, func(t *testing.T) {
			identity := testIdentity()
			var gotHost string
			g, err := New(Config{
				AllowedAuthority: func(authority string) bool {
					return authority == testServiceAuthority || authority == "service.test:8443"
				},
				Certificates: map[string]tls.Certificate{testServiceHost: testCertificate(t)},
				Authenticate: func(_ context.Context, token string) (*gateway.RunIdentity, error) {
					if token != "token" {
						t.Fatalf("token = %q", token)
					}
					return identity, nil
				},
				Revalidate: func(context.Context, *gateway.RunIdentity) error { return nil },
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotHost = r.Host
					_, _ = io.WriteString(w, "inspected")
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewUnstartedServer(g)
			proxy.EnableHTTP2 = false
			proxy.StartTLS()
			defer proxy.Close()
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			proxyURL.User = url.UserPassword("sproozi", "token")
			client := &http.Client{Transport: &http.Transport{
				Proxy:             http.ProxyURL(proxyURL),
				ForceAttemptHTTP2: false,
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
			}}
			response, err := client.Get("https://" + host + "/demo")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || string(body) != "inspected" || gotHost != host {
				t.Fatalf("status/body/host = %d/%q/%q", response.StatusCode, body, gotHost)
			}
		})
	}
}

func TestGatewayDeniesUnknownHostAndSNIMismatchWithoutHandler(t *testing.T) {
	called := 0
	identity := testIdentity()
	g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: testCertificate(t)}, Authenticate: func(context.Context, string) (*gateway.RunIdentity, error) { return identity, nil }, Revalidate: func(context.Context, *gateway.RunIdentity) error { return nil }, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ })})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"unknown.test:443", "service.test:80"} {
		server, client := net.Pipe()
		w := &pipeResponseWriter{header: make(http.Header), conn: server}
		r := &http.Request{Method: http.MethodConnect, Host: host, URL: &url.URL{Host: host}, Header: http.Header{proxyAuthorizationHeader: []string{testProxyAuthorization}}}
		go g.ServeHTTP(w, r)
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		line, _ := bufio.NewReader(client).ReadString('\n')
		if !strings.Contains(line, "unsupported destination") {
			t.Fatalf("host %s response = %q", host, line)
		}
		_ = client.Close()
	}
	if called != 0 {
		t.Fatalf("handler calls = %d", called)
	}
}

func TestGatewayRejectsAuthorityDisagreementAndSNI(t *testing.T) {
	identity := testIdentity()
	g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: testCertificate(t)}, Authenticate: func(context.Context, string) (*gateway.RunIdentity, error) { return identity, nil }, Revalidate: func(context.Context, *gateway.RunIdentity) error { return nil }, Handler: http.NotFoundHandler()})
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	w := &pipeResponseWriter{header: make(http.Header), conn: server}
	r := &http.Request{Method: http.MethodConnect, Host: testServiceAuthority, URL: &url.URL{Host: "other.test:443"}, Header: http.Header{proxyAuthorizationHeader: []string{testProxyAuthorization}}}
	go g.ServeHTTP(w, r)
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if body, _ := io.ReadAll(client); !strings.Contains(string(body), "unsupported destination") {
		t.Fatalf("authority disagreement response = %q", body)
	}
	_ = client.Close()

	server, client = net.Pipe()
	w = &pipeResponseWriter{header: make(http.Header), conn: server}
	r = &http.Request{Method: http.MethodConnect, Host: testServiceAuthority, URL: &url.URL{Host: testServiceAuthority}, Header: http.Header{proxyAuthorizationHeader: []string{testProxyAuthorization}}}
	go g.ServeHTTP(w, r)
	reader := bufio.NewReader(client)
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	tlsClient := tls.Client(&bufferedConn{Conn: client, reader: reader}, &tls.Config{InsecureSkipVerify: true, ServerName: "other.test"})
	if err := tlsClient.Handshake(); err == nil {
		t.Fatal("SNI mismatch unexpectedly succeeded")
	}
	_ = client.Close()
}

func TestGatewaySessionLifetimeClosesConnection(t *testing.T) {
	identity := testIdentity()
	g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: testCertificate(t)}, Authenticate: func(context.Context, string) (*gateway.RunIdentity, error) { return identity, nil }, Revalidate: func(context.Context, *gateway.RunIdentity) error { return nil }, Handler: http.NotFoundHandler(), SessionLifetime: time.Second, RevocationInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s := startProxySession(t, g, testProxyAuthorization, testServiceHost)
	defer func() { _ = s.client.Close() }()
	_ = s.client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := s.client.Read(buf); err == nil {
		t.Fatal("session remained open after lifetime")
	}
	select {
	case <-s.serveDone:
	case <-time.After(time.Second):
		t.Fatal("gateway ServeHTTP did not return after lifetime")
	}
}

func TestGatewayRevocationClosesActiveSession(t *testing.T) {
	var mu sync.Mutex
	revoked := false
	identity := testIdentity()
	g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: testCertificate(t)}, Authenticate: func(context.Context, string) (*gateway.RunIdentity, error) { return identity, nil }, Revalidate: func(context.Context, *gateway.RunIdentity) error {
		mu.Lock()
		defer mu.Unlock()
		if revoked {
			return fmt.Errorf("revoked")
		}
		return nil
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), RevocationInterval: 20 * time.Millisecond, SessionLifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s := startProxySession(t, g, testProxyAuthorization, testServiceHost)
	mu.Lock()
	revoked = true
	mu.Unlock()
	_ = s.client.SetReadDeadline(time.Now().Add(time.Second))
	for {
		_, err = s.client.Write([]byte("GET / HTTP/1.1\r\nHost: service.test\r\n\r\n"))
		if err != nil {
			break
		}
		buf := make([]byte, 1)
		if _, err = s.client.Read(buf); err != nil {
			break
		}
	}
	select {
	case <-s.serveDone:
	case <-time.After(time.Second):
		t.Fatal("session did not close after revocation")
	}
}
