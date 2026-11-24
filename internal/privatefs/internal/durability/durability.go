// Package durability holds the switch that lets a test binary skip the
// syncs privatefs makes. Go's internal rule lets only internal/privatefs and
// the packages below it import it: privatefs reads the switch, and
// privatefstest, which refuses to run outside a test binary, sets it.
package durability

import "sync/atomic"

var skipped atomic.Bool

// Skipped reports whether privatefs skips its file and directory syncs. It
// is false unless privatefstest.SkipSync was called in this process.
func Skipped() bool { return skipped.Load() }

// Skip makes Skipped report true for the rest of the process. Only
// privatefstest.SkipSync calls it.
func Skip() { skipped.Store(true) }
