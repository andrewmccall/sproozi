package privatefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Usage sums the sizes, as lstat reports them, of every entry under root
// that is not a directory and that skip does not name. It examines nothing
// but sizes, so an owner can bound what it keeps while others write.
//
// The sum is of what each directory's listing found, each at the size lstat
// reports when it is examined. A Replace in progress
// counts its temporary file, beside the file it replaces, which only
// overstates what is kept. An entry the listing found that is gone when it
// is examined is not counted: it was removed, or it was a Replace's
// temporary file renamed over its target, a write after the listing like
// any other. The only rename in a directory that Replace writes is that one,
// from a temporary file to its target, so no kept file is skipped under one
// name and missed under another.
//
// That holds because filepath.WalkDir lists each directory with os.ReadDir,
// which sorts by filename, and a temporary name (.pending-...) sorts before
// every name kept today: UUID-named records and journals, hex digests and
// owner.lock. The temporary file is therefore examined before its target, so
// if it is gone the target, examined later, has its new size. A kept name
// that sorts before ".pending-" would be examined first under its old size
// and break the argument. A scratch directory that goes with its
// contents is skipped the same way. Every other error fails the sum,
// including a missing root, a root that is not a directory by lstat (a
// symbolic link or a regular file) and a directory that cannot be listed.
func Usage(root string, skip func(name string) bool) (int64, error) {
	return usage(root, skip, nil)
}

// usage is Usage with listed, when not nil, called with each entry's path
// after its directory was listed and before the entry is examined, where a
// concurrent writer may remove or rename it.
func usage(root string, skip func(name string) bool, listed func(path string)) (int64, error) {
	// The walk does not follow a symbolic link, so a root that is one would
	// sum as the link alone: a small sum for a whole tree, which for an
	// admission decision is the unsafe direction. Refuse it.
	info, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("privatefs: usage root %s is not a directory", root)
	}
	var size int64
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil {
			if listed != nil {
				listed(path)
			}
			if d.IsDir() || skip != nil && skip(d.Name()) {
				return nil
			}
			var info fs.FileInfo
			if info, err = d.Info(); err == nil {
				size += info.Size()
			}
		}
		if path != root && errors.Is(err, fs.ErrNotExist) {
			// Listed, then removed or renamed before it was examined.
			return nil
		}
		return err
	})
	return size, err
}

// gone reports whether path, which an operation just failed on with an error
// that is fs.ErrNotExist, is itself gone, rather than unreachable because a
// directory above it became something else. On Unix the second case fails with
// ENOTDIR and never reaches here. On Windows syscall.ENOTDIR is
// ERROR_PATH_NOT_FOUND, which is also what a path below a file reports, so the
// error alone cannot tell them apart: a directory replaced by a file, or a
// parent replaced by one, would look like a removal and be skipped, an
// undercount. gone looks: path is gone only if it does not exist and the
// nearest ancestor that does is a directory (following links, as the failed
// operation did). Anything else, including an error other than not-exist, is
// not gone and the caller fails the sum.
func gone(path string) bool {
	if _, err := os.Lstat(path); err == nil {
		return false
	}
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil {
			return info.IsDir()
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}
