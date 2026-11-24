//go:build dnsstall

//go:debug netdns=go

// These tests measure what lost or slow DNS answers cost the gateway's model
// client, through Go's own resolver with a resolv.conf chosen by the test.
// Go reads only /etc/resolv.conf, so the test installs its configuration
// through the net package's internals, which the linker refuses without a
// flag. They therefore run only on request (make verify-dns-stall):
//
//	GOTOOLCHAIN=go1.26.11 go test -tags dnsstall -ldflags=-checklinkname=0 \
//	  -run '^TestDNS' -count=1 -v ./cmd/gateway/
//
// How a lookup reaches the fake server: the test writes the fake's loopback
// address into the process's resolver configuration as its only nameserver,
// and go:debug netdns=go makes every resolver in the binary Go's own, as it
// is in the gateway image (CGO_ENABLED=0 on Linux). Any net.Resolver
// therefore asks the fake, net.DefaultResolver or a new one, as long as it
// has no Dial of its own, or its Dial connects to the address it is given.
// A fix to how the gateway resolves must keep to that for these tests to
// see it. The fake binds 127.0.0.1 on an ephemeral port; the machine's own
// resolver configuration is never read by a lookup here, nor changed.

package main

import (
	"context"
	"errors"
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

// knownToolchain is the Go release whose net internals the mirror below
// matches. On any other toolchain the suite fails rather than reporting
// nothing: check src/net/dnsclient_unix.go (resolverConfig) and
// src/net/dnsconfig.go (dnsConfig, whose first field is servers), then
// update this prefix.
const knownToolchain = "go1.26."

// resolverConfigMirror has the layout of net.resolverConfig. The check in
// useResolvConf that the resolver returns the installed configuration, and
// that a lookup reaches the fake through its search list, is what catches a
// layout change.
type resolverConfigMirror struct {
	_         sync.Once
	_         chan struct{}
	_         time.Time
	dnsConfig atomic.Pointer[byte]
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

// The install's options (config/manager/gateway.yaml) and the others tried.
const (
	timeoutOne    = "timeout:1"
	attemptsThree = "attempts:3"
)

var installOptions = []string{timeoutOne, attemptsThree}

// useResolvConf makes Go's resolver use podResolvConf with extra options and
// the fake as its only nameserver.
func useResolvConf(t *testing.T, server *fakeDNS, options ...string) {
	t.Helper()
	if !strings.HasPrefix(runtime.Version(), knownToolchain) {
		t.Fatalf("the dnsstall suite mirrors net internals of %s*, not %s: check them and update knownToolchain",
			knownToolchain, runtime.Version())
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
	*(*[]string)(config) = []string{server.addr} // dnsConfig.servers
	systemResolvConf.dnsConfig.Store((*byte)(config))
	t.Cleanup(func() { systemResolvConf.dnsConfig.Store((*byte)(previous)) })
	if getSystemDNSConfig() != config {
		t.Fatal("the resolver did not take the test's configuration")
	}
	// A fresh resolver must reach the fake through the search list.
	_, _ = (&net.Resolver{}).LookupIPAddr(context.Background(), "layout-check.invalid")
	if !slices.Contains(askedNames(server.queries()), "layout-check.invalid.sproozi-system.svc.cluster.local.") {
		t.Fatalf("a lookup did not reach the fake through the test's search list (asked %v): "+
			"the net internals changed", askedNames(server.queries()))
	}
	server.reset()
}

// query is one question the fake server received.
type query struct {
	name, network string
	qtype         dnsmessage.Type
	dropped       bool
}

// fakeDNS answers like CoreDNS without a cache: names under cluster.local
// from its cluster zone, any other name as the upstream resolver it forwards
// to would. It drops a query when drop says so, as a congested node can drop
// a datagram, and answers after delay.
type fakeDNS struct {
	addr    string
	cluster map[string][4]byte // FQDN under cluster.local.
	public  map[string][4]byte // any other FQDN
	drop    func(name string, qtype dnsmessage.Type, seen int) bool
	delay   func(name string) time.Duration

	mu   sync.Mutex
	log  []query
	seen map[string]int
}

func newFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	loopback := [4]byte{127, 0, 0, 1}
	f := &fakeDNS{
		cluster: map[string][4]byte{
			hitCandidate:                            loopback,
			"kubernetes.default.svc.cluster.local.": loopback,
			defaultHost + ".cluster.local.":         loopback,
			// Service models in namespace ai.
			"models.ai.svc.cluster.local.": loopback,
		},
		public: map[string][4]byte{
			anthropicHost + ".": loopback,
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
			if reply, wait := f.reply(buffer[:n], "udp"); reply != nil {
				time.AfterFunc(wait, func() { _, _ = packets.WriteTo(reply, peer) })
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
	reply, wait := f.reply(message, "tcp")
	if reply == nil {
		return
	}
	time.Sleep(wait)
	_, _ = conn.Write(append([]byte{byte(len(reply) >> 8), byte(len(reply))}, reply...))
}

// reply builds the answer to message, or nil to drop it, and how long to
// wait before sending it.
func (f *fakeDNS) reply(message []byte, network string) ([]byte, time.Duration) {
	var parser dnsmessage.Parser
	header, err := parser.Start(message)
	if err != nil {
		return nil, 0
	}
	question, err := parser.Question()
	if err != nil {
		return nil, 0
	}
	name := strings.ToLower(question.Name.String())
	f.mu.Lock()
	key := name + "/" + question.Type.String()
	f.seen[key]++
	dropped := f.drop != nil && f.drop(name, question.Type, f.seen[key])
	f.log = append(f.log, query{name: name, network: network, qtype: question.Type, dropped: dropped})
	f.mu.Unlock()
	if dropped {
		return nil, 0
	}
	var wait time.Duration
	if f.delay != nil {
		wait = f.delay(name)
	}
	zone := f.public
	if strings.HasSuffix(name, ".cluster.local.") {
		zone = f.cluster
	}
	address, found := zone[name]
	header = dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true, RecursionAvailable: true}
	if !found {
		header.RCode = dnsmessage.RCodeNameError
	}
	builder := dnsmessage.NewBuilder(nil, header)
	if builder.StartQuestions() != nil || builder.Question(question) != nil || builder.StartAnswers() != nil {
		return nil, 0
	}
	if found && question.Type == dnsmessage.TypeA {
		if builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 30},
			dnsmessage.AResource{A: address}) != nil {
			return nil, 0
		}
	}
	out, err := builder.Finish()
	if err != nil {
		return nil, 0
	}
	return out, wait
}

// queries returns the questions received so far, in order.
func (f *fakeDNS) queries() []query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// reset forgets the questions received so far.
func (f *fakeDNS) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log, f.seen = nil, map[string]int{}
}

// count is how many times name was asked for qtype.
func count(queries []query, name string, qtype dnsmessage.Type) int {
	n := 0
	for _, q := range queries {
		if q.name == name && q.qtype == qtype {
			n++
		}
	}
	return n
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

// call is one model request's outcome.
type call struct {
	took time.Duration
	err  error
}

// callConcurrently sends calls requests at once with one gateway model
// client to host, served by one loopback upstream, and returns how long each
// took. The measured stalls happened while several sandboxes called the same
// provider; Go merges concurrent lookups of one host on one resolver.
func callConcurrently(t *testing.T, host string, calls int) []call {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := newModelClient()
	t.Cleanup(client.CloseIdleConnections)
	results := make([]call, calls)
	var wg sync.WaitGroup
	began := time.Now()
	for i := range results {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost,
				"http://"+net.JoinHostPort(host, port)+"/v1/messages", strings.NewReader("{}"))
			if err == nil {
				var response *http.Response
				if response, err = client.Do(request); err == nil {
					_ = response.Body.Close()
				}
			}
			results[i] = call{took: time.Since(began), err: err}
		})
	}
	wg.Wait()
	return results
}

// dropFirst drops the first `times` queries for name and qtype.
func dropFirst(name string, qtype dnsmessage.Type, times int) func(string, dnsmessage.Type, int) bool {
	return func(n string, q dnsmessage.Type, seen int) bool { return n == name && q == qtype && seen <= times }
}

const (
	fixtureHost    = "claude-fixture.sproozi-fixture.svc"
	kubernetesHost = "kubernetes.default.svc"
	firstCandidate = fixtureHost + ".sproozi-system.svc.cluster.local."
	hitCandidate   = fixtureHost + ".cluster.local."
	// concurrentCalls is how many calls each case makes to the same host.
	concurrentCalls = 2
)

// within reports whether d is within 300 ms above seconds.
func within(d time.Duration, seconds float64) bool {
	return d.Seconds() >= seconds && d.Seconds() <= seconds+0.3
}

// TestDNSLostDatagramStallPerResolverSetting measures two concurrent model
// calls to claude-fixture.sproozi-fixture.svc with each candidate resolver
// setting and each lost query. Each case states the stall Go's resolver gives
// every call (one timeout per lost query, attempts per query) and whether the
// calls fail. Losses are named by query; a lost reply costs the same.
func TestDNSLostDatagramStallPerResolverSetting(t *testing.T) {
	type want struct {
		seconds float64
		fails   bool
	}
	losses := []struct {
		name string
		drop func(string, dnsmessage.Type, int) bool
		// hitA is how many A queries for the answering name the two calls
		// send together: one lookup, merged, plus one per loss.
		hitA int
	}{
		{"none", nil, 1},
		{"first A of the first search name", dropFirst(firstCandidate, dnsmessage.TypeA, 1), 1},
		{"first AAAA of the name that answers", dropFirst(hitCandidate, dnsmessage.TypeAAAA, 1), 1},
		{"first two A of the name that answers", dropFirst(hitCandidate, dnsmessage.TypeA, 2), 3},
	}
	settings := []struct {
		name    string
		options []string
		want    []want // one per loss, in order
	}{
		// Two losses of one query use up attempts:2; see
		// TestDNSExhaustedAttemptsSurfaceAsNotFound for what follows.
		{"pod default (ndots:5, timeout 5, attempts 2)", nil, []want{{0, false}, {5, false}, {5, false}, {10, true}}},
		{"timeout:1 (the cluster trial)", []string{timeoutOne}, []want{{0, false}, {1, false}, {1, false}, {2, true}}},
		{"timeout:1 attempts:3 (the install)", installOptions, []want{{0, false}, {1, false}, {1, false}, {2, false}}},
		// Go reads single-request (and single-request-reopen) as "ask A, then
		// AAAA": a lost query still costs the whole timeout.
		{"single-request", []string{"single-request"}, []want{{0, false}, {5, false}, {5, false}, {10, true}}},
		// A lower ndots changes the names asked, not what a loss costs.
		{"ndots:2 timeout:1 attempts:3", append([]string{"ndots:2"}, installOptions...),
			[]want{{0, false}, {1, false}, {1, false}, {2, false}}},
	}
	for _, setting := range settings {
		for i, loss := range losses {
			t.Run(setting.name+"/"+loss.name, func(t *testing.T) {
				server := newFakeDNS(t)
				useResolvConf(t, server, setting.options...)
				server.drop = loss.drop
				calls := callConcurrently(t, fixtureHost, concurrentCalls)
				queries := server.queries()
				t.Logf("RESULT setting=%q loss=%q calls=%v queries=%d asked=%v leaked=%v",
					setting.name, loss.name, calls, len(queries), askedNames(queries), leaked(queries))
				expected := setting.want[i]
				for _, c := range calls {
					if (c.err != nil) != expected.fails {
						t.Errorf("call error %v, want failure %v", c.err, expected.fails)
					}
					if !within(c.took, expected.seconds) {
						t.Errorf("call took %s, want %.0fs", c.took, expected.seconds)
					}
				}
				hitA := loss.hitA
				if expected.fails {
					hitA = 2 // attempts:2, both lost, then the bare name
				}
				if got := count(queries, hitCandidate, dnsmessage.TypeA); got != hitA {
					t.Errorf("%d concurrent calls asked %s A %d times, want %d: Go merges their lookups",
						concurrentCalls, hitCandidate, got, hitA)
				}
			})
		}
	}
}

// TestDNSExhaustedAttemptsSurfaceAsNotFound records what happens when every
// attempt for the answering name is lost: the resolver goes on to the bare
// name, which CoreDNS forwards upstream (an internal name leaves the cluster),
// and the call fails with "no such host", not a timeout. A retry that reacts
// only to timeouts would not see it.
func TestDNSExhaustedAttemptsSurfaceAsNotFound(t *testing.T) {
	for _, c := range []struct {
		name    string
		options []string
		lost    int
		seconds float64
	}{
		{"pod default", nil, 2, 10},
		{"install options", installOptions, 3, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newFakeDNS(t)
			useResolvConf(t, server, c.options...)
			server.drop = dropFirst(hitCandidate, dnsmessage.TypeA, c.lost)
			result := callConcurrently(t, fixtureHost, 1)[0]
			queries := server.queries()
			var dnsErr *net.DNSError
			if !errors.As(result.err, &dnsErr) {
				t.Fatalf("call error %v, want a DNS error", result.err)
			}
			t.Logf("RESULT exhausted %s lost=%d took=%s IsNotFound=%v IsTimeout=%v IsTemporary=%v leaked=%v",
				c.name, c.lost, result.took, dnsErr.IsNotFound, dnsErr.IsTimeout, dnsErr.IsTemporary, leaked(queries))
			if !dnsErr.IsNotFound || dnsErr.IsTimeout {
				t.Errorf("exhausted attempts gave IsNotFound=%v IsTimeout=%v, want not found", dnsErr.IsNotFound,
					dnsErr.IsTimeout)
			}
			if !slices.Equal(leaked(queries), []string{fixtureHost + "."}) {
				t.Errorf("asked outside the cluster zone %v, want the bare name", leaked(queries))
			}
			if !within(result.took, c.seconds) {
				t.Errorf("call took %s, want %.0fs", result.took, c.seconds)
			}
		})
	}
}

// TestDNSSlowResolverFailsWithTheInstallOptions records what the install's
// options cost: an attempt is a new query with its own one-second deadline,
// so an answer later than a second is never read. A resolver that steadily
// answers more slowly than that fails lookups that succeeded with the
// default five seconds. This fake has no cache; a real CoreDNS that caches
// a late upstream answer may answer a later attempt in time, which nobody
// has measured.
func TestDNSSlowResolverFailsWithTheInstallOptions(t *testing.T) {
	slow := func(delay time.Duration, upstreamOnly bool) func(string) time.Duration {
		return func(name string) time.Duration {
			if upstreamOnly && strings.HasSuffix(name, ".cluster.local.") {
				return 0
			}
			return delay
		}
	}
	for _, c := range []struct {
		name    string
		host    string
		delay   func(string) time.Duration
		options []string
		seconds float64
		fails   bool
	}{
		{"every name at 1.2 s, pod default", fixtureHost, slow(1200*time.Millisecond, false), nil, 3.6, false},
		{"every name at 1.2 s, install options", fixtureHost, slow(1200*time.Millisecond, false), installOptions, 12, true},
		{"every name at 0.9 s, install options", fixtureHost, slow(900*time.Millisecond, false), installOptions, 2.7, false},
		{"upstream at 1.2 s, pod default", anthropicHost, slow(1200*time.Millisecond, true), nil, 1.2, false},
		{"upstream at 1.2 s, install options", anthropicHost, slow(1200*time.Millisecond, true), installOptions,
			3, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newFakeDNS(t)
			useResolvConf(t, server, c.options...)
			server.delay = c.delay
			result := callConcurrently(t, c.host, 1)[0]
			t.Logf("RESULT slow %q took=%s err=%v", c.name, result.took, result.err)
			if (result.err != nil) != c.fails {
				t.Errorf("call error %v, want failure %v", result.err, c.fails)
			}
			if !within(result.took, c.seconds) {
				t.Errorf("call took %s, want %.1fs", result.took, c.seconds)
			}
		})
	}
}

// TestDNSUseVCAsksOverTCP shows that Go honours use-vc: every query goes over
// TCP, on one new connection per query. What that does to a lost segment is
// not measured here.
func TestDNSUseVCAsksOverTCP(t *testing.T) {
	server := newFakeDNS(t)
	useResolvConf(t, server, "use-vc")
	if result := callConcurrently(t, fixtureHost, 1)[0]; result.err != nil {
		t.Fatal(result.err)
	}
	queries := server.queries()
	for _, q := range queries {
		if q.network != "tcp" {
			t.Fatalf("use-vc asked %s %s over %s", q.name, q.qtype, q.network)
		}
	}
	t.Logf("RESULT use-vc queries=%d all over tcp", len(queries))
}

// TestDNSNamesResolveAlikeWithInstallOptions checks that the install's
// options change neither which names are asked nor what they resolve to,
// and shows what a lower ndots would change.
func TestDNSNamesResolveAlikeWithInstallOptions(t *testing.T) {
	names := []string{fixtureHost, anthropicHost, kubernetesHost, defaultHost, "models.ai"}
	type outcome struct {
		address string
		asked   []string
	}
	resolve := func(t *testing.T, options []string, name string) outcome {
		server := newFakeDNS(t)
		useResolvConf(t, server, options...)
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
			after := resolve(t, installOptions, name)
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
// dial once the gateway itself bounds a lookup.
const lostDatagramBound = 1500 * time.Millisecond

// TestDNSLostDatagramDoesNotStallModelDial protects a cluster where the
// install's resolver options are absent or overridden: with the Pod's
// default settings, one lost datagram holds every concurrent call to the
// provider for the resolver's whole five seconds. With the install's options
// it already passes (about 1.0 s). It is skipped until the owner decides how
// the gateway itself should treat a slow or failed provider lookup, an open
// decision; the change that implements it removes the skip. A retry that asks the same net.Resolver again joins the
// lookup still waiting and does not pass this test.
func TestDNSLostDatagramDoesNotStallModelDial(t *testing.T) {
	t.Skip("the gateway does not yet bound a lookup itself; un-skip with the fix for the owner's open DNS decision")
	server := newFakeDNS(t)
	useResolvConf(t, server)
	server.drop = dropFirst(hitCandidate, dnsmessage.TypeA, 1)
	for _, c := range callConcurrently(t, fixtureHost, concurrentCalls) {
		if c.err != nil {
			t.Fatalf("model call after one lost DNS datagram: %v", c.err)
		}
		if c.took > lostDatagramBound {
			t.Errorf("one lost DNS datagram held a model call's dial for %s, more than %s", c.took, lostDatagramBound)
		}
	}
}
