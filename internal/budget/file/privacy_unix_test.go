//go:build !windows

package file_test

import "os"

func makeTestPathPublic(path string) error { return os.Chmod(path, 0755) }
