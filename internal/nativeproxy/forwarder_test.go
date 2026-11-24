package nativeproxy

import (
	"bufio"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServerName is the name in httptest's certificate.
const testServerName = "example.com"

// fakeGateway accepts CONNECT over TLS, records each Proxy-Authorization and
// then echoes the tunnel's bytes.
type fakeGateway struct {
	server *httptest.Server
	mu     sync.Mutex
	auth   []string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	g.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Proxy-Authorization"))
		g.mu.Unlock()
		if r.Method != http.MethodConnect || r.Host != "api.example.test:443" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		_, _ = io.Copy(conn, buffered)
	}))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) authorizations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth...)
}

func basic(password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+password))
}

func start(t *testing.T, g *fakeGateway, token string) (*Forwarder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(g.server.Certificate())
	endpoint, err := url.Parse(g.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Start(Config{Gateway: endpoint, TokenPath: path, Trust: pool, ServerName: testServerName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

// connect opens a tunnel through the forwarder with the given proxy
// credential and returns the response status and the connection.
func connect(t *testing.T, f *Forwarder, authorization, method string) (int, net.Conn) {
	t.Helper()
	proxy, err := url.Parse(f.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", proxy.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	head := method + " api.example.test:443 HTTP/1.1\r\nHost: api.example.test:443\r\n"
	if authorization != "" {
		head += "Proxy-Authorization: " + authorization + "\r\n"
	}
	if _, err = conn.Write([]byte(head + "\r\n")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, conn
}

func TestForwarderSendsTheCurrentTokenOnEachConnect(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t)
	f, path := start(t, g, "first-token")
	proxy, err := url.Parse(f.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := proxy.User.Password()
	if proxy.Scheme != "http" || proxy.User.Username() != proxyUser || len(secret) < 32 || !strings.HasPrefix(proxy.Host, "127.0.0.1:") {
		t.Fatalf("ProxyURL() = %q, want http://%s:<secret>@127.0.0.1:<port>", f.ProxyURL(), proxyUser)
	}

	status, conn := connect(t, f, basic(secret), http.MethodConnect)
	if status != http.StatusOK {
		t.Fatalf("first CONNECT status = %d, want 200", status)
	}
	if _, err = conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err = io.ReadFull(conn, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("tunnel echo = %q, %v; want ping", echo, err)
	}

	// The kubelet rotates the projected token; the next CONNECT uses it.
	if err = os.WriteFile(path, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _ = connect(t, f, basic(secret), http.MethodConnect); status != http.StatusOK {
		t.Fatalf("second CONNECT status = %d, want 200", status)
	}
	got := g.authorizations()
	want := []string{basic("first-token"), basic("rotated-token")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("gateway saw %q, want %q", got, want)
	}
}

// Other containers in the Pod share its loopback; without the launch secret
// they get nothing from the forwarder, and the gateway sees no request.
func TestForwarderRefusesWithoutTheLaunchSecret(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t)
	f, _ := start(t, g, "token")
	for _, authorization := range []string{"", basic("wrong"), basic("token")} {
		if status, _ := connect(t, f, authorization, http.MethodConnect); status != http.StatusProxyAuthRequired {
			t.Errorf("CONNECT with %q: status = %d, want 407", authorization, status)
		}
	}
	if got := g.authorizations(); len(got) != 0 {
		t.Fatalf("gateway saw %d requests, want none", len(got))
	}
}

func TestForwarderRefusesPlainHTTPRequests(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t)
	f, _ := start(t, g, "token")
	proxy, _ := url.Parse(f.ProxyURL())
	secret, _ := proxy.User.Password()
	if status, _ := connect(t, f, basic(secret), http.MethodGet); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", status)
	}
}

func TestForwarderRefusesAnUntrustedGateway(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(g.server.URL)
	f, err := Start(Config{Gateway: endpoint, TokenPath: path, Trust: x509.NewCertPool(), ServerName: testServerName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	proxy, _ := url.Parse(f.ProxyURL())
	secret, _ := proxy.User.Password()
	if status, _ := connect(t, f, basic(secret), http.MethodConnect); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if got := g.authorizations(); len(got) != 0 {
		t.Fatalf("an untrusted gateway received %d requests carrying the token", len(got))
	}
}

func TestStartRequiresAReadableToken(t *testing.T) {
	t.Parallel()
	endpoint, _ := url.Parse("https://gateway.invalid:8443")
	for name, content := range map[string]string{"missing": "", "empty": "\n", "spaced": "a b"} {
		path := filepath.Join(t.TempDir(), "token")
		if content != "" {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if f, err := Start(Config{Gateway: endpoint, TokenPath: path, Trust: x509.NewCertPool()}); err == nil {
			_ = f.Close()
			t.Errorf("%s token: Start() error = nil", name)
		}
	}
}

// projectedVolume models the directory the kubelet keeps for a projected
// volume mounted without subPath: each update is a new timestamped directory,
// ..data is a symlink to the current one, and the token is a symlink through
// ..data. The kubelet swaps ..data with an atomic rename and then removes
// the old directory.
type projectedVolume struct {
	t       *testing.T
	dir     string
	current string
	updates int
}

func newProjectedVolume(t *testing.T, token string) *projectedVolume {
	t.Helper()
	v := &projectedVolume{t: t, dir: t.TempDir()}
	v.publish(token)
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(v.dir, "token")); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *projectedVolume) publish(token string) {
	v.t.Helper()
	v.updates++
	data := fmt.Sprintf("..2026_11_17_07_00_%02d.%09d", v.updates, v.updates)
	if err := os.Mkdir(filepath.Join(v.dir, data), 0o755); err != nil {
		v.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.dir, data, "token"), []byte(token), 0o444); err != nil {
		v.t.Fatal(err)
	}
	if err := os.Symlink(data, filepath.Join(v.dir, "..data_tmp")); err != nil {
		v.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(v.dir, "..data_tmp"), filepath.Join(v.dir, "..data")); err != nil {
		v.t.Fatal(err)
	}
	if v.current != "" {
		if err := os.RemoveAll(filepath.Join(v.dir, v.current)); err != nil {
			v.t.Fatal(err)
		}
	}
	v.current = data
}

// A Kubernetes sandbox mounts the projected token's directory, so the
// kubelet's refresh is a ..data swap rather than a rewrite of the file. Each
// tunnel must carry the token current when it opens, never the one the
// forwarder read before.
func TestForwarderFollowsTheKubeletsTokenSwap(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t)
	volume := newProjectedVolume(t, "first-token")
	pool := x509.NewCertPool()
	pool.AddCert(g.server.Certificate())
	endpoint, err := url.Parse(g.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Start(Config{Gateway: endpoint, TokenPath: filepath.Join(volume.dir, "token"), Trust: pool, ServerName: testServerName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	proxy, _ := url.Parse(f.ProxyURL())
	secret, _ := proxy.User.Password()

	want := make([]string, 0, 3)
	want = append(want, basic("first-token"))
	if status, _ := connect(t, f, basic(secret), http.MethodConnect); status != http.StatusOK {
		t.Fatalf("first CONNECT status = %d, want 200", status)
	}
	for _, token := range []string{"second-token", "third-token"} {
		volume.publish(token)
		want = append(want, basic(token))
		if status, _ := connect(t, f, basic(secret), http.MethodConnect); status != http.StatusOK {
			t.Fatalf("CONNECT after publishing %s: status = %d, want 200", token, status)
		}
	}
	if got := g.authorizations(); !slices.Equal(got, want) {
		t.Fatalf("gateway saw %q, want %q", got, want)
	}
}
