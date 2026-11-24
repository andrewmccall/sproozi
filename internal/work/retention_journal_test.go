package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// journalOf starts an interactive session and returns its owner, ID and the
// path of its current execution's journal.
func journalOf(t *testing.T) (*Owner, string, string) {
	t.Helper()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	r, err := o.read(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(filepath.Dir(c.Directory), "sessions", r.Current+".json")
	if n, err := o.reserved(v.ID); n != 1 || err != nil {
		t.Fatalf("reserved of a running session: %d, %v; want 1, nil", n, err)
	}
	return o, v.ID, journal
}

// A journal that is there and cannot be read fails closed, like a record, and
// the log names the session. A directory in place of the file is unreadable
// on every platform and for every user.
func TestAdmissionFailsClosedForAJournalItCannotRead(t *testing.T) {
	t.Parallel()
	o, id, journal := journalOf(t)
	var log strings.Builder
	o.config.Log = &log
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := o.reserved(id); err == nil || n != 0 {
		t.Fatalf("reserved with an unreadable journal: %d, %v; want an error", n, err)
	}
	o.config.RetainedBytes = 1 << 40
	for _, newSession := range []bool{true, false} {
		if err := o.admitRetention(newSession); OutcomeCode(err) != profile.CodeUnavailable {
			t.Fatalf("admission (new session %v) with an unreadable journal: %v", newSession, err)
		}
	}
	if !strings.Contains(log.String(), "Could not determine reserved storage for work session "+id+": ") {
		t.Fatalf("the log does not name the session: %q", log.String())
	}
}

// A journal that is read but is not valid reserves nothing, as a corrupt
// record does: every coordinator operation loads it first and fails, so
// nothing commits a checkpoint for it, and failing closed would refuse all
// work until the volume is edited by hand.
func TestAdmissionAdmitsPastAnInvalidJournal(t *testing.T) {
	t.Parallel()
	o, id, journal := journalOf(t)
	for _, content := range []string{"{bad", `{"id":"x"} trailing`, "{}"} {
		if err := privatefs.Replace(filepath.Dir(journal), filepath.Base(journal), []byte(content)); err != nil {
			t.Fatal(err)
		}
		if n, err := o.reserved(id); n != 0 || err != nil {
			t.Fatalf("reserved with journal %q: %d, %v; want 0, nil", content, n, err)
		}
		o.config.RetainedBytes = 1 << 40
		if err := o.admitRetention(true); err != nil {
			t.Fatalf("admission with journal %q: %v", content, err)
		}
	}
}

// A journal removed, with the session's mark written, between the record's
// read and the journal's is gone: the one window in which the journal read
// answers invalid.
func TestReservedIsZeroForAJournalRemovedMidRead(t *testing.T) {
	t.Parallel()
	o, id, journal := journalOf(t)
	o.config.interleave = func(step, got string) {
		if step == stepEntry && got == id {
			_ = os.Remove(journal)
			_ = privatefs.Replace(o.config.Directory, id+removedSuffix, []byte(`{"version":1,"id":"`+id+`"}`))
		}
	}
	n, err := o.reserved(id)
	o.config.interleave = nil
	if n != 0 || err != nil {
		t.Fatalf("reserved of a journal removed mid-read: %d, %v; want 0, nil", n, err)
	}
}
