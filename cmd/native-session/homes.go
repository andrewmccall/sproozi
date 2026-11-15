package main

import "github.com/andrewmccall/sproozi/internal/harness"

// Each managed harness's native home and stock executable come from the
// registry (internal/harness/registry.go), not from constants here.
var (
	nativeHome        = harness.NativeHomePath(harness.NativeCodex)
	executable        = harness.NativeExecutable(harness.NativeCodex)
	claudeHome        = harness.NativeHomePath(harness.NativeClaude)
	claudeExecutable  = harness.NativeExecutable(harness.NativeClaude)
	copilotHome       = harness.NativeHomePath(harness.NativeCopilot)
	copilotExecutable = harness.NativeExecutable(harness.NativeCopilot)
)
