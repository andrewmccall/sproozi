package work

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/andrewmccall/sproozi/internal/workspace"
)

// Most tests need a session that has started, not a start of one: Owner.start
// captures the repository, about seventeen Git processes, and gives the
// coordinator the first execution with that capture as its input. Every
// fixture repository is a copy of the template, so that capture is the same
// bytes every time. submitRetained keeps the driver off a new session, has the
// coordinator retain the first execution from one capture of the template
// taken once per test binary, and lets the driver go. The driver then finds
// the execution and takes every step after the capture, as it does in
// production once start returns. Tests of the capture or of start itself call
// Submit and let the driver start the session.
var retained struct {
	once  sync.Once
	input []byte
	sum   [sha256.Size]byte
	tree  map[string]string
	err   error
}

// treeFiles is the content of every file, and the name of every directory,
// under root, so a repository can be compared with the template it was copied
// from. Modes are left out: the copy does not keep read-only objects.
func treeFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			files[rel] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		files[rel] = string(data)
		return err
	})
	return files, err
}

// retainedInput is a copy of the capture of the unmodified template
// repository.
func retainedInput(t *testing.T) []byte {
	t.Helper()
	template.once.Do(func() { template.err = buildTemplate() })
	if template.err != nil {
		t.Fatalf("build fixture template: %v", template.err)
	}
	retained.once.Do(func() {
		if retained.tree, retained.err = treeFiles(template.repo); retained.err != nil {
			return
		}
		retained.input, retained.err = workspace.Capture(context.Background(), template.repo, nil, testLimit)
		retained.sum = sha256.Sum256(retained.input)
	})
	if retained.err != nil {
		t.Fatalf("capture the template repository: %v", retained.err)
	}
	return bytes.Clone(retained.input)
}

// checkRetained fails the package if a test changed the template repository
// or the capture of it that sessions retain.
func checkRetained() error {
	if retained.input == nil {
		return nil
	}
	if sha256.Sum256(retained.input) != retained.sum {
		return fmt.Errorf("a test changed the captured template workspace")
	}
	tree, err := treeFiles(template.repo)
	if err != nil || !maps.Equal(tree, retained.tree) {
		return fmt.Errorf("a test changed the template repository %s (%v)", template.repo, err)
	}
	return nil
}

// submitRetained is o.SubmitAs for a session on a snapshot repository that
// is a copy of the template. Its first execution is retained from the
// template's capture before the driver runs; see retained. A submission
// that does not create a session returns as SubmitAs does.
func submitRetained(t *testing.T, o *Owner, caller Caller, q Submit) (View, error) {
	t.Helper()
	id := o.sessionID(q.RequestID)
	if _, err := o.read(id); err == nil {
		return submitAs(t, o, caller, q)
	}
	hold := &driver{stop: func() {}, wake: make(chan struct{}, 1)}
	o.mu.Lock()
	if o.running[id] != nil {
		o.mu.Unlock()
		t.Fatalf("a driver already runs session %s", id)
	}
	o.running[id] = hold
	o.mu.Unlock()
	release := func() {
		o.mu.Lock()
		delete(o.running, id)
		o.mu.Unlock()
	}
	v, err := submitAs(t, o, caller, q)
	if err != nil {
		release()
		return v, err
	}
	r, err := o.read(id)
	if err != nil {
		t.Fatal(err)
	}
	repository := o.config.Repositories[r.Intent.Repository]
	if r.RepositorySource != nil {
		repository = *r.RepositorySource
	}
	if r.Intent.WorkspaceFrom != nil || r.Intent.WorkspaceDigest != "" || repository.Snapshot == "" {
		t.Fatalf("submitRetained needs a snapshot repository and no source: %+v", r.Intent)
	}
	input := retainedInput(t)
	tree, treeErr := treeFiles(repository.Snapshot)
	if treeErr != nil || !maps.Equal(tree, retained.tree) {
		t.Fatalf("snapshot %s is not the template repository (%v, differs in %v); submit it with Submit", repository.Snapshot, treeErr, differing(tree, retained.tree))
	}
	unlock := o.jobs.Lock(id)
	_, err = o.config.Coordinator.Create(r.First, input, "")
	unlock()
	if err != nil {
		t.Fatalf("retain the first execution of session %s: %v", id, err)
	}
	release()
	o.dispatch(id)
	return v, nil
}

// differing names the files on which a and b disagree.
func differing(a, b map[string]string) []string {
	var names []string
	for name, v := range a {
		if w, ok := b[name]; !ok || v != w {
			names = append(names, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// submitAs is o.SubmitAs, repeated while its retention check fails because the
// driver of another session replaced a journal file in the directory the check
// was walking. Such a file vanishes between the walk reading the directory and
// examining it, which Retained and admitRetention report as an error. A
// submission that failed so saved nothing, so repeating it is the same
// submission. Without the shortcut, a driver reaches its first journal write
// only after capturing the repository, and the walk almost never meets it.
func submitAs(t *testing.T, o *Owner, caller Caller, q Submit) (View, error) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		v, err := o.SubmitAs(t.Context(), caller, q)
		if err == nil || attempt == 20 || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), ".pending-") {
			return v, err
		}
	}
}

// A session whose first execution submitRetained retained starts from what
// Start's own capture of the same repository gives it: the same manifest, but
// for the bundle's digest, and the same working files. The bundle itself holds
// a commit of the index that Capture makes at the time it runs, so no two
// captures are the same bytes. Both sessions take the launch Submit built to
// the placement.
func TestRetainedInputIsWhatStartCaptures(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	captured, err := o.Submit(t.Context(), taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	shortcut, err := submitRetained(t, o, CallerHome, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, captured.ID, phaseRetained)
	waitPhase(t, o, shortcut.ID, phaseRetained)
	want, err := c.Coordinator.Workspace(captured.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Coordinator.Workspace(shortcut.ID)
	if err != nil {
		t.Fatal(err)
	}
	if differs := archiveDifferences(t, got, want); len(differs) != 0 {
		t.Fatalf("retained input differs from Start's capture in %v", differs)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.launches) != 2 {
		t.Fatalf("%d launches, want 2", len(p.launches))
	}
	for i, l := range p.launches {
		if differs := archiveDifferences(t, p.inputs[l.AllocationID], want); len(differs) != 0 {
			t.Fatalf("launch %d prepared with an input that differs in %v", i, differs)
		}
	}
}

// archiveDifferences names the members of the tar archives a and b that
// differ, leaving out the bundle, whose digest the manifest records.
func archiveDifferences(t *testing.T, a, b []byte) []string {
	t.Helper()
	read := func(data []byte) map[string]string {
		members := map[string]string{}
		r := tar.NewReader(bytes.NewReader(data))
		for {
			h, err := r.Next()
			if err == io.EOF {
				return members
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			switch h.Name {
			case "repository.bundle":
				body = nil
			case "manifest.json":
				var manifest map[string]any
				if err = json.Unmarshal(body, &manifest); err != nil {
					t.Fatal(err)
				}
				delete(manifest, "bundle")
				if body, err = json.Marshal(manifest); err != nil {
					t.Fatal(err)
				}
			}
			members[h.Name] = string(body)
		}
	}
	x, y := read(a), read(b)
	var names []string
	for name, v := range x {
		if w, ok := y[name]; !ok || v != w {
			names = append(names, name)
		}
	}
	for name := range y {
		if _, ok := x[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
