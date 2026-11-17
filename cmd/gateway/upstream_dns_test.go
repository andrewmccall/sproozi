//go:build dnsstall

// These tests measure what one lost DNS datagram costs the gateway's model
// client, through Go's own resolver with a resolv.conf chosen by the test.
// Go reads only /etc/resolv.conf, so the test installs its configuration
// through the net package's internals, which the linker refuses without a
// flag. They therefore run only on request, pinned to this toolchain:
//
//	GOTOOLCHAIN=go1.26.11 go test -tags dnsstall -ldflags=-checklinkname=0 \
//	  -run 'DNS' -count=1 -v ./cmd/gateway/
//
// The fake DNS server binds 127.0.0.1 on an ephemeral port; the machine's own
// resolver configuration is never read or changed by a lookup here.

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/dns/dnsmessage"
)

// resolverConfigMirror has the layout of net.resolverConfig in go1.26.11
// (src/net/dnsclient_unix.go). A layout change shows up as wrong queried
// names, which every test here checks.
type resolverConfigMirror struct {
	initOnce    sync.Once
	ch          chan struct{}
	lastChecked time.Time
	dnsConfig   atomic.Pointer[byte]
}

//go:linkname systemResolvConf net.resolvConf
var systemResolvConf resolverConfigMirror

//go:linkname dnsReadConfig net.dnsReadConfig
func dnsReadConfig(filename string) unsafe.Pointer

//go:linkname getSystemDNSConfig net.getSystemDNSConfig
func getSystemDNSConfig() unsafe.Pointer

// podResolvConf is what the kubelet writes for a ClusterFirst Pod in
// sproozi-system on the Kind cluster where the stall was measured, plus
// no-reload so the test's file is never replaced by /etc/resolv.conf.
const podResolvConf = "nameserver 10.96.0.10\n" +
	"search sproozi-system.svc.cluster.local svc.cluster.local cluster.local\n" +
	"options ndots:5 no-reload\n"

// useResolvConf makes Go's resolver use podResolvConf with extra options, and
// sends every query to server instead of the configured nameserver.
func useResolvConf(t *testing.T, server string, options ...string) {
	t.Helper()
	if !strings.HasPrefix(runtime.Version(), "go1.26.") {
		t.Skipf("resolverConfigMirror matches go1.26, not %s", runtime.Version())
	}
	content := podResolvConf
	if len(options) > 0 {
		content += "options " + strings.Join(options, " ") + "\n"
	}
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := getSystemDNSConfig()
	config := dnsReadConfig(path)
	systemResolvConf.dnsConfig.Store((*byte)(config))
	if getSystemDNSConfig() != config {
		t.Fatal("the resolver did not take the test's configuration")
	}
	preferGo, dial := net.DefaultResolver.PreferGo, net.DefaultResolver.Dial
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, server)
	}
	t.Cleanup(func() {
		net.DefaultResolver.PreferGo, net.DefaultResolver.Dial = preferGo, dial
		systemResolvConf.dnsConfig.Store((*byte)(previous))
	})
}

// query is one question the fake server received.
type query struct {
	name, network string
	qtype         dnsmessage.Type
	dropped       bool
}

// fakeDNS answers like CoreDNS: names under cluster.local from its cluster
// zone, any other name as the upstream resolver it forwards to would. It drops
// a query when drop says so, as a congested node can drop a datagram.
type fakeDNS struct {
	addr    string
	cluster map[string][4]byte // FQDN under cluster.local.
	public  map[string][4]byte // any other FQDN
	drop    func(name string, qtype dnsmessage.Type, seen int) bool

	mu   sync.Mutex
	log  []query
	seen map[string]int
}

func newFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	loopback := [4]byte{127, 0, 0, 1}
	f := &fakeDNS{
		cluster: map[string][4]byte{
			"claude-fixture.sproozi-fixture.svc.cluster.local.": loopback,
			"kubernetes.default.svc.cluster.local.":             loopback,
			"sproozi-gateway.sproozi-system.svc.cluster.local.": loopback,
			// Service models in namespace ai.
			"models.ai.svc.cluster.local.": loopback,
		},
		public: map[string][4]byte{
			"api.anthropic.com.": loopback,
			// A public domain with the same spelling as models.ai above.
			"models.ai.": {127, 0, 0, 2},
		},
		seen: map[string]int{},
	}
	packets, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = packets.LocalAddr().String()
	stream, err := net.Listen("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = packets.Close(); _ = stream.Close() })
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, peer, err := packets.ReadFrom(buffer)
			if err != nil {
				return
			}
			if reply := f.reply(buffer[:n], "udp"); reply != nil {
				_, _ = packets.WriteTo(reply, peer)
			}
		}
	}()
	go func() {
		for {
			conn, err := stream.Accept()
			if err != nil {
				return
			}
			go f.serveStream(conn)
		}
	}()
	return f
}

func (f *fakeDNS) serveStream(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return
	}
	message := make([]byte, int(size[0])<<8|int(size[1]))
	if _, err := io.ReadFull(conn, message); err != nil {
		return
	}
	reply := f.reply(message, "tcp")
	if reply == nil {
		// A dropped query over TCP: hold the connection without an answer.
		time.Sleep(15 * time.Second)
		return
	}
	_, _ = conn.Write(append([]byte{byte(len(reply) >> 8), byte(len(reply))}, reply...))
}

func (f *fakeDNS) reply(message []byte, network string) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(message)
	if err != nil {
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		return nil
	}
	name := strings.ToLower(question.Name.String())
	f.mu.Lock()
	key := name + "/" + question.Type.String()
	f.seen[key]++
	dropped := f.drop != nil && f.drop(name, question.Type, f.seen[key])
	f.log = append(f.log, query{name: name, network: network, qtype: question.Type, dropped: dropped})
	f.mu.Unlock()
	if dropped {
		return nil
	}
	zone := f.public
	if strings.HasSuffix(name, ".cluster.local.") {
		zone = f.cluster
	}
	address, found := zone[name]
	reply := dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true, RecursionAvailable: true}
	if !found {
		reply.RCode = dnsmessage.RCodeNameError
	}
	builder := dnsmessage.NewBuilder(nil, reply)
	if builder.StartQuestions() != nil || builder.Question(question) != nil || builder.StartAnswers() != nil {
		return nil
	}
	if found && question.Type == dnsmessage.TypeA {
		if builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 30},
			dnsmessage.AResource{A: address}) != nil {
			return nil
		}
	}
	out, err := builder.Finish()
	if err != nil {
		return nil
	}
	return out
}

// queries returns the questions received so far, in order.
func (f *fakeDNS) queries() []query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// leaked lists the names asked outside cluster.local, which CoreDNS forwards
// to the node's upstream resolver.
func leaked(queries []query) []string {
	var names []string
	for _, q := range queries {
		if !strings.HasSuffix(q.name, ".cluster.local.") && !slices.Contains(names, q.name) {
			names = append(names, q.name)
		}
	}
	return names
}

// askedNames lists the distinct names asked, in order.
func askedNames(queries []query) []string {
	var names []string
	for _, q := range queries {
		if !slices.Contains(names, q.name) {
			names = append(names, q.name)
		}
	}
	return names
}

// dialStall sends one request with the gateway's model client to the fixture
// provider's in-cluster name and returns how long it took to arrive there.
func dialStall(t *testing.T, server *fakeDNS) (time.Duration, error) {
	t.Helper()
	arrived := make(chan time.Time, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- time.Now()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := newModelClient()
	t.Cleanup(client.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://claude-fixture.sproozi-fixture.svc:"+port+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return time.Since(began), err
	}
	_ = response.Body.Close()
	return (<-arrived).Sub(began), nil
}

// dropFirst drops the first `times` queries for name and qtype.
func dropFirst(name string, qtype dnsmessage.Type, times int) func(string, dnsmessage.Type, int) bool {
	return func(n string, q dnsmessage.Type, seen int) bool { return n == name && q == qtype && seen <= times }
}

const (
	firstCandidate = "claude-fixture.sproozi-fixture.svc.sproozi-system.svc.cluster.local."
	hitCandidate   = "claude-fixture.sproozi-fixture.svc.cluster.local."
)

// TestDNSLostDatagramStallPerResolverSetting measures the model client's
// dial to claude-fixture.sproozi-fixture.svc with each candidate resolver
// setting and each lost query. Each case states the stall Go's resolver
// gives (timeout per lost query, attempts per query) and whether the call
// fails. Losses are named by query, so a lost reply counts the same.
func TestDNSLostDatagramStallPerResolverSetting(t *testing.T) {
	type want struct {
		seconds float64
		fails   bool
	}
	losses := []struct {
		name string
		drop func(string, dnsmessage.Type, int) bool
	}{
		{"none", nil},
		{"first A of the first search name", dropFirst(firstCandidate, dnsmessage.TypeA, 1)},
		{"first AAAA of the name that answers", dropFirst(hitCandidate, dnsmessage.TypeAAAA, 1)},
		{"first two A of the name that answers", dropFirst(hitCandidate, dnsmessage.TypeA, 2)},
	}
	settings := []struct {
		name    string
		options []string
		want    []want // one per loss, in order
	}{
		// Two losses of one query use up attempts:2; the lookup then asks the
		// unsuffixed name, which CoreDNS forwards upstream, and fails with
		// "no such host".
		{"pod default (ndots:5, timeout 5, attempts 2)", nil, []want{{0, false}, {5, false}, {5, false}, {10, true}}},
		{"timeout:1 (the cluster trial)", []string{"timeout:1"}, []want{{0, false}, {1, false}, {1, false}, {2, true}}},
		{"timeout:1 attempts:3 (proposed)", []string{"timeout:1", "attempts:3"},
			[]want{{0, false}, {1, false}, {1, false}, {2, false}}},
		// Go reads single-request (and single-request-reopen) as "ask A, then
		// AAAA": a lost query still costs the whole timeout.
		{"single-request", []string{"single-request"}, []want{{0, false}, {5, false}, {5, false}, {10, true}}},
		// A lower ndots changes the names asked, not what a loss costs.
		{"ndots:2 timeout:1 attempts:3", []string{"ndots:2", "timeout:1", "attempts:3"},
			[]want{{0, false}, {1, false}, {1, false}, {2, false}}},
	}
	for _, setting := range settings {
		for i, loss := range losses {
			t.Run(setting.name+"/"+loss.name, func(t *testing.T) {
				server := newFakeDNS(t)
				server.drop = loss.drop
				useResolvConf(t, server.addr, setting.options...)
				took, err := dialStall(t, server)
				queries := server.queries()
				t.Logf("RESULT setting=%q loss=%q stall=%.3fs err=%v queries=%d asked=%v leaked=%v",
					setting.name, loss.name, took.Seconds(), err, len(queries), askedNames(queries), leaked(queries))
				expected := setting.want[i]
				if (err != nil) != expected.fails {
					t.Errorf("call error %v, want failure %v", err, expected.fails)
				}
				if got := took.Seconds(); got < expected.seconds || got > expected.seconds+0.3 {
					t.Errorf("stall %.3fs, want %.0fs", got, expected.seconds)
				}
			})
		}
	}
}

// TestDNSUseVCAsksOverTCP shows that Go honours use-vc: every query goes over
// TCP, one new connection per query. A lost segment is then retransmitted by
// the kernel instead of waiting out the resolver's timeout.
func TestDNSUseVCAsksOverTCP(t *testing.T) {
	server := newFakeDNS(t)
	useResolvConf(t, server.addr, "use-vc")
	if _, err := dialStall(t, server); err != nil {
		t.Fatal(err)
	}
	queries := server.queries()
	for _, q := range queries {
		if q.network != "tcp" {
			t.Fatalf("use-vc asked %s %s over %s", q.name, q.qtype, q.network)
		}
	}
	t.Logf("RESULT use-vc queries=%d all over tcp", len(queries))
}

// TestDNSNamesResolveAlikeWithProposedOptions checks that the proposed
// options change neither which names are asked nor what they resolve to,
// and shows what a lower ndots would change.
func TestDNSNamesResolveAlikeWithProposedOptions(t *testing.T) {
	names := []string{"claude-fixture.sproozi-fixture.svc", "api.anthropic.com", "kubernetes.default.svc",
		"sproozi-gateway.sproozi-system.svc", "models.ai"}
	type outcome struct {
		address string
		asked   []string
	}
	resolve := func(t *testing.T, options []string, name string) outcome {
		server := newFakeDNS(t)
		useResolvConf(t, server.addr, options...)
		addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), name)
		out := outcome{asked: askedNames(server.queries())}
		if err != nil {
			out.address = "error: " + err.Error()
		} else {
			out.address = addrs[0].IP.String()
		}
		t.Logf("RESULT options=%v name=%s address=%s asked=%v leaked=%v", options, name, out.address, out.asked,
			leaked(server.queries()))
		return out
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			before := resolve(t, nil, name)
			after := resolve(t, []string{"timeout:1", "attempts:3"}, name)
			if fmt.Sprint(before) != fmt.Sprint(after) {
				t.Errorf("timeout:1 attempts:3 changed %s: %+v, was %+v", name, after, before)
			}
			for _, ndots := range []string{"ndots:2", "ndots:1"} {
				lower := resolve(t, []string{ndots}, name)
				if lower.address != before.address {
					t.Logf("CHANGED %s makes %s resolve to %s instead of %s", ndots, name, lower.address,
						before.address)
				}
			}
		})
	}
}

// lostDatagramBound is what one lost DNS datagram may add to a model call's
// dial once the gateway no longer waits out the resolver's whole timeout.
const lostDatagramBound = 1500 * time.Millisecond

// TestDNSLostDatagramDoesNotStallModelDial is red until the gateway itself
// bounds a lookup that one lost datagram holds: with the Pod's default
// resolver settings the dial waits the resolver's whole five-second timeout.
// The install's resolver options do not make it pass; only a change to how
// the gateway resolves or keeps connections does.
func TestDNSLostDatagramDoesNotStallModelDial(t *testing.T) {
	server := newFakeDNS(t)
	server.drop = dropFirst(hitCandidate, dnsmessage.TypeA, 1)
	useResolvConf(t, server.addr)
	took, err := dialStall(t, server)
	if err != nil {
		t.Fatalf("model call after one lost DNS datagram: %v", err)
	}
	if took > lostDatagramBound {
		t.Fatalf("one lost DNS datagram held the model call's dial for %s, more than %s", took, lostDatagramBound)
	}
}
