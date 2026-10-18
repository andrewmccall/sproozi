package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/privatefs"
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
	if err := privatefs.EnsureDir(dir); err != nil {
		return nil, err
	}
	lock, err := privatefs.Lock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return nil, fmt.Errorf("docker gateway: registry already owned: %w", err)
	}
	return &registry{dir: dir, lock: lock}, nil
}

func (s *registry) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return privatefs.UnlockClose(s.lock)
}

func (s *registry) read(id string) (record, error) {
	if s.closed || !identifier.MatchString(id) {
		return record{}, errors.New("docker gateway: unavailable registry")
	}
	path := filepath.Join(s.dir, id+".json")
	data, err := privatefs.ReadFile(path, maxRecordBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return record{}, errRecordMissing
		}
		return record{}, err
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
	return privatefs.Replace(s.dir, r.Launch.AllocationID+".json", data)
}
