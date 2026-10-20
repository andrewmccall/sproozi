//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/google/uuid"
)

// writeRemoteConfig writes a client configuration with two valid remotes.
func writeRemoteConfig(t *testing.T, path, dir string) {
	t.Helper()
	remote := `{"url":"https://profile.example:8443","tokenFile":` + strconv.Quote(filepath.Join(dir, "token")) +
		`,"maxCheckpointBytes":8388608}`
	data := `{"directory":` + strconv.Quote(dir) + `, "model":"fixture", "local":{"Name":"local"},
 "allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}, "maxDuration":"1h",
 "remote":{"engineering":` + remote + `, "shared":` + remote + `}}`
	if err := testutil.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalCommandsUseSharedSessionAddressing(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		action string
		flags  []string
	}{
		{home.ActionStart, []string{"--workspace", testutil.PrivateDir(t)}},
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
	dir := testutil.PrivateDir(t)
	config := filepath.Join(dir, "config.json")
	writeRemoteConfig(t, config, dir)
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
	dir := testutil.PrivateDir(t)
	socket := home.SocketPath(dir)
	l, err := listenTestControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
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
		select {
		case e := <-done:
			t.Logf("DIAG fake home finished with: %v", e)
		case <-time.After(2 * time.Second):
			t.Logf("DIAG fake home never finished (no connection accepted)")
		}
		t.Logf("DIAG socket=%q", socket)
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
	dir := testutil.PrivateDir(t)
	config := filepath.Join(dir, "config.json")
	writeRemoteConfig(t, config, dir)
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
	dir := testutil.PrivateDir(t)
	config := filepath.Join(dir, "config.json")
	writeRemoteConfig(t, config, dir)
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

func TestSessionUUIDMayPrecedeOrFollowFlags(t *testing.T) {
	id := uuid.NewString()
	for _, action := range []string{home.ActionStatus, home.ActionStop, home.ActionResume, home.ActionCancel,
		home.ActionRecover, home.ActionAttach} {
		for _, args := range [][]string{
			{action, id, "--executor", "docker", testSocketFlag, "/unused/private/socket"},
			{action, "--executor", "docker", id, testSocketFlag, "/unused/private/socket"},
			{action, "--executor", "docker", testSocketFlag, "/unused/private/socket", id},
		} {
			q, err := parseProduct(args)
			if err != nil || q.id != id {
				t.Fatalf("%v: %+v %v", args, q, err)
			}
		}
		for _, args := range [][]string{
			{action, "--executor", "docker", id, id, testSocketFlag, "/unused/private/socket"},
			{action, id, "--executor", "docker", id, testSocketFlag, "/unused/private/socket"},
		} {
			if _, err := parseProduct(args); err == nil || !strings.Contains(err.Error(), "unexpected argument \""+id+"\"") {
				t.Fatalf("%v: two session UUIDs accepted: %v", args, err)
			}
		}
		args := []string{action, "--executor", "docker", testSocketFlag, "/unused/private/socket", id, "--id", id}
		if _, err := parseProduct(args); err == nil || !strings.Contains(err.Error(), "positional session UUID or --id") {
			t.Fatalf("%v: ambiguous session addressing accepted: %v", args, err)
		}
	}
	if _, err := parseProduct([]string{home.ActionSessions, testSocketFlag, "/unused/private/socket", id}); err == nil ||
		!strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("sessions accepted a session UUID: %v", err)
	}
}

func TestNonCanonicalUUIDsNameTheArgumentAndFormat(t *testing.T) {
	const rule = "use a non-zero UUID in lowercase 8-4-4-4-12 hexadecimal form, " +
		"such as 3f2b8c1e-7d4a-4e5b-9c6d-0a1b2c3d4e5f"
	stop := "stop " + uuid.NewString() + " --operation 1 --socket /unused/private/socket"
	for args, want := range map[string]string{
		"status 1 --socket /unused/private/socket": `session UUID "1" is not a canonical UUID: ` + rule,
		stop: `--operation "1" is not a canonical UUID: ` + rule,
	} {
		if _, err := parseProduct(strings.Fields(args)); err == nil || err.Error() != want {
			t.Fatalf("%s: %v, want %q", args, err, want)
		}
	}
}

// A session home does not retain is reported as such, without a retry hint.
func TestUnknownHomeSessionHasNoRetryHint(t *testing.T) {
	dir := testutil.PrivateDir(t)
	socket := home.SocketPath(dir)
	l, err := listenTestControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	id := uuid.NewString()
	want := "docker (home): no session " + id + `; run "sproozi sessions" to list sessions`
	// Like home, answer every connection: a Windows named pipe has no backlog
	// for a dial made while another connection is being served.
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			if json.NewDecoder(c).Decode(new(home.Request)) == nil {
				_ = json.NewEncoder(c).Encode(home.Response{Error: want, UnknownSession: true})
			}
			_ = c.Close()
		}
	}()
	var stdout, stderr bytes.Buffer
	if code := cli([]string{"stop", id, "--executor", "docker", testSocketFlag, socket}, &stdout, &stderr); code != 1 ||
		stderr.String() != "Sproozi failed: "+want+"\n" {
		t.Fatalf("stop of an unknown session exited %d: %q", code, stderr.String())
	}
}

// A move home rejected before recording it is reported without a retry hint,
// even when no configuration was read to check the destination first.
func TestRejectedHomeMoveHasNoRetryHint(t *testing.T) {
	dir := testutil.PrivateDir(t)
	socket := home.SocketPath(dir)
	l, err := listenTestControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	want := `home: move destination "nowhere" is not a configured placement; use one of: local`
	// Like home, answer every connection: a Windows named pipe has no backlog
	// for a second dial while the first is being served.
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			if json.NewDecoder(c).Decode(new(home.Request)) == nil {
				_ = json.NewEncoder(c).Encode(home.Response{Error: want, Rejected: true,
					Status: &home.Status{ID: uuid.NewString()}})
			}
			_ = c.Close()
		}
	}()
	var stdout, stderr bytes.Buffer
	args := []string{"move", uuid.NewString(), "--to", "nowhere", testSocketFlag, socket}
	if code := cli(args, &stdout, &stderr); code != 1 || stdout.Len() != 0 ||
		stderr.String() != "Sproozi failed: "+want+"\n" {
		t.Fatalf("rejected move exited %d: %q %q", code, stdout.String(), stderr.String())
	}
}

// Pull's --from takes a remote executor name, and its errors say so.
func TestPullFromNamesARemoteExecutor(t *testing.T) {
	dir := testutil.PrivateDir(t)
	config := filepath.Join(dir, "config.json")
	writeRemoteConfig(t, config, dir)
	id := uuid.NewString()
	for args, want := range map[string]string{
		"pull " + id + " --config " + config: `pull requires --from NAME naming the remote executor to pull from, ` +
			`configured under "remote" or "workSources" in ` + config,
		"pull " + id + " --config " + config + " --from " + sharedOwner + " --executor " + testExecutor: "pull --from " +
			sharedOwner + " conflicts with --executor " + testExecutor +
			"; name the remote executor once, with --from or --executor",
		"pull " + id + " --config " + config + " --from elsewhere": `pull --from "elsewhere" names no remote executor ` +
			`configured under "remote" or "workSources" in ` + config + "; see " + remoteSetupDoc,
	} {
		if _, err := parseProduct(strings.Fields(args)); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
}
