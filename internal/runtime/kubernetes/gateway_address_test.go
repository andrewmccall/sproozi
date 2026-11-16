package kubernetes

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// testGatewayAddress is the address the fake resolver answers with.
var testGatewayAddress = [4]byte{10, 96, 0, 42}

// lossyResolver is a resolver whose DNS server drops the first `lost`
// queries it receives, as a cluster network can drop a datagram, and
// answers every later one with testGatewayAddress after delay.
func lossyResolver(t *testing.T, lost int, delay time.Duration) *net.Resolver {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var mu sync.Mutex
	go func() {
		buffer := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			mu.Lock()
			drop := lost > 0
			lost--
			mu.Unlock()
			if drop {
				continue
			}
			if reply := answer(buffer[:n]); reply != nil {
				time.AfterFunc(delay, func() { _, _ = conn.WriteTo(reply, peer) })
			}
		}
	}()
	server := conn.LocalAddr().String()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", server)
	}}
}

// answer replies to an A query with testGatewayAddress and to any other
// query with no records.
func answer(query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		return nil
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true})
	if builder.StartQuestions() != nil || builder.Question(question) != nil || builder.StartAnswers() != nil {
		return nil
	}
	if question.Type == dnsmessage.TypeA {
		if builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 30},
			dnsmessage.AResource{A: testGatewayAddress}) != nil {
			return nil
		}
	}
	reply, err := builder.Finish()
	if err != nil {
		return nil
	}
	return reply
}

// lostReplyBound is how long resolving the gateway may take when one DNS
// datagram is lost. The controller has one reconciler, so every AgentRun
// waits as long as this lookup does.
const lostReplyBound = 2 * time.Second

// A lost DNS datagram must not hold the lookup for the resolver's whole
// timeout: the lookup is asked again at once.
func TestLookupGatewayAsksAgainAfterALostReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	began := time.Now()
	address, err := lookupGateway(ctx, lossyResolver(t, 1, 0), "sproozi-gateway.sproozi-system.svc.")
	took := time.Since(began)
	if err != nil || address != "10.96.0.42" {
		t.Fatalf("lookup: %q, %v", address, err)
	}
	if took > lostReplyBound {
		t.Fatalf("a lost DNS reply held the gateway lookup for %s", took)
	}
}

// A resolver slower than the first lookup's bound still answers: the lookup
// asked again waits with the resolver's own bounds.
func TestLookupGatewayWaitsForASlowResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	address, err := lookupGateway(ctx, lossyResolver(t, 0, gatewayLookupTry+500*time.Millisecond),
		"sproozi-gateway.sproozi-system.svc.")
	if err != nil || address != "10.96.0.42" {
		t.Fatalf("lookup: %q, %v", address, err)
	}
}
