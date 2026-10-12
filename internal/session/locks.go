package session

import "sync"

type journalLock struct {
	mu      sync.Mutex
	callers int
}

// JournalLocks serializes one retained identity without blocking independent
// sessions or allocations. Idle entries are removed; arbitrary failed lookup
// requests cannot accumulate an unbounded lock table. It grants no authority.
type JournalLocks struct {
	mu   sync.Mutex
	rows map[string]*journalLock
}

func (j *JournalLocks) Lock(key string) func() {
	j.mu.Lock()
	if j.rows == nil {
		j.rows = make(map[string]*journalLock)
	}
	row := j.rows[key]
	if row == nil {
		row = &journalLock{}
		j.rows[key] = row
	}
	row.callers++
	j.mu.Unlock()
	row.mu.Lock()
	return func() {
		row.mu.Unlock()
		j.mu.Lock()
		defer j.mu.Unlock()
		row.callers--
		if row.callers == 0 {
			delete(j.rows, key)
		}
	}
}
