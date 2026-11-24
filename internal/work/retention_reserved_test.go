package work

import (
	"os"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// A running session reserves its checkpoint, and admission counts it. A
// record that is gone, or present but not a valid record, reserves nothing;
// one that is present and cannot be read fails admission: "could not look"
// is not "nothing reserved". A directory in place of the record file is
// unreadable on every platform and for every user, which chmod 000 is not
// (root, Windows), so the test needs no seam on the read.
func TestAdmissionFailsClosedForARecordItCannotRead(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	path := o.path(v.ID, ".json")

	// Counted: with room for the kept bytes and three reservations (two for
	// the new session, one for the running one) admission succeeds, and one
	// byte less refuses.
	o.config.RetainedBytes = keptBytes(t, c) + 3*o.config.MaxCheckpointBytes
	if err := o.admitRetention(true); err != nil {
		t.Fatalf("admission with room for the running session's reservation: %v", err)
	}
	o.config.RetainedBytes--
	if err := o.admitRetention(true); code(err) != wireRetentionFull {
		t.Fatalf("admission with room for one byte less: %v", err)
	}

	// Not counted, and not an error: the record is gone.
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if n, err := o.reserved(v.ID); n != 0 || err != nil {
		t.Fatalf("reserved of a record that is gone: %d, %v; want 0, nil", n, err)
	}
	o.config.RetainedBytes = 1 << 40
	if err = o.admitRetention(true); err != nil {
		t.Fatalf("admission with the record gone: %v", err)
	}

	// Not counted, and not an error: the record is there and is not a record.
	if err = os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := o.reserved(v.ID); n != 0 || err != nil {
		t.Fatalf("reserved of a corrupt record: %d, %v; want 0, nil", n, err)
	}
	if err = o.admitRetention(true); err != nil {
		t.Fatalf("admission with a corrupt record: %v", err)
	}

	// An error, whatever the limit: the record is there and cannot be read.
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := o.reserved(v.ID); err == nil || n != 0 {
		t.Fatalf("reserved of an unreadable record: %d, %v; want an error", n, err)
	}
	err = o.admitRetention(true)
	if err == nil || OutcomeCode(err) != profile.CodeUnavailable {
		t.Fatalf("admission with an unreadable record: %v (%q), want a refusal as unavailable", err, OutcomeCode(err))
	}
	if err = o.admitRetention(false); err == nil || OutcomeCode(err) != profile.CodeUnavailable {
		t.Fatalf("a resume's admission with an unreadable record: %v", err)
	}

	// Put back, it counts again.
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, record, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := o.reserved(v.ID); n != 1 || err != nil {
		t.Fatalf("reserved of a running session: %d, %v; want 1, nil", n, err)
	}
}
