package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
)

const catalogueSessions = 512
const catalogueExecutors = 32
const availabilityAvailable = "available"
const availabilityUnavailable = "unavailable"

// Ownership values: this machine owns the session, a pull to this machine is
// under way, or the cluster that listed it owns it.
const (
	ownershipLocal   = localName
	ownershipPulling = "pulling"
	ownershipCluster = "cluster"
)

// errorServiceNotRunning is the executor error when this machine's service is
// not running.
const errorServiceNotRunning profile.Code = "service_not_running"

// Catalogue rows are compact discovery facts, not full journals or untrusted
// final answers. Executor names the catalogue that listed the row: local for
// this machine, or a cluster's name; placement names where it runs now.
type catalogueRow struct {
	project.Association
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
	// Login is the named login of a harness that signs in itself.
	Login string `json:"login,omitempty"`
}
type executorAvailability struct {
	Executor string `json:"executor,omitempty"`
	// Context names a cluster that could not be reached, so its executor's
	// name is unknown.
	Context      string       `json:"context,omitempty"`
	Availability string       `json:"availability"`
	Error        profile.Code `json:"error,omitempty"`
}

// key names an availability row: its executor, or the context of a cluster
// that could not be reached.
func (e executorAvailability) key() string {
	if e.Context != "" {
		return "context " + e.Context
	}
	return e.Executor
}

type catalogue struct {
	Sessions  []catalogueRow         `json:"sessions"`
	Executors []executorAvailability `json:"executors"`
	Truncated bool                   `json:"truncated"`
	// problems holds each unavailable catalogue's error.
	problems map[string]error
}
type discovered struct {
	availability executorAvailability
	rows         []catalogueRow
	err          error
}

// sessions lists this machine's sessions and each cluster's. It never starts
// the service: a listing has no side effects.
func (q *options) sessions(ctx context.Context) error {
	var targets []remoteTarget
	if q.remote {
		if err := q.loadOptional(); err != nil {
			return err
		}
		target, err := resolveRemote(ctx, q.context, q.namespace)
		if err != nil {
			return err
		}
		targets = []remoteTarget{target}
	} else {
		if err := q.load(); err != nil {
			return err
		}
		targets = defaultTargets(ctx)
	}
	found := discoverSessions(ctx, q, !q.remote, targets)
	if q.json {
		if err := writeJSON(q.out, found); err != nil {
			return err
		}
	} else if len(found.problems) < len(found.Executors) {
		if err := writeCatalogueTable(q.out, found); err != nil {
			return err
		}
	}
	return found.failure(q)
}

func discoverSessions(ctx context.Context, q *options, local bool, targets []remoteTarget) catalogue {
	out := catalogue{Sessions: []catalogueRow{}, Executors: []executorAvailability{}, problems: map[string]error{}}
	if len(targets) > catalogueExecutors {
		targets, out.Truncated = targets[:catalogueExecutors], true
	}
	results := make(chan discovered, len(targets)+1)
	if local {
		go func() { results <- q.discoverLocal(ctx) }()
	}
	for _, target := range targets {
		go func() { results <- q.discoverRemote(ctx, target) }()
	}
	for range len(targets) + btoi(local) {
		v := <-results
		if v.err != nil {
			v.availability.Availability = availabilityUnavailable
			v.availability.Error = work.OutcomeCode(v.err)
			if errors.Is(v.err, home.ErrControlUnavailable) {
				v.availability.Error = errorServiceNotRunning
			}
			out.problems[v.availability.key()] = v.err
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
		out.Sessions, out.Truncated = out.Sessions[:catalogueSessions], true
	}
	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (q *options) discoverLocal(ctx context.Context) discovered {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v := discovered{availability: executorAvailability{Executor: localName, Availability: availabilityAvailable}}
	response, err := home.Call(requestCtx, q.socket, home.Request{Action: home.ActionSessions, Project: q.project})
	v.err = err
	for _, status := range response.Sessions {
		status = q.publicStatus(status)
		ownership := status.Ownership
		if ownership == "source" {
			ownership = ownershipPulling
		}
		v.rows = append(v.rows, catalogueRow{
			Association: status.Association, ID: status.ID, Executor: localName, Ownership: ownership,
			Phase: status.Phase, Harness: status.Harness, ExecutionID: status.ExecutionID,
			AllocationID: status.AllocationID, Incarnation: status.Incarnation, Placement: status.Placement,
			ThreadID: status.ThreadID, Deadline: status.Deadline, Login: status.Login,
		})
	}
	return v
}

func (q *options) discoverRemote(ctx context.Context, target remoteTarget) discovered {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v := discovered{availability: executorAvailability{Executor: target.Name, Availability: availabilityAvailable}}
	if target.Context != "" {
		v.availability = executorAvailability{Context: target.Context}
	}
	if target.Err != nil {
		v.err = target.Err
		return v
	}
	client := target.Client
	defer func() { _ = client.Close() }()
	views, err := client.List(requestCtx, work.SessionQuery{Project: q.project})
	v.err = err
	for _, view := range views {
		ownership, phase := ownershipCluster, string(view.Phase)
		if view.Phase == work.PhasePulled {
			// The cluster keeps a record of a session pulled to a machine.
			ownership = ownershipLocal
		}
		v.rows = append(v.rows, catalogueRow{
			Association: view.Association, ID: view.ID, Executor: target.Name, Ownership: ownership, Phase: phase,
			Harness: view.Harness, ExecutionID: view.ExecutionID, AllocationID: view.AllocationID,
			Incarnation: view.Incarnation, Placement: view.Placement, ThreadID: view.ThreadID, Deadline: view.Deadline,
			Login: view.Login,
		})
	}
	return v
}

// failure explains catalogues that did not answer. It fails only when none
// did; otherwise it notes each one on standard error.
func (c catalogue) failure(q *options) error {
	explainOne := func(e executorAvailability, err error) string {
		switch {
		case errors.Is(err, home.ErrControlUnavailable):
			// Listing never starts the service, which only runs while needed.
			command := startServiceCommand
			if q.configPath != "" {
				command = startCommand(q.configPath)
			}
			return "the Sproozi service is not running, so this machine's sessions can't be listed; " +
				"start it with \"" + command + "\" (run, attach, stop and resume start it too)"
		case e.Context != "":
			return fmt.Sprintf("cluster (context %s) is unreachable: %s", e.Context, executorCause(err))
		case e.Executor == localName:
			return "this machine's sessions: " + explain(err)
		}
		return fmt.Sprintf("cluster %s did not answer: %s", e.Executor, executorCause(err))
	}
	if len(c.problems) < len(c.Executors) {
		for _, e := range c.Executors {
			if err := c.problems[e.key()]; err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Not listed: %s\n", explainOne(e, err))
			}
		}
		return nil
	}
	var reasons []string
	for _, e := range c.Executors {
		reasons = append(reasons, explainOne(e, c.problems[e.key()]))
	}
	if len(reasons) == 1 {
		return errors.New(reasons[0])
	}
	return fmt.Errorf("no sessions listed: %s", strings.Join(reasons, "; "))
}

// executorCause explains why a cluster did not answer, including this client's
// own transport failure, which the error message leaves out.
func executorCause(err error) string {
	var remote *profile.RemoteError
	if errors.As(err, &remote) && remote.Transport != nil {
		return fmt.Sprintf("%v (%v)", err, remote.Transport)
	}
	return fmt.Sprint(err)
}
