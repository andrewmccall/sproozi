// Package privatefstest lets a test binary write host-private files without
// waiting for them to reach the disk.
package privatefstest

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs/internal/durability"
)

// SkipSync makes privatefs skip the file and directory syncs it makes after
// writing, for the rest of the test binary. Every other step is unchanged:
// the name, directory and target checks, the exclusive private temporary
// file and its owner and mode check, and the rename. Call it from TestMain,
// before m.Run, in a package whose tests write many private files. On macOS
// a sync is F_FULLFSYNC, which waits for the whole drive's cache, so while
// other processes write each one can take hundreds of milliseconds. A sync
// matters only across power loss, which no test observes. privatefs's own
// tests do not call it. It panics outside a test binary.
func SkipSync() {
	if !testing.Testing() {
		panic("privatefstest: SkipSync called outside a test binary")
	}
	durability.Skip()
}
