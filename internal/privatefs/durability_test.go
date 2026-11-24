package privatefs

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs/internal/durability"
)

// privatefs's own tests keep every sync: none of them calls
// privatefstest.SkipSync, so the replacement and SyncDir tests here run the
// real syncing path. A TestMain added here that skipped syncs fails this.
func TestSyncsAreOnUnlessATestSkipsThem(t *testing.T) {
	if durability.Skipped() {
		t.Fatal("privatefs's own tests skip syncs; they must exercise the syncing path")
	}
}
