//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/google/uuid"
)

func TestLocalCommandsUseSharedSessionAddressing(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		action string
		flags  []string
	}{
		{home.ActionStart, []string{"--workspace", t.TempDir()}},
		{home.ActionAttach, nil}, {home.ActionMove, []string{"--to", testExecutor}},
	} {
		args := append([]string{tc.action, id, testSocketFlag, "/unused/private/socket"}, tc.flags...)
		q, err := parseProduct(args)
		if err != nil || q.id != id || q.socket != "/unused/private/socket" {
			t.Fatalf("%s: %+v %v", tc.action, q, err)
		}
		if _, err = parseProduct(append(args, "--id", id)); err == nil {
			t.Fatal("ambiguous session addressing accepted")
		}
	}
}

func TestRemoteTargetsRejectUnsupportedControlsBeforeConfiguration(t *testing.T) {
	for _, action := range []string{"stop", "resume", home.ActionStart, home.ActionAttach, home.ActionMove} {
		err := run([]string{action, uuid.NewString(), "--executor", testExecutor, "--config", "/missing/config"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "requires the local home owner") {
			t.Fatalf("%s used configuration before rejecting target: %v", action, err)
		}
	}
}

func TestLocalCommandsReadExplicitConfigurationWithSocketOverride(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	data := `{"directory":` + `"` + dir + `", "model":"fixture", "local":{"Name":"docker"},
 "allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}, "maxDuration":"1h"}`
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		action string
		flags  []string
	}{
		{home.ActionStart, []string{"--workspace", dir}},
		{home.ActionAttach, nil},
		{home.ActionMove, []string{"--to", testExecutor}},
	} {
		args := []string{tc.action, uuid.NewString(), "--config", config, testSocketFlag, "/override.sock"}
		q, err := parseProduct(append(args, tc.flags...))
		if err != nil || q.fixed.Directory != dir || q.socket != "/override.sock" {
			t.Fatalf("%s: %+v %v", tc.action, q, err)
		}
	}
}

func TestSessionsWritesOneLabelledCatalogue(t *testing.T) {
	dir, err := os.MkdirTemp("", "sproozi-catalogue-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err = os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	id, execution := uuid.NewString(), uuid.NewString()
	done := make(chan error, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			done <- e
			return
		}
		defer func() { _ = c.Close() }()
		var request home.Request
		if e = json.NewDecoder(c).Decode(&request); e == nil {
			if request.Project != "sproozi-dev" {
				t.Errorf("project filter lost: %+v", request)
			}
			e = json.NewEncoder(c).Encode(home.Response{Sessions: []home.Status{{
				Association: project.Association{Project: "sproozi-dev", Repository: "sproozi"},
				ID:          id, ExecutionID: execution, Ownership: testHomeOwnership, Phase: "active",
			}}})
		}
		done <- e
	}()
	var stdout bytes.Buffer
	if err = run([]string{"sessions", testSocketFlag, socket, "--project", "sproozi-dev"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&stdout)
	var out catalogue
	if err = decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].ID != id || out.Sessions[0].ExecutionID != execution ||
		out.Sessions[0].Project != "sproozi-dev" || out.Sessions[0].Repository != "sproozi" ||
		out.Sessions[0].Executor != dockerExecutor || out.Sessions[0].Ownership != testHomeOwnership ||
		len(out.Executors) != 1 || out.Executors[0].Availability != testAvailable {
		t.Fatalf("unlabelled catalogue: %+v", out)
	}
	if decoder.Decode(new(any)) == nil {
		t.Fatal("multiple top-level JSON values")
	}
}

func TestProjectAndRevisionOptionsUseSharedAddressing(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	data := `{"directory":"` + dir + `", "model":"fixture", "local":{"Name":"docker"},
 "allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}, "maxDuration":"1h"}`
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	source := uuid.NewString()
	q, err := parseProduct([]string{"run", "--executor", testExecutor, "--config", config,
		"--project", "sproozi-dev", "--workspace-from", source,
		"--checkpoint-digest", strings.Repeat("a", 64), "--revision", strings.Repeat("b", 40)})
	if err != nil || q.project != "sproozi-dev" || q.workspaceFrom == nil || q.workspaceFrom.SessionID != source ||
		q.workspaceFrom.Digest != strings.Repeat("a", 64) || q.workspaceFrom.Revision != strings.Repeat("b", 40) {
		t.Fatalf("Project/revision run intent lost: %+v %v", q, err)
	}
	for _, args := range [][]string{
		{"status", source, testSocketFlag, "/unused", "--project", "sproozi-dev"},
		{"sessions", testSocketFlag, "/unused", "--project", "Invalid"},
		{"run", "--executor", testExecutor, "--config", config, "--workspace-from", source},
		{"sessions", testSocketFlag, "/unused", "--workspace-from", source,
			"--checkpoint-digest", "digest", "--revision", "revision"},
	} {
		if _, err = parseProduct(args); err == nil {
			t.Fatalf("Unsupported Project/revision intent accepted: %v", args)
		}
	}
}

func TestPlainPullRetryRepeatsTheSameOperation(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	data := `{"directory":"` + dir + `", "model":"fixture", "local":{"Name":"docker"},
 "allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}, "maxDuration":"1h"}`
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	parse := func(args ...string) options {
		t.Helper()
		q, err := parseProduct(append([]string{"pull", id, "--config", config}, args...))
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	first, retry := parse("--from", testExecutor), parse("--executor", testExecutor)
	if first.source != testExecutor || retry.source != testExecutor ||
		first.operation == "" || retry.operation != first.operation {
		t.Fatalf("Pull retry changed intent: %+v %+v", first, retry)
	}
	if other := parse("--from", sharedOwner); other.operation == first.operation {
		t.Fatal("Another source reused the pull operation")
	}
	explicit := uuid.NewString()
	if q := parse("--from", testExecutor, "--operation", explicit); q.operation != explicit {
		t.Fatalf("Explicit pull operation replaced: %s", q.operation)
	}
	if _, err := parseProduct([]string{"pull", id, "--config", config}); err == nil {
		t.Fatal("Pull without a source accepted")
	}
}
