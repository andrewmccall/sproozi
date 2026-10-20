// Package diagnose answers an execution's questions about its own recent
// gateway denials. It reuses gateway audit records; it makes no decisions.
package diagnose

import (
	"strconv"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// Limits bound the in-memory journal. Zero values select the defaults.
type Limits struct {
	Executions   int
	PerExecution int
	MaxAge       time.Duration
}

const (
	defaultExecutions   = 1024
	defaultPerExecution = 64
	defaultMaxAge       = time.Hour
)

// Entry is one retained denial. ID is meaningful only to the execution that
// owns it.
type Entry struct {
	ID    string
	Event audit.Event
}

type execution struct {
	entries []Entry // oldest first
	written uint64
}

// Journal is an audit.Writer that forwards every event and retains recent
// denials attributed to an execution. Contents are lost on gateway restart.
type Journal struct {
	next   audit.Writer
	limits Limits

	mu         sync.Mutex
	sequence   uint64
	executions map[string]*execution
}

func NewJournal(next audit.Writer, limits Limits) *Journal {
	if limits.Executions <= 0 {
		limits.Executions = defaultExecutions
	}
	if limits.PerExecution <= 0 {
		limits.PerExecution = defaultPerExecution
	}
	if limits.MaxAge <= 0 {
		limits.MaxAge = defaultMaxAge
	}
	return &Journal{next: next, limits: limits, executions: map[string]*execution{}}
}

// Log forwards e unchanged, then retains it when it is an attributed denial.
func (j *Journal) Log(e audit.Event) {
	if j.next != nil {
		j.next.Log(e)
	}
	if e.Allowed || e.RunUID == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.sequence++
	entry, ok := j.executions[e.RunUID]
	if !ok {
		if len(j.executions) >= j.limits.Executions {
			j.evictLeastRecent()
		}
		entry = &execution{}
		j.executions[e.RunUID] = entry
	}
	entry.written = j.sequence
	entry.entries = append(entry.entries, Entry{ID: "d" + strconv.FormatUint(j.sequence, 10), Event: e})
	if excess := len(entry.entries) - j.limits.PerExecution; excess > 0 {
		entry.entries = append([]Entry(nil), entry.entries[excess:]...)
	}
}

func (j *Journal) evictLeastRecent() {
	var oldest string
	var written uint64
	for run, entry := range j.executions {
		if oldest == "" || entry.written < written {
			oldest, written = run, entry.written
		}
	}
	delete(j.executions, oldest)
}

// Denials returns runUID's retained denials at or after since, newest first.
// Entries older than the journal's MaxAge relative to now are never returned.
func (j *Journal) Denials(runUID string, since, now time.Time) []Entry {
	if floor := now.Add(-j.limits.MaxAge); since.Before(floor) {
		since = floor
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.executions[runUID]
	if runUID == "" || !ok {
		return nil
	}
	var result []Entry
	for i := len(entry.entries) - 1; i >= 0; i-- {
		if !entry.entries[i].Event.Timestamp.Before(since) {
			result = append(result, entry.entries[i])
		}
	}
	return result
}

// Lookup returns runUID's retained denial with id.
func (j *Journal) Lookup(runUID, id string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if entry, ok := j.executions[runUID]; ok && runUID != "" {
		for _, d := range entry.entries {
			if d.ID == id {
				return d, true
			}
		}
	}
	return Entry{}, false
}
