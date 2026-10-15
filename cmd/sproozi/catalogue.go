package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
)

const catalogueSessions = 512
const catalogueExecutors = 32
const availabilityAvailable = "available"
const availabilityUnavailable = "unavailable"
const ownershipHome = "home"

// Catalogue rows are compact discovery facts, not full execution journals or
// untrusted final answers. Executor labels the catalogue queried; placement
// separately names the current execution destination.
type catalogueRow struct {
	ID           string               `json:"id"`
	Executor     string               `json:"executor"`
	Ownership    string               `json:"ownership"`
	Phase        string               `json:"phase"`
	Harness      harness.NativeKind   `json:"harness"`
	ExecutionID  string               `json:"executionId,omitempty"`
	AllocationID string               `json:"allocationId,omitempty"`
	Incarnation  *session.Incarnation `json:"incarnation,omitempty"`
	Placement    string               `json:"placement,omitempty"`
	ThreadID     string               `json:"threadId,omitempty"`
	Deadline     time.Time            `json:"deadline"`
}
type executorAvailability struct {
	Executor     string       `json:"executor"`
	Availability string       `json:"availability"`
	Error        profile.Code `json:"error,omitempty"`
}
type catalogue struct {
	Sessions  []catalogueRow         `json:"sessions"`
	Executors []executorAvailability `json:"executors"`
	Truncated bool                   `json:"truncated"`
}
type discovered struct {
	availability executorAvailability
	rows         []catalogueRow
	err          error
}

func discoverSessions(ctx context.Context, q options) catalogue {
	names := selectedExecutors(q.fixed, q.executor)
	if q.executor != "" && q.executor != dockerExecutor {
		names = []string{q.executor}
	}
	if q.executor == "" || q.executor == dockerExecutor {
		names = append(names, dockerExecutor)
	}
	slices.Sort(names)
	out := catalogue{Sessions: []catalogueRow{}, Executors: []executorAvailability{}}
	if len(names) > catalogueExecutors {
		names = names[:catalogueExecutors]
		out.Truncated = true
	}
	results := make(chan discovered, len(names))
	for _, name := range names {
		go func() {
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			v := discovered{availability: executorAvailability{Executor: name, Availability: availabilityAvailable}}
			if name == dockerExecutor {
				response, err := home.Call(requestCtx, q.socket, home.Request{Action: home.ActionSessions})
				v.err = err
				for _, status := range response.Sessions {
					row := catalogueRow{
						ID: status.ID, Executor: name, Ownership: status.Ownership, Phase: status.Phase,
						Harness: status.Harness, ExecutionID: status.ExecutionID, AllocationID: status.AllocationID,
						Incarnation: status.Incarnation, Placement: status.Placement, ThreadID: status.ThreadID,
						Deadline: status.Deadline,
					}
					v.rows = append(v.rows, row)
				}
			} else {
				fixed, ok := workConnection(q.fixed, name)
				if !ok {
					v.err = &profile.Rejection{Code: profile.CodeInvalid}
				} else {
					client, err := work.NewClient(fixed)
					v.err = err
					if err == nil {
						rows, err := client.List(requestCtx)
						_ = client.Close()
						v.err = err
						for _, row := range rows {
							ownership := "executor"
							if row.Phase == "home-owned" {
								ownership = ownershipHome
							}
							v.rows = append(v.rows, catalogueRow{
								ID: row.ID, Executor: name, Ownership: ownership, Phase: row.Phase, Harness: row.Harness,
								ExecutionID: row.ExecutionID, AllocationID: row.AllocationID, Incarnation: row.Incarnation,
								Placement: row.Placement, ThreadID: row.ThreadID, Deadline: row.Deadline,
							})
						}
					}
				}
			}
			results <- v
		}()
	}
	for range names {
		v := <-results
		if v.err != nil {
			v.availability.Availability = availabilityUnavailable
			v.availability.Error = profile.CodeUnavailable
			var remote *profile.RemoteError
			var rejection *profile.Rejection
			if errors.As(v.err, &remote) {
				v.availability.Error = remote.Code
			} else if errors.As(v.err, &rejection) {
				v.availability.Error = rejection.Code
			}
		} else {
			out.Sessions = append(out.Sessions, v.rows...)
		}
		out.Executors = append(out.Executors, v.availability)
	}
	slices.SortFunc(out.Executors, func(a, b executorAvailability) int { return strings.Compare(a.Executor, b.Executor) })
	slices.SortFunc(out.Sessions, func(a, b catalogueRow) int {
		if c := strings.Compare(a.Executor, b.Executor); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(out.Sessions) > catalogueSessions {
		out.Sessions = out.Sessions[:catalogueSessions]
		out.Truncated = true
	}
	return out
}
