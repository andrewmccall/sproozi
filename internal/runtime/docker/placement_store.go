package docker

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/session"
)

// recordVersion 2 keeps Launch exactly as prepared and records a first visit's
// bound native thread in Thread. Version 1 records rewrote Launch.ThreadID.
const recordVersion = 2
const maxRecordBytes = int64(1 << 20)

type placementRecord struct {
	Version      int            `json:"version"`
	ConfigDigest string         `json:"configDigest"`
	Launch       session.Launch `json:"launch"`
	// Thread is the genuine native thread a first visit bound; resumed
	// launches name theirs in Launch.ThreadID.
	Thread                string  `json:"thread,omitempty"`
	InputDigest           string  `json:"inputDigest"`
	Execution             string  `json:"execution"`
	Phase                 string  `json:"phase"`
	Volume                *Volume `json:"volume,omitempty"`
	Agent, Helper, Anchor *Instance
	ActualStopped         bool             `json:"actualStopped"`
	NativeQuiet           bool             `json:"nativeQuiet,omitempty"`
	Receipt               *session.Receipt `json:"receipt,omitempty"`
	CheckpointDigest      string           `json:"checkpointDigest,omitempty"`
	// Removing is saved after the placement confirmed every recorded container
	// stopped and before it removes any of them. From then on an absent
	// container is one this placement removed, not missing writer evidence.
	Removing bool `json:"removing,omitempty"`
	// Removed records that the containers and workspace volume are gone.
	Removed bool `json:"removed,omitempty"`
}

type Placement struct {
	mu        sync.RWMutex
	journals  session.JournalLocks
	storageMu sync.Mutex
	dir       string
	lock      *os.File
	closed    bool
	engine    *Engine
	config    PlacementConfig
	digest    string
	authority Authority
}

func privateFile(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("docker placement path must be owned and private")
	}
	return nil
}
func OpenPlacement(dir string, engine *Engine, cfg PlacementConfig, authority Authority) (*Placement, error) {
	if engine == nil || authority == nil {
		return nil, errors.New("docker placement requires engine and trusted authority")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = privateFile(dir, true); err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "owner.lock")
	if _, err = os.Lstat(path); err == nil {
		if err = privateFile(path, false); err != nil {
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
		return nil, errors.New("docker placement directory already owned")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	p := &Placement{dir: dir, lock: lock, engine: engine, config: cfg, authority: authority, digest: fmt.Sprintf("%x", sha256.Sum256(encoded))}
	existing, err := p.read("config.json", maxRecordBytes)
	if os.IsNotExist(err) {
		err = p.write("config.json", encoded)
	} else if err == nil && !bytes.Equal(existing, encoded) {
		err = errors.New("docker placement configuration changed")
	}
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}
func (p *Placement) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return errors.Join(syscall.Flock(int(p.lock.Fd()), syscall.LOCK_UN), p.lock.Close())
}
func (p *Placement) ready() error {
	if p.closed {
		return errors.New("docker placement closed")
	}
	return nil
}

func (p *Placement) lockOperation(l session.Launch) (func(), error) {
	p.mu.RLock()
	unlock := p.journals.Lock(operationKey(l))
	if err := p.ready(); err != nil {
		unlock()
		p.mu.RUnlock()
		return nil, err
	}
	return func() { unlock(); p.mu.RUnlock() }, nil
}
func operationKey(l session.Launch) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(l.ExecutionID+"\x00"+l.OperationID)))
}
func (p *Placement) read(name string, limit int64) ([]byte, error) {
	path := filepath.Join(p.dir, name)
	if err := privateFile(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("docker placement file exceeds limit")
	}
	return data, nil
}
func (p *Placement) write(name string, data []byte) error {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	return p.writeUnlocked(name, data)
}

func (p *Placement) writeUnlocked(name string, data []byte) error {
	f, err := os.CreateTemp(p.dir, ".pending-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() { _ = os.Remove(temp) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temp, filepath.Join(p.dir, name)); err != nil {
		return err
	}
	parent, err := os.Open(p.dir)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
func (p *Placement) load(l session.Launch) (placementRecord, error) {
	var r placementRecord
	data, err := p.read(operationKey(l)+".json", maxRecordBytes)
	if err != nil {
		return r, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&r); err != nil {
		return r, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return r, errors.New("docker placement has trailing journal data")
	}
	if r.Version != recordVersion || r.ConfigDigest != p.digest || r.Execution != "spz-"+operationKey(l)[:32] || !session.SameLaunch(r.Launch, l) {
		return r, errors.New("docker placement immutable intent conflict")
	}
	return r, nil
}
func (p *Placement) save(r placementRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxRecordBytes {
		return errors.New("docker placement record exceeds limit")
	}
	return p.write(operationKey(r.Launch)+".json", data)
}

// fresh reports a first visit started from a Git workspace rather than a checkpoint.
func (r placementRecord) fresh() bool { return r.Launch.CheckpointDigest == "" }

// thread is the native thread the visit runs, once known.
func (r placementRecord) thread() string { return cmp.Or(r.Launch.ThreadID, r.Thread) }

func (p *Placement) retain(name string, data []byte) error {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	existing, err := p.read(name, p.config.MaxCheckpointBytes)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("docker immutable artifact changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return err
	}
	var total int64
	var records int
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("unexpected Docker placement storage entry")
		}
		total += info.Size()
		if filepath.Ext(entry.Name()) == ".json" {
			records++
		}
	}
	if total+int64(len(data)) > p.config.RetainedBytes || records > 1000 {
		return errors.New("docker placement retention limit reached")
	}
	return p.writeUnlocked(name, data)
}
