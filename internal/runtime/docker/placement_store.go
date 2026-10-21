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
	"reflect"
	"strings"
	"sync"

	"github.com/andrewmccall/sproozi/internal/privatefs"
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
	ActualStopped         bool `json:"actualStopped"`
	NativeQuiet           bool `json:"nativeQuiet,omitempty"`
	// Turnless records that the supervisor stopped a first visit with no
	// native conversation to keep; its stop captures no checkpoint.
	Turnless         bool             `json:"turnless,omitempty"`
	Receipt          *session.Receipt `json:"receipt,omitempty"`
	CheckpointDigest string           `json:"checkpointDigest,omitempty"`
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
	if err = privatefs.EnsureDir(dir); err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	lock, err := privatefs.Lock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return nil, fmt.Errorf("docker placement directory already owned: %w", err)
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
		err = p.reconfigure(existing, encoded)
	}
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// reconfigure accepts changed settings only while no execution record exists.
// Every record carries the digest of the settings it was prepared under and is
// never deleted, so changing them afterwards would strand those executions.
func (p *Placement) reconfigure(existing, encoded []byte) error {
	changed := changedSettings(existing, encoded)
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return err
	}
	records := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" && entry.Name() != "config.json" {
			records++
		}
	}
	if records == 0 {
		return p.write("config.json", encoded)
	}
	noun := "execution records in %s were"
	if records == 1 {
		noun = "execution record in %s was"
	}
	return fmt.Errorf("local placement settings changed (%s), but %d "+noun+" prepared "+
		"under the earlier values; set them back to keep those sessions, or use a new home directory for the "+
		"new values", strings.Join(changed, ", "), records, p.dir)
}

// changedSettings names, as local.* settings, the fields that differ between
// two encoded PlacementConfig values, with the earlier and new values where short.
func changedSettings(existing, encoded []byte) []string {
	var before, after map[string]json.RawMessage
	if json.Unmarshal(existing, &before) != nil || json.Unmarshal(encoded, &after) != nil {
		return []string{"unreadable config.json"}
	}
	var changed []string
	t := reflect.TypeFor[PlacementConfig]()
	for field := range t.Fields() {
		name := field.Name
		if bytes.Equal(before[name], after[name]) {
			continue
		}
		setting := "local." + strings.ToLower(name[:1]) + name[1:]
		if len(before[name]) <= 80 && len(after[name]) <= 80 {
			setting += fmt.Sprintf(" %s -> %s", cmp.Or(string(before[name]), "unset"), after[name])
		}
		changed = append(changed, setting)
	}
	return changed
}

func (p *Placement) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return privatefs.UnlockClose(p.lock)
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
	return privatefs.ReadFile(filepath.Join(p.dir, name), limit)
}
func (p *Placement) write(name string, data []byte) error {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	return p.writeUnlocked(name, data)
}

func (p *Placement) writeUnlocked(name string, data []byte) error {
	return privatefs.Replace(p.dir, name, data)
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
