package proxytransport

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	memoryChildEnv     = "SPROOZI_SESSION_MEMORY_CHILD"
	memoryGatewayEnv   = "SPROOZI_SESSION_MEMORY_GATEWAY"
	memoryWarmSessions = 16
	memorySessions     = 256
	memoryChunkBytes   = 16 << 10
)

// TestInspectedSessionMemory measures the gateway memory one inspected session
// holds while it streams: the outer ServeTLS connection, the inspected
// tls.Server, the per-session HTTP server and revocation monitor, the upstream
// TLS client connection to a streaming httptest TLS server, and a 32 KiB copy
// buffer. Workload clients and the upstream run in a child process, so the
// figure counts the gateway alone. It backs the per-session estimate behind
// DefaultMaxConnections. Run it with:
//
//	go test ./internal/proxytransport -run TestInspectedSessionMemory -v
func TestInspectedSessionMemory(t *testing.T) {
	if os.Getenv(memoryChildEnv) != "" {
		runMemoryChild(t)
		return
	}
	if testing.Short() {
		t.Skip("measures memory across hundreds of TLS sessions")
	}
	var upstreamAddress atomic.Value
	upstream := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // local test upstream
		ForceAttemptHTTP2: false, MaxIdleConnsPerHost: memoryWarmSessions + memorySessions,
	}
	defer upstream.CloseIdleConnections()
	g, err := New(Config{
		Certificates:     map[string]tls.Certificate{testServiceHost: testCertificate(t)},
		AllowedAuthority: onlyTestService,
		Authenticate: func(_ context.Context, token string) (*gateway.ExecutionGrant, error) {
			identity := testIdentity()
			identity.Execution.UID = token // one session per execution
			return identity, nil
		},
		Revalidate: func(context.Context, *gateway.ExecutionGrant) error { return nil },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			request, err := http.NewRequestWithContext(r.Context(), r.Method,
				"https://"+upstreamAddress.Load().(string)+r.URL.Path, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			response, err := upstream.RoundTrip(request)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = response.Body.Close() }()
			w.WriteHeader(response.StatusCode)
			flusher := w.(http.Flusher)
			buffer := make([]byte, 32<<10)
			for {
				n, err := response.Body.Read(buffer)
				if n > 0 {
					if _, err := w.Write(buffer[:n]); err != nil {
						return
					}
					flusher.Flush()
				}
				if err != nil {
					return
				}
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(listener.Addr().String(), testCertificate(t), g)
	go func() { _ = server.ServeTLS(g.LimitListener(listener), "", "") }()
	defer func() { _ = server.Close() }()

	child := exec.Command(os.Args[0], "-test.run=^TestInspectedSessionMemory$")
	child.Env = append(os.Environ(), memoryChildEnv+"=1", memoryGatewayEnv+"="+listener.Addr().String())
	child.Stderr = os.Stderr
	toChild, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	fromChild, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	lines := bufio.NewScanner(fromChild)
	expect := func(prefix string) string {
		for lines.Scan() {
			if value, ok := strings.CutPrefix(lines.Text(), prefix); ok {
				return value
			}
		}
		t.Fatalf("child exited before %q: %v", prefix, lines.Err())
		return ""
	}
	upstreamAddress.Store(expect("UPSTREAM "))
	signal := func() {
		if _, err := io.WriteString(toChild, "go\n"); err != nil {
			t.Fatal(err)
		}
	}
	signal()
	expect("WARM") // first sessions paid for one-off TLS and HTTP setup
	before, beforeGoroutines := liveMemory(), runtime.NumGoroutine()
	signal()
	expect("READY")
	after, afterGoroutines := liveMemory(), runtime.NumGoroutine()
	signal()

	perSession := (after - before) / memorySessions
	t.Logf("inspected streaming session: %d bytes (%.0f KiB) of in-use heap and stack, %d goroutines",
		perSession, float64(perSession)/1024, (afterGoroutines-beforeGoroutines)/memorySessions)
	if perSession > InspectedSessionBytes {
		t.Fatalf("an inspected session holds %d bytes, above the %d byte budget behind DefaultMaxConnections",
			perSession, InspectedSessionBytes)
	}
}

// liveMemory is the in-use heap spans and goroutine stacks after a full GC.
// In-use spans, rather than live objects, include fragmentation, which the
// process also pays for.
func liveMemory() int64 {
	var stats runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapInuse + stats.StackInuse)
}

// runMemoryChild runs the workload clients and the streaming upstream for
// TestInspectedSessionMemory, outside the measured gateway process.
func runMemoryChild(t *testing.T) {
	release := make(chan struct{})
	chunk := strings.Repeat("x", memoryChunkBytes)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, chunk)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	upstream.EnableHTTP2 = false
	upstream.StartTLS()
	defer upstream.Close()
	fmt.Println("UPSTREAM " + upstream.Listener.Addr().String())
	commands := bufio.NewScanner(os.Stdin)
	wait := func() {
		if !commands.Scan() {
			t.Fatal("parent closed the command stream")
		}
	}
	gatewayAddress := os.Getenv(memoryGatewayEnv)
	var sessions []net.Conn
	open := func(first, count int) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := first; i < first+count; i++ {
			wg.Go(func() {
				connection, err := streamingSession(gatewayAddress, "token-"+strconv.Itoa(i))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				sessions = append(sessions, connection)
				mu.Unlock()
			})
		}
		wg.Wait()
		if t.Failed() {
			t.FailNow()
		}
	}
	wait()
	open(0, memoryWarmSessions)
	fmt.Println("WARM")
	wait()
	open(memoryWarmSessions, memorySessions)
	time.Sleep(100 * time.Millisecond) // let the gateway settle after the last write
	fmt.Println("READY")
	wait()
	close(release)
	for _, connection := range sessions {
		_ = connection.Close()
	}
}

// streamingSession opens one inspected session through the gateway and reads
// the first streamed chunk, leaving the response open.
func streamingSession(gatewayAddress, token string) (net.Conn, error) {
	raw, err := net.Dial("tcp", gatewayAddress)
	if err != nil {
		return nil, err
	}
	outer := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // test gateway
	_, err = fmt.Fprintf(outer, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s: Bearer %s\r\n\r\n",
		testServiceAuthority, testServiceAuthority, proxyAuthorizationHeader, token)
	if err != nil {
		return nil, err
	}
	outerReader := bufio.NewReader(outer)
	established, err := http.ReadResponse(outerReader, &http.Request{Method: http.MethodConnect})
	if err != nil || established.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CONNECT: %v %v", established, err)
	}
	inner := tls.Client(&bufferedConn{Conn: outer, reader: outerReader}, //nolint:gosec // test inspection certificate
		&tls.Config{InsecureSkipVerify: true, ServerName: testServiceHost, MinVersion: tls.VersionTLS12})
	if _, err := fmt.Fprintf(inner, "GET /stream HTTP/1.1\r\nHost: %s\r\n\r\n", testServiceHost); err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(inner), nil)
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inspected request: %v %v", response, err)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, memoryChunkBytes)); err != nil {
		return nil, err
	}
	return raw, nil
}
