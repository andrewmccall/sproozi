package session

import (
	"os"
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs/privatefstest"
)

func TestMain(m *testing.M) {
	// The tests write host-private files through privatefs; no test can
	// observe a sync, and on macOS each one waits on every process's writes.
	privatefstest.SkipSync()
	os.Exit(m.Run())
}
