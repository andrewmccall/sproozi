package model

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// In test builds every model call checks that end, settle and mark run on
// the goroutine that created it (end.go).
func init() {
	newOwnerCheck = func() func() {
		owner := goroutineID()
		return func() {
			if id := goroutineID(); id != owner {
				panic(fmt.Sprintf("model call owned by goroutine %d was ended on goroutine %d", owner, id))
			}
		}
	}
}

// goroutineID reads the current goroutine's number from its stack header,
// "goroutine N [running]:".
func goroutineID() uint64 {
	var buf [64]byte
	header := buf[:runtime.Stack(buf[:], false)]
	header = bytes.TrimPrefix(header, []byte("goroutine "))
	id, err := strconv.ParseUint(string(header[:bytes.IndexByte(header, ' ')]), 10, 64)
	if err != nil {
		panic(err)
	}
	return id
}

// Two goroutines that are not the call's owner end it at once. The owner
// check stops both before either touches the call.
func TestEndRunsOnlyOnTheCallsOwner(t *testing.T) {
	for range 50 {
		c := newEndTestCall(t, callRelaying, callLive)
		var wg sync.WaitGroup
		panics := make([]string, 2)
		for i := range 2 {
			wg.Go(func() {
				defer func() { panics[i] = fmt.Sprint(recover()) }()
				c.call.end(ending{kind: endingInterrupted, reason: endClientCancelled})
			})
		}
		wg.Wait()
		for _, p := range panics {
			if !strings.Contains(p, "was ended on goroutine") {
				t.Fatalf("end ran off its owner without the check firing: %q", p)
			}
		}
		if used, reserved := c.used(t); len(c.events.events) != 0 || used != 0 || reserved != 1000 ||
			c.call.phase != callRelaying {
			t.Fatalf("a refused owner changed the call: events=%d used=%d reserved=%d phase=%s",
				len(c.events.events), used, reserved, c.call.phase)
		}
		c.call.end(ending{kind: endingInterrupted, reason: endClientCancelled})
		if used, _ := c.used(t); len(c.events.events) != 1 || used != 10 {
			t.Fatalf("the owner's end: events=%d used=%d", len(c.events.events), used)
		}
	}
}
