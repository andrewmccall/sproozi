package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		err := run([]string{action, uuid.NewString(), "--executor", testExecutor, "--config", "/missing/config"})
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
			e = json.NewEncoder(c).Encode(home.Response{Sessions: []home.Status{{
				ID: id, ExecutionID: execution, Ownership: testHomeOwnership, Phase: "active",
			}}})
		}
		done <- e
	}()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	err = run([]string{"sessions", testSocketFlag, socket})
	os.Stdout = original
	_ = w.Close()
	defer func() { _ = r.Close() }()
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(r)
	var out catalogue
	if err = decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].ID != id || out.Sessions[0].ExecutionID != execution ||
		out.Sessions[0].Executor != dockerExecutor || out.Sessions[0].Ownership != testHomeOwnership ||
		len(out.Executors) != 1 || out.Executors[0].Availability != testAvailable {
		t.Fatalf("unlabelled catalogue: %+v", out)
	}
	if decoder.Decode(new(any)) == nil {
		t.Fatal("multiple top-level JSON values")
	}
}
