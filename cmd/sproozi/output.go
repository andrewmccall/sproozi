package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/work"
)

// localName is the one name for this machine in commands and output.
const localName = "local"

// serviceOwnership is the service's own name for sessions this machine owns.
const serviceOwnership = "home"

// phaseCancelled is the phase of a session whose cancellation finished.
const phaseCancelled = "cancelled"

// livePhases are the phases in which a session's deadline still applies.
var livePhases = []string{"preparing", "activating", "active", "stopping", "starting"}

// writeJSON prints one indented JSON document.
func writeJSON(w io.Writer, v any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

// shortID is the start of a session ID that commands accept and lists show.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// localPlacement reports a placement name that means this machine.
func (q *options) localPlacement(name string) bool {
	if name == q.fixed.Local.Name || name == localName || strings.HasPrefix(name, localName+"-") {
		return true
	}
	return slices.ContainsFunc(q.fixed.Harnesses, func(h home.LocalHarness) bool { return h.Name == name })
}

// publicStatus names this machine "local" wherever the service uses its own
// names for it.
func (q *options) publicStatus(s home.Status) home.Status {
	if s.Ownership == serviceOwnership {
		s.Ownership = localName
	}
	if q.localPlacement(s.Placement) {
		s.Placement = localName
	}
	return s
}

// printStatus prints a session command's result.
func (q *options) printStatus(s home.Status) error {
	s = q.publicStatus(s)
	if q.json {
		return writeJSON(q.out, s)
	}
	_, err := fmt.Fprintln(q.out, statusText(q.action(), s, q.detach))
	return err
}

// statusText is a line about the session and, where there is one, the next
// command to run.
func statusText(action string, s home.Status, detached bool) string {
	id, short := s.ID, shortID(s.ID)
	switch action {
	case cmdStop:
		if s.Exit != nil && s.Exit.Code != 0 {
			return fmt.Sprintf("Session %s had already stopped: its agent exited with code %d.%s\n"+
				"Continue it with: sproozi resume %s    Delete it with: sproozi rm %s",
				id, s.Exit.Code, exitOutput(s.Exit), short, short)
		}
		return fmt.Sprintf("Stopped session %s; its work is kept.\nContinue it with: sproozi resume %s", id, short)
	case cmdCancel:
		return fmt.Sprintf("Cancelled session %s.\nDelete it with: sproozi rm %s", id, short)
	case cmdRemove:
		return fmt.Sprintf("Deleted session %s.", id)
	case cmdMove:
		return fmt.Sprintf("Moved session %s to %s.", id, s.Placement)
	case cmdPull:
		return fmt.Sprintf("Pulled session %s to this machine.\nContinue it with: sproozi resume %s", id, short)
	case cmdRun, cmdResume:
		verb := "Started"
		if action == cmdResume {
			verb = "Resumed"
		}
		line := fmt.Sprintf("%s session %s (%s).", verb, id, s.Harness)
		if detached {
			line += fmt.Sprintf("\nConnect with: sproozi attach %s    Stop with: sproozi stop %s", short, short)
		}
		return line
	}
	text := describe(id, string(s.Harness), s.Phase, s.Placement, s.Deadline)
	switch {
	case s.Exit != nil && s.Exit.Code != 0:
		text += fmt.Sprintf("\nExited     code %d%s", s.Exit.Code, exitOutput(s.Exit))
		text += fmt.Sprintf("\nContinue it with: sproozi resume %s    Delete it with: sproozi rm %s", short, short)
	case s.Exit != nil:
		text += "\nExited     code 0: the agent quit by itself"
	}
	return text
}

// exitOutput indents the last lines an exited agent printed.
func exitOutput(e *session.Exit) string {
	if e.Output == "" {
		return ""
	}
	return "\n           " + strings.ReplaceAll(e.Output, "\n", "\n           ")
}

// describe is the status summary of a session.
func describe(id, kind, phase, placement string, deadline time.Time) string {
	lines := []string{"Session    " + id, "Harness    " + kind, "Phase      " + phase}
	if placement != "" {
		lines = append(lines, "Placement  "+placement)
	}
	if slices.Contains(livePhases, phase) && !deadline.IsZero() {
		lines = append(lines, "Deadline   "+formatTime(deadline))
	}
	return strings.Join(lines, "\n")
}

// printView prints a cluster's session.
func (q *options) printView(cluster string, v work.View) error {
	if q.json {
		return writeJSON(q.out, v)
	}
	text := describe(v.ID, string(v.Harness), string(v.Phase), cluster, v.Deadline)
	if v.Problem != "" {
		text += "\nProblem    " + v.Problem
	}
	if v.Result != "" {
		text += "\n\n" + v.Result
	}
	_, err := fmt.Fprintln(q.out, text)
	return err
}

func formatTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04 MST") }

// writeCatalogueTable prints the sessions as a table.
func writeCatalogueTable(w io.Writer, c catalogue) error {
	if len(c.Sessions) == 0 {
		_, err := fmt.Fprintln(w, "No sessions.")
		return err
	}
	// A pulled session is listed by both catalogues, so name the catalogue
	// whenever rows come from more than one.
	several := slices.ContainsFunc(c.Sessions, func(r catalogueRow) bool { return r.Executor != c.Sessions[0].Executor })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{"ID", "HARNESS", "PHASE", "PLACEMENT", "DEADLINE"}
	if several {
		header = append(header, "LISTED BY")
	}
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range c.Sessions {
		ends := "-"
		if slices.Contains(livePhases, r.Phase) && !r.Deadline.IsZero() {
			ends = formatTime(r.Deadline)
		}
		row := []string{shortID(r.ID), string(r.Harness), r.Phase, r.Placement, ends}
		if several {
			row = append(row, r.Executor)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if c.Truncated {
		_, err := fmt.Fprintln(w, "The list is cut short; narrow it with --context or --project.")
		return err
	}
	return nil
}
