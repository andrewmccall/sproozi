package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxRecordBytes = 256 << 10

var errRecordMissing = errors.New("docker gateway: allocation record missing")

type registry struct {
	dir    string
	lock   *os.File
	closed bool
}

func openRegistry(dir string) (*registry, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("docker gateway: absolute private registry required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := privatePath(dir, true); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "owner.lock")
	if _, err := os.Lstat(path); err == nil {
		if err := privatePath(path, false); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("docker gateway: registry already owned")
	}
	return &registry{dir: dir, lock: lock}, nil
}

func privatePath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("docker gateway: registry paths must be owned and private")
	}
	return nil
}

func (s *registry) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
}

func (s *registry) read(id string) (record, error) {
	if s.closed || !identifier.MatchString(id) {
		return record{}, errors.New("docker gateway: unavailable registry")
	}
	path := filepath.Join(s.dir, id+".json")
	if err := privatePath(path, false); err != nil {
		if os.IsNotExist(err) {
			return record{}, errRecordMissing
		}
		return record{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return record{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return record{}, err
	}
	if len(data) > maxRecordBytes {
		return record{}, errors.New("docker gateway: allocation record exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var r record
	if err = d.Decode(&r); err != nil {
		return record{}, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return record{}, errors.New("docker gateway: trailing allocation data")
	}
	return r, nil
}

func (s *registry) save(r record) error {
	if s.closed || !identifier.MatchString(r.Launch.AllocationID) {
		return errors.New("docker gateway: unavailable registry")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return errors.New("docker gateway: allocation record exceeds bound")
	}
	temp, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), filepath.Join(s.dir, r.Launch.AllocationID+".json")); err != nil {
		return err
	}
	parent, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
