package privatefs

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// Usage sums the sizes, as lstat reports them, of every entry under root
// that is not a directory and that skip does not name. It examines nothing
// but sizes, so an owner can bound what it keeps while others write.
//
// The sum is of what each directory's listing found. A Replace in progress
// counts its temporary file, beside the file it replaces, which only
// overstates what is kept. An entry the listing found that is gone when it
// is examined is not counted: it was removed, or it was a Replace's
// temporary file renamed over its target, a write after the listing like
// any other. The only rename in a directory that Replace writes is that one,
// from a temporary file to its target, so no kept file is skipped under one
// name and missed under another. A scratch directory that goes with its
// contents is skipped the same way. Every other error fails the sum,
// including a missing root and a directory that cannot be listed.
func Usage(root string, skip func(name string) bool) (int64, error) {
	return usage(root, skip, nil)
}

// usage is Usage with listed, when not nil, called with each entry's path
// after its directory was listed and before the entry is examined, where a
// concurrent writer may remove or rename it.
func usage(root string, skip func(name string) bool, listed func(path string)) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
