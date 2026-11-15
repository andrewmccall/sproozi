package home

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// launchDeadline is the deadline of a status's current execution.
func launchDeadline(t *testing.T, s Status) time.Time {
	t.Helper()
	if s.Session == nil || s.Session.Active == nil {
		t.Fatalf("status has no active visit: %+v", s)
	}
	return s.Session.Active.Launch.Deadline
}

// A session's own choice of deadline outlives its executions: started with
// none, it resumes with none whatever the home's maxDuration, until a resume
// chooses again (ADR 0056).
func TestSessionKeepsItsChoiceOfDeadline(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id := uuid.NewString()
	started, err := s.StartWithMaxDuration(t.Context(), id, uuid.NewString(), repo, "", nil, nil, "", "none")
	if err != nil {
		t.Fatal(err)
	}
	if !launchDeadline(t, started).IsZero() || !started.Deadline.IsZero() {
		t.Fatalf("start with none has a deadline: %v", started.Deadline)
	}
	if _, err = s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil || !launchDeadline(t, resumed).IsZero() {
		t.Fatalf("resume did not keep no deadline: %+v %v", resumed, err)
	}
	if _, err = s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	resumed, err = s.ResumeWithMaxDuration(t.Context(), id, uuid.NewString(), nil, "2h")
	if err != nil {
		t.Fatal(err)
	}
	if got := launchDeadline(t, resumed); got.Before(time.Now().Add(time.Hour)) || got.After(time.Now().Add(2*time.Hour)) {
		t.Fatalf("resume with 2h has deadline %v", got)
	}
	if c, err := s.readCatalog(id); err != nil || c.MaxDuration != "2h" {
		t.Fatalf("catalogue choice = %q, %v", c.MaxDuration, err)
	}
}

// A home without maxDuration starts sessions with no deadline; one with it
// gives them its deadline.
func TestHomeDefaultDeadline(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	s.duration = 0
	started, err := s.Start(t.Context(), uuid.NewString(), uuid.NewString(), repo, "", nil, nil)
	if err != nil || !launchDeadline(t, started).IsZero() {
		t.Fatalf("home without maxDuration: %+v %v", started, err)
	}
	s.duration = time.Hour
	started, err = s.Start(t.Context(), uuid.NewString(), uuid.NewString(), repo, "", nil, nil)
	if err != nil || launchDeadline(t, started).IsZero() {
		t.Fatalf("home with maxDuration: %+v %v", started, err)
	}
}

func TestMaxDurationRequestValidation(t *testing.T) {
	t.Parallel()
	for _, q := range []Request{
		{Action: ActionStop, ID: uuid.NewString(), OperationID: uuid.NewString(), MaxDuration: "1h"},
		{Action: ActionResume, ID: uuid.NewString(), OperationID: uuid.NewString(), MaxDuration: "0s"},
		{Action: ActionStart, ID: uuid.NewString(), OperationID: uuid.NewString(), MaxDuration: "forever"},
	} {
		if err := validateCommonRequest(q); err == nil {
			t.Errorf("%+v accepted", q)
		}
	}
	ok := Request{Action: ActionResume, ID: uuid.NewString(), OperationID: uuid.NewString(), MaxDuration: "none"}
	if err := validateCommonRequest(ok); err != nil {
		t.Fatal(err)
	}
}

func TestConfigMaxDurationIsOptional(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{`""`: "", `"none"`: "", `"48h"`: "", `"0s"`: "maxDuration:", `"x"`: "maxDuration:"} {
		_, err := readTestConfig(t, `,"maxDuration":`+value)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("maxDuration %s: %v", value, err)
		}
	}
	if d, err := defaultDuration(""); err != nil || d != 0 {
		t.Fatalf("omitted maxDuration = %v, %v", d, err)
	}
}
