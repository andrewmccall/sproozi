package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/harness"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/session/internal/lifecycle"
)

const journalVersion = 1
const maxJournalBytes = int64(1 << 20)
const maxOperations = 1000

// Coordinator exclusively owns one protected directory and its placements.
// Close releases the cross-process lock. Placement implementations enforce the
// actual runtime/authority fences; a private Go value alone supplies no fence.
type Coordinator struct {
	dir           string
	placements    map[string]Placement
	maxCheckpoint int64
	lock          *os.File
	mu            sync.RWMutex
	journals      JournalLocks
	graceful      lifecycle.Control
	closed        bool
}

// Open takes a nonblocking owner lock. Existing directories/files must be private
// and owned by this user. Destination names are configured here, never imported
// from checkpoint contents or an untrusted HTTP request's resource options.
func Open(dir string, placements map[string]Placement, maxCheckpointBytes int64) (*Coordinator, error) {
	if maxCheckpointBytes <= 0 || len(placements) == 0 {
		return nil, errors.New("session: checkpoint limit and placements required")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = privatePath(dir, true); err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	configured := make(map[string]Placement, len(placements))
	for name, p := range placements {
		if !validName(name) || p == nil {
			return nil, errors.New("session: invalid placement")
		}
		configured[name] = p
	}
	lockPath := filepath.Join(dir, "owner.lock")
	if _, err = os.Lstat(lockPath); err == nil {
		if err = privatePath(lockPath, false); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("session: coordinator already owned: %w", err)
	}
	c := &Coordinator{dir: dir, placements: configured, maxCheckpoint: maxCheckpointBytes, lock: lock}
	checkpoints := filepath.Join(dir, "checkpoints")
	if err = os.MkdirAll(checkpoints, 0700); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err = privatePath(checkpoints, true); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
func privatePath(p string, directory bool) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("session: path must be owned and private: %s", p)
	}
	return nil
}
func (c *Coordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	unlock := syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
	return errors.Join(unlock, c.lock.Close())
}
func (c *Coordinator) ready() error {
	if c.closed {
		return errors.New("session: coordinator closed")
	}
	return nil
}

func (c *Coordinator) lockSession(id string) (func(), error) {
	if !validUUID(id) {
		return nil, errors.New("session: invalid session ID")
	}
	c.mu.RLock()
	unlock := c.journals.Lock(id)
	if err := c.ready(); err != nil {
		unlock()
		c.mu.RUnlock()
		return nil, err
	}
	return func() { unlock(); c.mu.RUnlock() }, nil
}
func atomicFile(dir, name string, data []byte) error {
	temp, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	nameTemp := temp.Name()
	defer func() { _ = os.Remove(nameTemp) }()
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(nameTemp, filepath.Join(dir, name)); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
func (c *Coordinator) save(r Record) error {
	if err := validateRecord(r); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxJournalBytes {
		return errors.New("session: journal exceeds bound")
	}
	return atomicFile(c.dir, r.ID+".json", data)
}
func readFile(p string, max int64) ([]byte, error) {
	if err := privatePath(p, false); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("session: file exceeds bound")
	}
	return data, nil
}
func (c *Coordinator) load(id string) (Record, error) {
	var r Record
	if !validUUID(id) {
		return r, errors.New("session: invalid session ID")
	}
	data, err := readFile(filepath.Join(c.dir, id+".json"), maxJournalBytes)
	if err != nil {
		return r, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&r); err != nil {
		return r, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return r, errors.New("session: trailing journal data")
	}
	if r.ID != id {
		return r, errors.New("session: journal identity mismatch")
	}
	if err = validateRecord(r); err != nil {
		return r, err
	}
	return r, nil
}
func (c *Coordinator) commitCheckpoint(data []byte, kind harness.NativeKind, thread, digest string) (Checkpoint, error) {
	meta, err := checkpoint.Inspect(data, c.maxCheckpoint)
	if err != nil {
		return Checkpoint{}, err
	}
	if meta.Harness != kind || meta.ThreadID != thread || meta.SHA256 != digest {
		return Checkpoint{}, errors.New("session: checkpoint identity/digest mismatch")
	}
	name := meta.SHA256 + ".json"
	path := filepath.Join("checkpoints", name)
	existing, err := readFile(filepath.Join(c.dir, path), c.maxCheckpoint)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return Checkpoint{}, errors.New("session: checkpoint collision")
		}
	} else if os.IsNotExist(err) {
		if err = atomicFile(filepath.Join(c.dir, "checkpoints"), name, data); err != nil {
			return Checkpoint{}, err
		}
	} else {
		return Checkpoint{}, err
	}
	return Checkpoint{Metadata: meta, Path: path}, nil
}
func (c *Coordinator) checkpointBytes(r Record) ([]byte, error) {
	data, err := readFile(filepath.Join(c.dir, r.Checkpoint.Path), c.maxCheckpoint)
	if err != nil {
		return nil, err
	}
	meta, err := checkpoint.Inspect(data, c.maxCheckpoint)
	if err != nil {
		return nil, err
	}
	if meta != r.Checkpoint.Metadata {
		return nil, errors.New("session: committed checkpoint changed")
	}
	return data, nil
}
