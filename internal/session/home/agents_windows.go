//go:build windows

package home

import (
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"os"
)

// ownedEntry applies the private host-file policy on Windows, where ACLs
// rather than mode bits express ownership.
func ownedEntry(path string, directory bool) error { return privatefs.Check(path, directory) }

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
