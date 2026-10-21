package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"golang.org/x/term"
)

// summarised lists the commands that print a short summary, rather than JSON,
// to a terminal.
var summarised = []string{home.ActionStart, home.ActionResume, home.ActionStop, home.ActionSessions}

// humanOutput reports whether this command's result goes to a person: it
// prints a summary, standard output is a terminal and --json was not given.
// Scripts read standard output through a pipe, so they keep the JSON.
func (q *options) humanOutput() bool {
	f, ok := q.out.(*os.File)
	return slices.Contains(summarised, q.action) && !q.setup.json && ok && term.IsTerminal(int(f.Fd()))
}

// printStatus writes a session command's resulting status.
func (q *options) printStatus(s home.Status) error {
	if !q.humanOutput() {
		return json.NewEncoder(q.out).Encode(s)
	}
	_, err := fmt.Fprintln(q.out, statusSummary(q.action, s, q.detach))
	return err
}

// statusSummary is one line about the session and one naming the next command.
func statusSummary(action string, s home.Status, detached bool) string {
	line := fmt.Sprintf("Session %s: %s, %s", s.ID, s.Harness, s.Phase)
	if s.Placement != "" {
		line += " on " + s.Placement
	}
	if !s.Deadline.IsZero() {
		line += ", deadline " + deadline(s.Deadline)
	}
	switch {
	case action == home.ActionStop:
		return line + "\nResume it with: sproozi resume " + s.ID
	case detached:
		return line + "\nAttach with: sproozi attach " + s.ID + "    Stop with: sproozi stop " + s.ID
	}
	return line
}

func deadline(t time.Time) string { return t.Local().Format("2006-01-02 15:04 MST") }

// printCatalogue writes the sessions catalogue as a table for a person, or as
// one JSON document.
func (q *options) printCatalogue(c catalogue) error {
	if !q.humanOutput() {
		return json.NewEncoder(q.out).Encode(c)
	}
	return writeCatalogueTable(q.out, c)
}

func writeCatalogueTable(w io.Writer, c catalogue) error {
	if len(c.Sessions) == 0 {
		_, err := fmt.Fprintln(w, "No sessions.")
		return err
	}
	// A pulled session is listed by both catalogues, so name the catalogue
	// whenever rows come from more than one.
	single := !slices.ContainsFunc(c.Sessions, func(r catalogueRow) bool { return r.Executor != c.Sessions[0].Executor })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{"ID", "HARNESS", "PHASE", "PLACEMENT", "DEADLINE"}
	if !single {
		header = append(header, "EXECUTOR")
	}
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range c.Sessions {
		row := []string{r.ID, string(r.Harness), r.Phase, r.Placement, "-"}
		if !r.Deadline.IsZero() {
			row[4] = deadline(r.Deadline)
		}
		if !single {
			row = append(row, r.Executor)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if c.Truncated {
		_, err := fmt.Fprintln(w, "The list is truncated; run with --json, or narrow it with --executor or --project.")
		return err
	}
	return nil
}
