//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"reflect"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

func projectFixture(t *testing.T) (Config, *fixturePlacement) {
	t.Helper()
	c, p := fixture(t)
	return withProjects(c), p
}

// withProjects adds the project definitions to an owner configuration.
func withProjects(c Config) Config {
	c.TemplateName = "portable-codex"
	c.Allowances["github.pull_request"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 30}}
	c.Allowances["kubernetes.read"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 50}}
	r := c.Repositories[repositorySproozi]
	r.Reference = "urn:sproozi:fixture"
	c.Repositories[repositorySproozi] = r
	c.Projects = map[project.ID]project.Definition{
		"sproozi-dev": {Template: "portable-codex", Workspace: "sproozi", Members: []string{"hermes"},
			Capabilities: []string{"model.inference", "github.pull_request"}},
		"home-ops": {Template: "portable-codex", Members: []string{"hermes"},
			Capabilities: []string{"model.inference", "kubernetes.read"}},
		"private": {Template: "portable-codex", Members: []string{"another-installation"},
			Capabilities: []string{"model.inference"}},
	}
	return c
}

func projectRequest(id project.ID) Submit {
	return Submit{RequestID: uuid.NewString(), Project: id, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Task: fixtureTask}
}

func TestProjectsGroupSessionsAndNarrowActualAdmission(t *testing.T) {
	t.Parallel()
	c, p := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	q := projectRequest("sproozi-dev")
	engineering, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, engineering.ID, phaseRetained)
	q.Project = "home-ops"
	if _, err = o.Submit(t.Context(), q); err == nil {
		t.Fatal("Project change replayed an existing operation")
	}
	ops, err := o.Submit(t.Context(), projectRequest("home-ops"))
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, ops.ID, phaseRetained)
	standalone := submit(t, o)
	waitPhase(t, o, standalone.ID, phaseRetained)
	for id, expected := range map[project.ID]string{"sproozi-dev": engineering.ID, "home-ops": ops.ID} {
		list, listErr := o.List(t.Context(), SessionQuery{Project: id})
		if listErr != nil || len(list) != 1 || list[0].ID != expected || list[0].Project != id {
			t.Fatalf("Project %s listing: %#v %v", id, list, listErr)
		}
	}
	all, err := o.List(t.Context(), SessionQuery{})
	if err != nil || len(all) != 3 || standalone.Project != "" || standalone.Repository != "sproozi" {
		t.Fatalf("Standalone sessions were lost: %#v %v", all, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.launches) != 3 {
		t.Fatalf("Unexpected admitted work: %#v", p.launches)
	}
	expected := map[string]session.Allowance{
		"model.inference":     {Units: session.Limit{Bounded: true, Value: 100}},
		"github.pull_request": {Units: session.Limit{Bounded: true, Value: 30}},
	}
	if !reflect.DeepEqual(p.launches[0].Allowances, expected) {
		t.Fatalf("Project widened actual launch: %#v", p.launches[0].Allowances)
	}
}

func TestProjectAccessGatesSubmitAndEveryRetainedRead(t *testing.T) {
	t.Parallel()
	c, p := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.Submit(t.Context(), projectRequest("private")); err == nil {
		t.Fatal("A nonmember submitted work")
	}
	q := projectRequest("sproozi-dev")
	v, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	d := c.Projects["sproozi-dev"]
	d.Members = []string{"another-installation"}
	c.Projects["sproozi-dev"] = d
	o, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	if leaked, infoErr := o.Info(t.Context(), v.ID); OutcomeCode(infoErr) != profile.CodeDenied || leaked.ID != "" {
		t.Fatalf("Revoked membership revealed retained work: %#v %v", leaked, infoErr)
	}
	if _, err = o.Submit(t.Context(), q); OutcomeCode(err) != profile.CodeDenied {
		t.Fatal("Submission replay bypassed membership")
	}
	if _, err = o.BeginExport(t.Context(), v.ID, uuid.NewString()); OutcomeCode(err) != profile.CodeDenied {
		t.Fatal("Export bypassed membership")
	}
	if _, err = o.Recover(t.Context(), v.ID); OutcomeCode(err) != profile.CodeDenied {
		t.Fatal("Recovery exposed revoked project")
	}
	if _, err = o.Cancel(t.Context(), v.ID, uuid.NewString()); OutcomeCode(err) != profile.CodeDenied {
		t.Fatal("Cancellation exposed revoked project")
	}
	list, err := o.List(t.Context(), SessionQuery{})
	if err != nil || len(list) != 0 {
		t.Fatalf("Unfiltered catalogue bypassed membership: %#v %v", list, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initial != 1 {
		t.Fatal("Denied project or replay launched a writer")
	}
}

func TestRevokedProjectStillFencesUnfinishedWriterAfterRestart(t *testing.T) {
	c, p := projectFixture(t)
	p.lostInitial = true
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseUnknown)
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	delete(c.Projects, "sproozi-dev")
	o, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	// Public access is revoked, but owner cleanup must independently finish.
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, readErr := o.read(v.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if r.Phase == phaseCancelled {
			if r.Stopped == nil || !r.Stopped.ActualStopped || r.Stopped.Receipt == nil ||
				r.Incarnation == nil || r.Incarnation.ResourceUID != "exact-resource" {
				t.Fatalf("Revocation lost fencing/accounting: %#v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Revoked writer remains %s", r.Phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = o.Info(t.Context(), v.ID); err == nil {
		t.Fatal("Cleanup restored user access")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != 0 || p.initial != 1 {
		t.Fatal("Revocation activated or re-prepared work")
	}
}

func TestConfiguredTemplateCannotRebindRetainedAssociation(t *testing.T) {
	t.Parallel()
	c, _ := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	c.TemplateName = "different-template"
	for id, definition := range c.Projects {
		definition.Template = c.TemplateName
		c.Projects[id] = definition
	}
	if rebound, openErr := Open(c); openErr == nil {
		_ = rebound.Close()
		t.Fatal("Retained catalogue rebound to a different template")
	}
}

func TestInvalidProjectConfigurationCannotStartAnOwner(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"template", "repository", "capability", "members", "identity", "credential"} {
		t.Run(invalid, func(t *testing.T) {
			// Validation rejects each configuration before any Git runs, so
			// no snapshot repository is built.
			p := &fixturePlacement{}
			c := withProjects(fixtureConfig(t, t.TempDir(), p))
			d := c.Projects["sproozi-dev"]
			switch invalid {
			case "template":
				d.Template = "different-template"
			case "repository":
				d.Workspace = "unknown"
			case "capability":
				d.Capabilities = []string{"model.inference", "unapproved"}
			case "members":
				d.Members = nil
			case "identity":
				r := c.Repositories["sproozi"]
				r.Reference = ""
				c.Repositories["sproozi"] = r
			case "credential":
				r := c.Repositories["sproozi"]
				r.Reference = "https://user:secret@example.invalid/repo"
				c.Repositories["sproozi"] = r
			}
			c.Projects["sproozi-dev"] = d
			if o, err := Open(c); err == nil {
				_ = o.Close()
				t.Fatal("Owner accepted invalid project configuration")
			}
			if p.initial != 0 {
				t.Fatal("Invalid configuration prepared a writer")
			}
		})
	}
}
