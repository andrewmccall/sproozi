//go:build windows

package agentprofile

import "github.com/andrewmccall/sproozi/internal/privatefs"

// ownedEntry applies the private host-file policy on Windows, where ACLs
// rather than mode bits express ownership.
func ownedEntry(path string, directory bool) error { return privatefs.Check(path, directory) }
