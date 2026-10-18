// Package privatefs protects host-only state owned by the current user.
// Domain owners retain their record formats, bounds and lock lifetimes. This
// package never repairs permissions on an existing path.
package privatefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ReadFile opens and validates the same file handle before reading bounded data.
func ReadFile(path string, max int64) ([]byte, error) {
	if max <= 0 || max == int64(^uint64(0)>>1) {
		return nil, errors.New("privatefs: invalid read bound")
	}
	f, err := openFile(path, false)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("privatefs: file exceeds bound")
	}
	return data, nil
}

// Replace flushes a private temporary file, then replaces a private regular file
// in the same directory. Successful return includes the platform durability
// operation. It does not establish hardware power-loss guarantees.
func Replace(dir, leaf string, data []byte) error {
	if !validLeaf(leaf) {
		return errors.New("privatefs: ordinary filename required")
	}
	if err := Check(dir, true); err != nil {
		return err
	}
	target := filepath.Join(dir, leaf)
	if err := Check(target, false); err != nil && !os.IsNotExist(err) {
		return err
	}
	var f *os.File
	var err error
	for range 10 {
		name, e := randomName(".pending-")
		if e != nil {
			return e
		}
		f, err = createFile(filepath.Join(dir, name))
		if !os.IsExist(err) {
			break
		}
	}
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() { _ = os.Remove(temp) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return replaceFile(temp, target, dir)
}

// TempDir creates a directory with private permissions at creation. Its parent
// may be the shared OS temporary directory. Existing objects are never reused.
func TempDir(parent, prefix string) (string, error) {
	if parent == "" {
		parent = os.TempDir()
	}
	if strings.ContainsAny(prefix, `/\:`) {
		return "", errors.New("privatefs: invalid temporary prefix")
	}
	for range 10 {
		name, err := randomName(prefix)
		if err != nil {
			return "", err
		}
		path := filepath.Join(parent, name)
		if err = mkdir(path); os.IsExist(err) {
			continue
		} else if err != nil {
			return "", err
		}
		if err = Check(path, true); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return "", errors.New("privatefs: temporary name collision")
}

func randomName(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func validLeaf(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:\x00") && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ")
}

func unsafePath(path string) error {
	return fmt.Errorf("privatefs: path must be owned, private and ordinary: %s", path)
}
