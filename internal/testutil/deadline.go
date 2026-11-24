package testutil

import (
	"testing"
	"time"
)

// overtakenAttempts is how many times PassDeadline starts set-up again with a
// deadline twice as far ahead before it gives up.
const overtakenAttempts = 6

// PassDeadline sets up work with a real deadline and returns once that
// deadline has passed by the machine's clock, which every reader on the path
// under test then sees. setUp builds fresh state whose deadline is ahead of
// now, ahead the first time, and reports that deadline and any error.
//
// Set-up that ends at or after its deadline may have been overtaken by it, so
// it is not trusted, whatever it returned: PassDeadline starts again with the
// deadline twice as far ahead, up to six times. Set-up that fails before its
// deadline fails the test. The assertions after it never depend on the
// machine's speed; a slow machine only makes the test longer.
func PassDeadline(t testing.TB, ahead time.Duration, setUp func(ahead time.Duration) (time.Time, error)) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		until, err := setUp(ahead)
		if time.Now().Before(until) {
			if err != nil {
				t.Fatal(err)
			}
			// The wall clock, which deadlines are judged by, can trail the
			// monotonic clock Sleep uses; 50ms covers it.
			time.Sleep(time.Until(until) + 50*time.Millisecond)
			return
		}
		if attempt == overtakenAttempts {
			t.Fatalf("set-up outlasted a deadline %s ahead %d times (last error: %v)", ahead, attempt, err)
		}
		t.Logf("set-up outlasted a deadline %s ahead; starting again with %s", ahead, 2*ahead)
		ahead *= 2
	}
}
