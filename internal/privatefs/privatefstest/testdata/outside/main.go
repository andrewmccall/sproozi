// Command outside calls privatefstest.SkipSync from an ordinary program, as
// a production binary that imported privatefstest would.
// TestSkipSyncPanicsOutsideATestBinary builds and runs it and expects the
// panic. It lives under testdata so that ./... never builds it.
package main

import (
	"fmt"

	"github.com/andrewmccall/sproozi/internal/privatefs/privatefstest"
)

func main() {
	privatefstest.SkipSync()
	fmt.Println("SkipSync returned outside a test binary")
}
