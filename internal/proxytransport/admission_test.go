package proxytransport

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

func admissionConfig(t *testing.T) Config {
	t.Helper()
	return Config{Certificates: map[string]tls.Certificate{testServiceHost: testCertificate(t)},
		Authenticate: func(context.Context, string) (*gateway.ExecutionGrant, error) { return testIdentity(), nil },
		Revalidate:   func(context.Context, *gateway.ExecutionGrant) error { return nil },
		Handler:      http.NotFoundHandler(), AllowedAuthority: onlyTestService, AuthorityTimeout: 40 * time.Millisecond}
}

func connectRequest(token string) *http.Request {
	return &http.Request{Method: http.MethodConnect, Host: testServiceAuthority,
		URL: &url.URL{Host: testServiceAuthority}, Header: http.Header{proxyAuthorizationHeader: []string{"Bearer " + token}}}
}

func TestAuthenticationDeadlineRetainsWorkerAdmissionUntilReturn(t *testing.T) {
	config := admissionConfig(t)
	config.MaxConnections = 1
	release := make(chan struct{})
	returned := make(chan struct{})
	var calls atomic.Int32
	config.Authenticate = func(context.Context, string) (*gateway.ExecutionGrant, error) {
		if calls.Add(1) == 1 {
			<-release
			close(returned)
		}
		return testIdentity(), nil
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { g.ServeHTTP(first, connectRequest("first")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("authentication deadline did not return")
	}
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("first status = %d", first.Code)
	}
	second := httptest.NewRecorder()
	g.ServeHTTP(second, connectRequest("second"))
	if second.Code != http.StatusUnauthorized || calls.Load() != 1 {
		close(release)
		t.Fatalf("pending callback admission = status %d, calls %d", second.Code, calls.Load())
	}
	close(release)
	<-returned
	// The callback relinquishes its admission when its worker has returned.
	for deadline := time.Now().Add(time.Second); ; {
		third := httptest.NewRecorder()
		g.ServeHTTP(third, connectRequest("third"))
		if third.Code == http.StatusInternalServerError {
			break
		} // authenticated; recorder cannot hijack
		if time.Now().After(deadline) {
			t.Fatalf("released callback status = %d", third.Code)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPreAuthenticationAdmissionReturnsCapacityAndRecovers(t *testing.T) {
	config := admissionConfig(t)
	config.MaxConnections = 1
	config.AuthorityTimeout = time.Second
	entered, release := make(chan struct{}), make(chan struct{})
	config.Authenticate = func(context.Context, string) (*gateway.ExecutionGrant, error) {
		close(entered)
		<-release
		return testIdentity(), nil
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { g.ServeHTTP(first, connectRequest("first")); close(done) }()
	<-entered
	second := httptest.NewRecorder()
	g.ServeHTTP(second, connectRequest("second"))
	close(release)
	<-done
	if first.Code != http.StatusInternalServerError || second.Code != http.StatusServiceUnavailable {
		t.Fatalf("admission statuses = %d, %d", first.Code, second.Code)
	}
	// A non-CONNECT request is handled normally after the first returns.
	third := httptest.NewRecorder()
	g.ServeHTTP(third, httptest.NewRequest(http.MethodGet, "/", nil))
	if third.Code != http.StatusMethodNotAllowed {
		t.Fatalf("recovered status = %d", third.Code)
	}
}

func TestExecutionSessionAdmissionIsIndependentAndReleased(t *testing.T) {
	config := admissionConfig(t)
	config.MaxConnections = 4
	config.MaxSessionsPerExecution = 1
	config.Authenticate = func(_ context.Context, token string) (*gateway.ExecutionGrant, error) {
		identity := testIdentity()
		if token == "other" {
			identity.Execution.UID = "other-execution"
		}
		return identity, nil
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	session := startProxySession(t, g, testProxyAuthorization)
	same, other := httptest.NewRecorder(), httptest.NewRecorder()
	g.ServeHTTP(same, connectRequest("token"))
	g.ServeHTTP(other, connectRequest("other"))
	_ = session.client.Close()
	<-session.serveDone
	if same.Code != http.StatusTooManyRequests || other.Code != http.StatusInternalServerError {
		t.Fatalf("execution admission statuses = %d, %d", same.Code, other.Code)
	}
	recovered := httptest.NewRecorder()
	g.ServeHTTP(recovered, connectRequest("token"))
	if recovered.Code != http.StatusInternalServerError {
		t.Fatalf("released session status = %d", recovered.Code)
	}
}

func TestRevalidationDeadlineClosesSessionWhileCallbackIsPending(t *testing.T) {
	config := admissionConfig(t)
	config.RevocationInterval = 10 * time.Millisecond
	release, entered := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	config.Revalidate = func(context.Context, *gateway.ExecutionGrant) error {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	session := startProxySession(t, g, testProxyAuthorization)
	defer close(release)
	defer func() { _ = session.client.Close() }()
	<-entered
	_ = session.client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := session.client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("timed out session read = %v", err)
	}
	select {
	case <-session.serveDone:
	case <-time.After(time.Second):
		t.Fatal("session monitor did not finish")
	}
}

func TestPerRequestRevalidationDeadlineDeniesBeforeDispatch(t *testing.T) {
	config := admissionConfig(t)
	config.RevocationInterval = 5 * time.Second
	release := make(chan struct{})
	var calls atomic.Int32
	config.Revalidate = func(context.Context, *gateway.ExecutionGrant) error {
		if calls.Add(1) == 2 {
			<-release
		}
		return nil
	}
	config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "approved") })
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	session := startProxySession(t, g, testProxyAuthorization)
	defer close(release)
	defer func() { _ = session.client.Close() }()
	_ = session.client.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(session.client, "GET / HTTP/1.1\r\nHost: service.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(session.client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("revalidation status = %d", response.StatusCode)
	}
}

func TestListenerAdmissionClosesExcessAndRecoversAfterClose(t *testing.T) {
	config := admissionConfig(t)
	config.MaxConnections = 1
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := g.LimitListener(raw)
	defer func() { _ = listener.Close() }()
	first, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	admitted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admitted.Close() }()
	next := make(chan net.Conn, 1)
	go func() { connection, _ := listener.Accept(); next <- connection }()
	second, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := second.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("excess connection read = %v", err)
	}
	_ = second.Close()
	_ = admitted.Close()
	_ = admitted.Close()
	third, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	select {
	case recovered := <-next:
		if recovered == nil {
			t.Fatal("listener failed to admit after close")
		}
		_ = recovered.Close()
	case <-time.After(time.Second):
		t.Fatal("listener admission did not recover")
	}
}

func TestGatewayRequiresAuthorityRouting(t *testing.T) {
	config := admissionConfig(t)
	config.AllowedAuthority = nil
	if _, err := New(config); err == nil {
		t.Fatal("gateway accepted no authority routing")
	}
}
