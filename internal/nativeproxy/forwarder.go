// Package nativeproxy relays a managed harness's proxy connections to the
// Sproozi gateway with the current projected workload token.
//
// The kubelet refreshes a Pod's projected ServiceAccount token, but a harness
// reads its proxy URL once, from the environment, at launch. Putting the token
// in that URL ties a session's gateway access to the token's lifetime. The
// forwarder instead listens on loopback, accepts CONNECT requests that carry a
// secret made for this launch, and opens each tunnel to the gateway over TLS
// with the token read from the projected file at that moment.
//
// Every container in a Pod shares its loopback interface. The launch secret
// keeps the forwarder from lending the token to a container that cannot read
// it, such as the credential-free checkpoint helper.
package nativeproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// proxyUser is the user name in both the harness's proxy URL and the
	// gateway's Proxy-Authorization.
	proxyUser = "sproozi"
	// maxTokenBytes bounds the projected token file.
	maxTokenBytes = 16 << 10
	// headTimeout bounds reading a client's CONNECT and the gateway's reply.
	headTimeout = 10 * time.Second
)

// Config names the gateway and the projected token.
type Config struct {
	// Gateway is the gateway's https endpoint, with no user, path or query.
	Gateway *url.URL
	// TokenPath is the projected token file the kubelet keeps fresh.
	TokenPath string
	// Trust verifies the gateway's certificate.
	Trust *x509.CertPool
	// ServerName overrides the name verified in the gateway's certificate.
	// Empty uses the endpoint's host name.
	ServerName string
}

// Forwarder is a running loopback relay. Close stops it.
type Forwarder struct {
	config   Config
	listener net.Listener
	secret   string
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Start checks the token is readable now, so a launch without one fails
// before the harness starts, and listens on an ephemeral loopback port.
func Start(c Config) (*Forwarder, error) {
	if c.Gateway == nil || c.Gateway.Scheme != "https" || c.Gateway.Host == "" || c.Gateway.User != nil || c.Gateway.Path != "" || c.Gateway.RawQuery != "" || c.Trust == nil {
		return nil, errors.New("invalid gateway forwarder configuration")
	}
	if _, err := readToken(c.TokenPath); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{config: c, listener: listener, secret: hex.EncodeToString(secret), ctx: ctx, cancel: cancel}
	f.wg.Add(1)
	go f.serve()
	return f, nil
}

// ProxyURL is the harness's HTTP(S)_PROXY value.
func (f *Forwarder) ProxyURL() string {
	return (&url.URL{Scheme: "http", User: url.UserPassword(proxyUser, f.secret), Host: f.listener.Addr().String()}).String()
}

// Close stops accepting connections and ends open tunnels.
func (f *Forwarder) Close() error {
	f.cancel()
	err := f.listener.Close()
	f.wg.Wait()
	return err
}

func (f *Forwarder) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.wg.Go(func() {
			f.relay(conn)
		})
	}
}

func (f *Forwarder) relay(client net.Conn) {
	defer func() { _ = client.Close() }()
	stop := context.AfterFunc(f.ctx, func() { _ = client.Close() })
	defer stop()
	_ = client.SetDeadline(time.Now().Add(headTimeout))
	reader := bufio.NewReader(client)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if request.Method != http.MethodConnect {
		reply(client, http.StatusMethodNotAllowed, "")
		return
	}
	if !f.authorized(request.Header.Get("Proxy-Authorization")) {
		reply(client, http.StatusProxyAuthRequired, "Proxy-Authenticate: Basic realm=\"sproozi\"\r\n")
		return
	}
	// No tunnel bytes may precede the gateway's acknowledgement.
	if reader.Buffered() != 0 || strings.ContainsAny(request.Host, "\r\n ") {
		reply(client, http.StatusBadRequest, "")
		return
	}
	token, err := readToken(f.config.TokenPath)
	if err != nil {
		reply(client, http.StatusBadGateway, "")
		return
	}
	upstream, err := f.dial()
	if err != nil {
		reply(client, http.StatusBadGateway, "")
		return
	}
	defer func() { _ = upstream.Close() }()
	stopUpstream := context.AfterFunc(f.ctx, func() { _ = upstream.Close() })
	defer stopUpstream()
	credential := base64.StdEncoding.EncodeToString([]byte(proxyUser + ":" + token))
	head := "CONNECT " + request.Host + " HTTP/1.1\r\nHost: " + request.Host + "\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n"
	_ = upstream.SetDeadline(time.Now().Add(headTimeout))
	if _, err = io.WriteString(upstream, head); err != nil {
		reply(client, http.StatusBadGateway, "")
		return
	}
	_ = client.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	// The gateway's reply, success or refusal, reaches the harness unchanged.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (f *Forwarder) dial() (net.Conn, error) {
	name := f.config.ServerName
	if name == "" {
		name = f.config.Gateway.Hostname()
	}
	host := f.config.Gateway.Host
	if f.config.Gateway.Port() == "" {
		host = net.JoinHostPort(f.config.Gateway.Hostname(), "443")
	}
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: headTimeout},
		Config: &tls.Config{RootCAs: f.config.Trust, ServerName: name, MinVersion: tls.VersionTLS12}}
	ctx, cancel := context.WithTimeout(f.ctx, headTimeout)
	defer cancel()
	return dialer.DialContext(ctx, "tcp", host)
}

func (f *Forwarder) authorized(header string) bool {
	encoded, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return false
	}
	user, password, ok := strings.Cut(string(decoded), ":")
	return ok && user == proxyUser && subtle.ConstantTimeCompare([]byte(password), []byte(f.secret)) == 1
}

func reply(conn net.Conn, status int, headers string) {
	_, _ = io.WriteString(conn, "HTTP/1.1 "+strconv.Itoa(status)+" "+http.StatusText(status)+"\r\n"+headers+"Content-Length: 0\r\nConnection: close\r\n\r\n")
}

// readToken reads the projected token as the harness's environment used to.
func readToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("projected native token is unavailable")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > maxTokenBytes {
		return "", errors.New("projected native token is unreadable or oversized")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\x00\r\n\t ") {
		return "", errors.New("invalid projected native token")
	}
	return token, nil
}
