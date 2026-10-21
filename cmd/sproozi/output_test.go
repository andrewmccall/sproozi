//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// Every command summary starts in the same column, however long its name.
func TestUsageAlignsEverySummary(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	column := -1
	for _, c := range commands {
		for line := range strings.SplitSeq(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, "  "+c.name+" "); ok {
				at := len(line) - len(strings.TrimLeft(rest, " "))
				if column >= 0 && at != column {
					t.Fatalf("%s summary at column %d, want %d:\n%s", c.name, at, column, out.String())
				}
				column = at
			}
		}
	}
}

// Every registered flag has a description.
func TestEveryFlagIsDescribed(t *testing.T) {
	for _, c := range commands {
		for _, name := range c.flags {
			if c.help(name) == "" {
				t.Errorf("%s --%s has no description", c.name, name)
			}
		}
	}
}

// A person sees a summary and the next command; a script reads the JSON.
func TestStatusAndCatalogueSummaries(t *testing.T) {
	id := uuid.NewString()
	s := home.Status{ID: id, Harness: "codex", Phase: "active", Placement: "local",
		Deadline: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	got := statusSummary(home.ActionStart, s, true)
	if !strings.Contains(got, "Session "+id+": codex, active on local, deadline ") ||
		!strings.Contains(got, "sproozi attach "+id) || !strings.Contains(got, "sproozi stop "+id) {
		t.Errorf("start summary: %q", got)
	}
	if got := statusSummary(home.ActionStop, s, false); !strings.Contains(got, "sproozi resume "+id) {
		t.Errorf("stop summary: %q", got)
	}
	var out bytes.Buffer
	q := options{action: home.ActionStop, out: &out}
	if err := q.printStatus(s); err != nil || !strings.HasPrefix(out.String(), `{"id":"`+id+`"`) {
		t.Errorf("non-terminal output is not JSON: %q %v", out.String(), err)
	}
	out.Reset()
	c := catalogue{Sessions: []catalogueRow{{ID: id, Executor: dockerExecutor, Harness: "codex", Phase: "active",
		Placement: "local", Deadline: s.Deadline}}}
	if err := writeCatalogueTable(&out, c); err != nil || !strings.HasPrefix(out.String(), "ID  ") ||
		!strings.Contains(out.String(), id+"  codex") || strings.Contains(out.String(), "EXECUTOR") {
		t.Errorf("table: %q %v", out.String(), err)
	}
	out.Reset()
	c.Sessions = append(c.Sessions, catalogueRow{ID: id, Executor: "engineering", Phase: "home-owned"})
	if err := writeCatalogueTable(&out, c); err != nil || !strings.Contains(out.String(), "EXECUTOR") {
		t.Errorf("table from two catalogues: %q %v", out.String(), err)
	}
}

// attach without a terminal fails with its reason before contacting home.
func TestAttachNeedsATerminal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	socket := filepath.Join(testutil.PrivateDir(t), "missing.sock")
	if code := cli([]string{"attach", uuid.NewString(), "--socket", socket}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), errNoTerminal.Error()) {
		t.Fatalf("exited %d: %q", code, stderr.String())
	}
}

// A local installation's home is started with the service, not serve.
func TestMissingLocalHomeSuggestsTheService(t *testing.T) {
	root := filepath.Join(testutil.PrivateDir(t), "installation")
	if err := privatefs.EnsureDir(root); err != nil {
		t.Fatal(err)
	}
	if err := privatefs.Replace(root, "installation.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.json")
	if got := startHomeCommand(config); got != "sproozi service start --root "+shellQuote(root) {
		t.Fatalf("local installation: %s", got)
	}
	if got := privateHint(home.ErrControlUnavailable, config).Error(); !strings.HasSuffix(got,
		`start home with "sproozi service start --root `+shellQuote(root)+`"`) {
		t.Fatalf("hint: %s", got)
	}
	manual := filepath.Join(testutil.PrivateDir(t), "config.json")
	if got := startHomeCommand(manual); !strings.HasPrefix(got, "sproozi serve") {
		t.Fatalf("manual configuration: %s", got)
	}
}
