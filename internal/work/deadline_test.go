package work

import (
	"errors"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

func TestTaskDeadlineFollowsTheExecutorCap(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		limit  time.Duration
		choice string
		want   time.Time
		code   profile.Code
	}{
		{"capped default", time.Hour, "", now.Add(time.Hour), ""},
		{"capped shorter", time.Hour, "30m", now.Add(30 * time.Minute), ""},
		{"capped longer denied", time.Hour, "2h", time.Time{}, profile.CodeDenied},
		{"capped none denied", time.Hour, deadline.NoneArg, time.Time{}, profile.CodeDenied},
		{"uncapped default is none", 0, "", time.Time{}, ""},
		{"uncapped none", 0, deadline.NoneArg, time.Time{}, ""},
		{"uncapped any", 0, "48h", now.Add(48 * time.Hour), ""},
		{"invalid", 0, "forever", time.Time{}, profile.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &Owner{config: Config{MaxDuration: tc.limit}}
			got, err := o.taskDeadline(tc.choice, now)
			var rejection *profile.Rejection
			if tc.code != "" {
				if !errors.As(err, &rejection) || rejection.Code != tc.code {
					t.Fatalf("err = %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil || !got.Time().Equal(tc.want) {
				t.Fatalf("deadline = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// A task submitted without a deadline to an executor without a cap runs to
// its retained result; the choice is part of the submission's intent.
func TestTaskWithoutDeadlineRuns(t *testing.T) {
	c, _ := fixture(t)
	c.MaxDuration = 0
	o, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = o.Close() })
	q := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Task: fixtureTask, MaxDuration: deadline.NoneArg}
	v, e := o.Submit(t.Context(), q)
	if e != nil || !v.Deadline.IsZero() {
		t.Fatalf("submit without deadline: %+v %v", v, e)
	}
	q.MaxDuration = "1h"
	if _, e = o.Submit(t.Context(), q); e == nil {
		t.Fatal("a retry changed the submission's maximum duration")
	}
	if done := waitPhase(t, o, v.ID, phaseRetained); done.Result != testResult {
		t.Fatalf("task without deadline: %+v", done)
	}
}
