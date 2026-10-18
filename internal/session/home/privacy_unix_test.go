//go:build !windows

package home

import "os"

func makeTestPathPublic(path string) error { return os.Chmod(path, 0755) }
