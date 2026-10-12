// Package file persists shared accounting rules in a private, single-owner directory.
package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/budget"
)

const maxRecordBytes = 16 << 10

type Ledger struct {
	budget.Ledger
	store *store
}

type store struct {
	dir    string
	lock   *os.File
	mu     sync.Mutex
	closed bool
}

// NewLedger retains an exclusive process lock until Close. Worker containers
// must never mount this directory; only the trusted gateway can mutate it.
func NewLedger(dir string) (*Ledger, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("budget: absolute private directory required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := private(dir, true); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "owner.lock")
	if _, err := os.Lstat(path); err == nil {
		if err := private(path, false); err != nil {
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
		return nil, errors.New("budget: ledger already owned")
	}
	s := &store{dir: dir, lock: lock}
	return &Ledger{Ledger: budget.NewLedger(s), store: s}, nil
}

func (l *Ledger) Close() error {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
}

func private(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("budget: ledger paths must be owned and private")
	}
	return nil
}

func (s *store) path(key budget.AccountKey) string {
	return filepath.Join(s.dir, fmt.Sprintf("account-%x.json", sha256.Sum256([]byte(key))))
}

func (s *store) read(key budget.AccountKey) (*budget.Record, error) {
	path := s.path(key)
	if err := private(path, false); err != nil {
		if os.IsNotExist(err) {
			return nil, budget.ErrAccountNotFound
		}
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if len(data) > maxRecordBytes {
		return nil, errors.New("budget: ledger record exceeds bound")
	}
	return budget.DecodeRecord(string(data))
}

func (s *store) Read(ctx context.Context, key budget.AccountKey) (*budget.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("budget: ledger closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.read(key)
}

func (s *store) Update(ctx context.Context, key budget.AccountKey, mutate func(*budget.Record, bool) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("budget: ledger closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := s.read(key)
	exists := err == nil
	if errors.Is(err, budget.ErrAccountNotFound) {
		record = budget.NewRecord()
	} else if err != nil {
		return err
	}
	if err = mutate(record, exists); err != nil {
		return err
	}
	data, err := record.Encode()
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return errors.New("budget: ledger record exceeds bound")
	}
	temp, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err = temp.WriteString(data); err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), s.path(key)); err != nil {
		return err
	}
	parent, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
