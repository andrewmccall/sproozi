package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

type releasingPlacement struct {
	session.Placement
	calls int
	err   error
}

func (p *releasingPlacement) ReleaseStopped(context.Context) (int, error) {
	p.calls++
	return 1, p.err
}

// The service, not a coordinator, retries removals a durable stop left behind.
func TestServiceReleasesStoppedVisitsOfEveryPlacement(t *testing.T) {
	ok, failing := &releasingPlacement{}, &releasingPlacement{err: errors.New("engine unavailable")}
	var log bytes.Buffer
	s := &Service{placements: map[string]session.Placement{codexPlacement: ok, "claude": failing}, log: &log}
	s.releaseStoppedOnce(t.Context())
	if ok.calls != 1 || failing.calls != 1 {
		t.Fatalf("calls = %d, %d; want each placement swept once", ok.calls, failing.calls)
	}
	if !strings.Contains(log.String(), "placement claude: engine unavailable") {
		t.Fatalf("log = %q, want the failure named", log.String())
	}
	(&Service{placements: map[string]session.Placement{codexPlacement: failing}}).releaseStoppedOnce(t.Context())
}

// A control connection over the limit gets a reason, not a bare reset.
func TestRefuseBusyExplainsTheLimit(t *testing.T) {
	server, client := net.Pipe()
	go refuseBusy(server)
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, strconv.Itoa(maxControlConnections)+" requests") {
		t.Fatalf("response = %+v", response)
	}
}

// Cleanup runs while executor discovery adds placements; the race detector
// fails this test if the map is read without the lock.
func TestReleaseStoppedRacesExecutorDiscoverySafely(t *testing.T) {
	s := &Service{placements: map[string]session.Placement{codexPlacement: &releasingPlacement{}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			s.remoteMu.Lock()
			s.placements[fmt.Sprintf("remote-%d", i)] = &releasingPlacement{}
			s.remoteMu.Unlock()
		}
	}()
	for range 50 {
		s.releaseStoppedOnce(t.Context())
	}
	<-done
}

const codexPlacement = "codex"
