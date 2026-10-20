// Package file persists shared accounting rules in a private, single-owner directory.
package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/privatefs"
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
	if err := privatefs.EnsureDir(dir); err != nil {
		return nil, err
	}
	lock, err := privatefs.Lock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return nil, fmt.Errorf("budget: ledger already owned: %w", err)
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
	return privatefs.UnlockClose(s.lock)
}

func (s *store) path(key budget.AccountKey) string {
	return filepath.Join(s.dir, fmt.Sprintf("account-%x.json", sha256.Sum256([]byte(key))))
}

func (s *store) read(key budget.AccountKey) (*budget.Record, error) {
	path := s.path(key)
	data, err := privatefs.ReadFile(path, maxRecordBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, budget.ErrAccountNotFound
		}
		return nil, err
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
	return privatefs.Replace(s.dir, filepath.Base(s.path(key)), []byte(data))
}

// Sweep settles every expired reservation lease in the ledger, so an idle
// account does not keep an abandoned reservation until its next reservation
// or seal. The home daemon runs it at start and then periodically. legacy
// also charges in full every pending reservation without a lease, which can
// only come from a daemon from before leases. A corrupt record is logged and
// skipped; only an I/O error fails the sweep.
func (l *Ledger) Sweep(now time.Time, legacy bool) error {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("budget: ledger closed")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var first error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "account-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := s.recoverFile(name, now, legacy); err != nil && first == nil {
			first = fmt.Errorf("budget: ledger %s: %w", name, err)
		}
	}
	return first
}

// recoverFile settles one record file's expired leases. The caller holds s.mu.
func (s *store) recoverFile(name string, now time.Time, legacy bool) error {
	data, err := privatefs.ReadFile(filepath.Join(s.dir, name), maxRecordBytes)
	if err != nil {
		return err
	}
	needed, err := budget.NeedsRecovery(string(data), now, legacy)
	if err != nil {
		slog.Default().Error("Skipped corrupt budget ledger", "file", name, "error", err.Error())
		return nil
	}
	if !needed {
		return nil
	}
	record, err := budget.DecodeRecord(string(data))
	if err != nil {
		return err
	}
	committed, err := budget.RecoverExpired(record, name, now, legacy)
	if err != nil {
		return err
	}
	encoded, err := record.Encode()
	if err != nil {
		return err
	}
	if err := privatefs.Replace(s.dir, name, []byte(encoded)); err != nil {
		return err
	}
	committed()
	return nil
}
