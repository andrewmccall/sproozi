package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/session/internal/lifecycle"
)

// journalVersion 2 starts each execution at its first visit. Version 1
// journals, which began at an imported stopped visit, are not read.
const journalVersion = 2
const maxJournalBytes = int64(1 << 20)
const maxOperations = 1000
const checkpointsDir = "checkpoints"
const workspacesDir = "workspaces"
const profilesDir = "profiles"

func checkpointPath(digest string) string { return filepath.Join(checkpointsDir, digest+".json") }
func workspacePath(digest string) string  { return filepath.Join(workspacesDir, digest+".workspace") }
func profilePath(digest string) string {
	return filepath.Join(profilesDir, strings.TrimPrefix(digest, "sha256:")+".tar.gz")
}

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
	// now reads the clock for original-deadline checks.
	now func() time.Time
	// profile archives the user's current profile for a new visit; an empty
	// digest means there is none.
	profile func() (archive []byte, digest string, err error)
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
	if err = privatefs.EnsureDir(dir); err != nil {
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
	lock, err := privatefs.Lock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return nil, fmt.Errorf("session: coordinator already owned: %w", err)
	}
	c := &Coordinator{dir: dir, placements: configured, maxCheckpoint: maxCheckpointBytes, lock: lock, now: time.Now}
	for _, name := range []string{checkpointsDir, workspacesDir, profilesDir} {
		sub := filepath.Join(dir, name)
		if err = privatefs.EnsureDir(sub); err != nil {
			_ = c.Close()
			return nil, err
		}
		if err = privatefs.Check(sub, true); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// SetProfileSource archives the user's profile when each move creates its
// launch. Retries reuse the journaled launch and its retained archive.
func (c *Coordinator) SetProfileSource(source func() ([]byte, string, error)) { c.profile = source }

// ProfileArchive returns a retained profile archive by digest.
func (c *Coordinator) ProfileArchive(digest string) ([]byte, error) {
	if !agentprofile.ValidDigest(digest) {
		return nil, errors.New("session: invalid profile digest")
	}
	return readFile(filepath.Join(c.dir, profilePath(digest)), agentprofile.MaxArchiveBytes)
}

// retainProfile archives the current profile for a new launch.
func (c *Coordinator) retainProfile() (string, error) {
	if c.profile == nil {
		return "", nil
	}
	archive, digest, err := c.profile()
	if err != nil || digest == "" {
		return "", err
	}
	return digest, c.retainImmutable(profilePath(digest), archive)
}

func (c *Coordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return privatefs.UnlockClose(c.lock)
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
	return privatefs.Replace(c.dir, r.ID+".json", data)
}
func readFile(p string, max int64) ([]byte, error) { return privatefs.ReadFile(p, max) }
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
	path := checkpointPath(meta.SHA256)
	if err = c.retainImmutable(path, data); err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Metadata: meta, Path: path}, nil
}

// retainWorkspace keeps a new execution's captured Git workspace input.
func (c *Coordinator) retainWorkspace(data []byte) (string, error) {
	if len(data) == 0 || int64(len(data)) > c.maxCheckpoint {
		return "", errors.New("session: workspace input exceeds bound")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return digest, c.retainImmutable(workspacePath(digest), data)
}

// retainImmutable writes content-addressed data once; a retry must match it.
func (c *Coordinator) retainImmutable(path string, data []byte) error {
	existing, err := readFile(filepath.Join(c.dir, path), c.maxCheckpoint)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("session: retained input collision")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return privatefs.Replace(filepath.Join(c.dir, filepath.Dir(path)), filepath.Base(path), data)
}

// input reads what a launch restores: the committed checkpoint it names, or the
// execution's retained workspace for a first visit started from Git. Both are
// content-addressed and immutable, so a retry reads the same bytes even after
// later visits committed newer checkpoints.
func (c *Coordinator) input(r Record, l Launch) ([]byte, error) {
	path, digest := checkpointPath(l.CheckpointDigest), l.CheckpointDigest
	if digest == "" {
		path, digest = workspacePath(r.Workspace), r.Workspace
	}
	data, err := readFile(filepath.Join(c.dir, path), c.maxCheckpoint)
	if err == nil && fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
		err = errors.New("session: retained input changed")
	}
	return data, err
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

// Checkpoint returns the execution's last committed checkpoint bytes.
func (c *Coordinator) Checkpoint(id string) ([]byte, error) {
	unlock, err := c.lockSession(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := c.load(id)
	if err != nil {
		return nil, err
	}
	if !r.hasCheckpoint() {
		return nil, errors.New("session: execution has no committed checkpoint")
	}
	return c.checkpointBytes(r)
}

// List returns the IDs of every retained execution journal.
func (c *Coordinator) List() ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.ready(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && validUUID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Retained reports the executions and total bytes kept in the coordinator
// directory, including inputs and checkpoints, so an owner can bound storage.
func (c *Coordinator) Retained() (executions int, size int64, err error) {
	ids, err := c.List()
	if err != nil {
		return 0, 0, err
	}
	err = filepath.WalkDir(c.dir, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		info, err := entry.Info()
		if err == nil {
			size += info.Size()
		}
		return err
	})
	return len(ids), size, err
}
