package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/work"
)

func TestMaxDurationFlag(t *testing.T) {
	for args, want := range map[string]string{
		"run --max-duration 8h":         "8h",
		"run --max-duration none":       deadline.NoneArg,
		"resume abcd --max-duration 1h": "1h",
	} {
		q, err := parse(strings.Fields(args))
		if err != nil || q.maxDuration != want {
			t.Errorf("%s: %q %v", args, q.maxDuration, err)
		}
		request, _, err := q.request(context.Background())
		if err != nil || request.MaxDuration != want {
			t.Errorf("%s: request %q %v", args, request.MaxDuration, err)
		}
	}
	for _, args := range []string{"run --max-duration 0s", "run --max-duration forever", "stop abcd --max-duration 1h"} {
		if _, err := parse(strings.Fields(args)); err == nil {
			t.Errorf("%s accepted", args)
		}
	}
}

const phaseActive = "active"

func TestDeadlineText(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		phase string
		at    time.Time
		want  string
	}{
		{phaseActive, time.Time{}, deadline.NoneArg},
		{phaseActive, at, formatTime(at)},
		{phaseCancelled, at, "-"},
		{phaseCancelled, time.Time{}, "-"},
		// Remote work reports the same phases.
		{"preparing", at, formatTime(at)},
		{"stopping", at, formatTime(at)},
		{string(work.PhaseRetained), at, "-"},
		// No phase is called starting; unresolved and retained ones have none.
		{"starting", at, "-"},
		{"unknown-stop", at, "-"},
		{string(work.PhasePulled), at, "-"},
	} {
		if got := deadlineText(tc.phase, tc.at); got != tc.want {
			t.Errorf("%s %v: %q, want %q", tc.phase, tc.at, got, tc.want)
		}
	}
	var out bytes.Buffer
	listed := catalogue{Sessions: []catalogueRow{{ID: "3f2b0000", Phase: phaseActive}}}
	if err := writeCatalogueTable(&out, listed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), deadline.NoneArg) {
		t.Fatalf("live session without deadline listed as %q", out.String())
	}
}

// An older service refuses the field, and the message says so.
func TestOldServiceRefusesMaxDuration(t *testing.T) {
	q := options{maxDuration: "8h"}
	err := q.homeError(errors.New(`json: unknown field "maxDuration"`))
	if err == nil || !strings.Contains(err.Error(), "older than this command") {
		t.Fatalf("error = %v", err)
	}
}
