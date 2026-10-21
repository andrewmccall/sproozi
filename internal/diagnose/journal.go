// Package diagnose answers an execution's questions about its own recent
// gateway denials. It reuses gateway audit records; it makes no decisions.
package diagnose

import (
	"slices"
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
	// MaxBytes bounds the retained text across all executions.
	MaxBytes int
}

const (
	defaultExecutions   = 1024
	defaultPerExecution = 64
	defaultMaxAge       = time.Hour
	defaultMaxBytes     = 16 << 20
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
	bytes   int
}

// Journal is an audit.Writer that forwards every event and retains recent
// denials attributed to an execution. Contents are lost on gateway restart.
type Journal struct {
	next   audit.Writer
	limits Limits

	mu         sync.Mutex
	sequence   uint64
	bytes      int
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
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaultMaxBytes
	}
	return &Journal{next: next, limits: limits, executions: map[string]*execution{}}
}

// Log forwards e, then retains a bounded copy when it is an attributed
// denial. Retained text is bounded per field, per execution, by age and in
// total, so denials with large workload-chosen text cannot exhaust memory.
func (j *Journal) Log(e audit.Event) {
	e = audit.Bound(e)
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
	size := eventBytes(e)
	entry.entries = append(entry.entries, Entry{ID: "d" + strconv.FormatUint(j.sequence, 10), Event: e})
	entry.bytes += size
	j.bytes += size
	floor := e.Timestamp.Add(-j.limits.MaxAge)
	drop := len(entry.entries) - j.limits.PerExecution
	for i, d := range entry.entries {
		if i < drop || (!e.Timestamp.IsZero() && d.Event.Timestamp.Before(floor)) {
			continue
		}
		drop = i
		break
	}
	if drop > 0 {
		for _, d := range entry.entries[:drop] {
			removed := eventBytes(d.Event)
			entry.bytes -= removed
			j.bytes -= removed
		}
		entry.entries = append([]Entry(nil), entry.entries[drop:]...)
	}
	for j.bytes > j.limits.MaxBytes && len(j.executions) > 1 {
		j.evictLeastRecent()
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
	if entry, ok := j.executions[oldest]; ok {
		j.bytes -= entry.bytes
	}
	delete(j.executions, oldest)
}

// eventBytes approximates the memory a retained event's text holds.
func eventBytes(e audit.Event) int {
	return 256 + len(e.Operation) + len(e.Resource) + len(e.DenyReason) + len(e.ArtifactURL) +
		len(e.RunID) + len(e.RunUID) + len(e.SAName) + len(e.PolicyName) + len(e.Capability) + len(e.Service)
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
	for _, v := range slices.Backward(entry.entries) {
		if !v.Event.Timestamp.Before(since) {
			result = append(result, v)
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
