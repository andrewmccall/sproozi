package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

const testPathEnvironment = "PATH=/usr/bin:/bin"

const selected = "01234567-89ab-cdef-0123-456789abcdef"
const instructions = "Only the isolated fixture is approved."

func digest() string { return fmt.Sprintf("%x", sha256.Sum256([]byte(instructions))) }
func journal(extra string) string {
	return `{"type":"session_meta","payload":{"id":"` + selected + `","cli_version":"0.161.0",` +
		`"cwd":"/workspace/session/workspace"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[` +
		`{"type":"input_text","text":"` + instructions + `"}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-one"}}` + "\n" +
		extra + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-one"}}` + "\n"
}
func writeJournal(t *testing.T, data string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "11")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-11T12-00-00-"+selected+".jsonl"),
		[]byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}
func TestCompletedTurnObserver(t *testing.T) {
	home := writeJournal(t, journal(""))
	id, turn, err := observeTurn(home, selected, digest(), 8192)
	if err != nil || id != selected || turn != "turn-one" {
		t.Fatalf("selected completed turn: id=%s turn=%s error=%v", id, turn, err)
	}
	id, turn, err = observeTurn(home, "", digest(), 8192)
	if err != nil || id != selected || turn != "turn-one" {
		t.Fatalf("new native selection: %s %s %v", id, turn, err)
	}
	for _, tc := range []struct{ name, data, hash string }{
		{"pending tool", journal(`{"type":"response_item","payload":{"type":"function_call","call_id":"tool-one"}}
`), digest()},
		{"pending exec", journal(`{"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"exec-one"}}
`), digest()},
		{"new turn", journal("") + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-two"}}
`, digest()},
		{"different instructions", journal(""), strings.Repeat("0", 64)},
		{"partial row", strings.TrimSuffix(journal(""), "\n"), digest()},
		{"unknown tool", journal(`{"type":"response_item","payload":{"type":"new_tool_call","call_id":"unknown"}}
`), digest()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := observeTurn(writeJournal(t, tc.data), selected, tc.hash, 8192); err == nil {
				t.Fatal("unsafe native observation accepted")
			}
		})
	}
	resolved := journal(`{"type":"response_item","payload":{"type":"function_call","call_id":"tool-one"}}
{"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"exec-one"}}
{"type":"event_msg","payload":{"type":"exec_command_end","call_id":"exec-one"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"tool-one"}}
`)
	if _, _, err := observeTurn(writeJournal(t, resolved), selected, digest(), 8192); err != nil {
		t.Fatal(err)
	}
	if _, _, err := observeTurn(home, selected, digest(), 32); err == nil {
		t.Fatal("oversized journal accepted")
	}
}
func TestTrustedContract(t *testing.T) {
	c := config{Harness: harness.NativeCodex, Deadline: time.Now().Add(time.Minute), Instructions: instructions,
		InstructionDigest: digest(),
		Env:               []string{testPathEnvironment}, RolloutMaxBytes: 8192}
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	c.Env = append(c.Env, "CODEX_HOME=/untrusted")
	if validateConfig(c) == nil {
		t.Fatal("caller override of native home accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfig(path); err == nil {
		t.Fatal("writable launch contract accepted")
	}
}

func TestNativeLaunchRejectsReservedAndUnboundedOptions(t *testing.T) {
	c := config{Harness: harness.NativeCodex, Deadline: time.Now().Add(time.Minute), Instructions: instructions,
		InstructionDigest: digest(),
		Env:               []string{testPathEnvironment}, RolloutMaxBytes: 8192}
	c.Options = []string{`projects."/workspace/session/workspace".trust_level="trusted"`}
	if err := validateConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]string{
		{`"developer_instructions"="other"`}, {` developer_instructions="other"`},
		{`=true`}, {strings.Repeat("x", 4097)}, make([]string, 33),
	} {
		c.Options = options
		if err := validateConfig(c); err == nil {
			t.Fatal("unsafe native launch contract accepted")
		}
	}
}
func TestFixedAttachmentReconnectAndGate(t *testing.T) {
	native, err := nativepty.Start(nativepty.Launch{Executable: "/bin/sh",
		Args: []string{"-c", "stty -echo; while IFS= read -r line; do printf 'SEEN:%s\\n' \"$line\"; done"},
		Env:  []string{testPathEnvironment, "TERM=xterm"}, Dir: t.TempDir(),
		Deadline: time.Now().Add(15 * time.Second), Size: nativepty.Size{Rows: 24, Columns: 80}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = native.Stop(ctx)
	})
	s := &server{native: native, state: stateActive, done: make(chan struct{}),
		config:   config{Harness: harness.NativeCodex, Deadline: time.Now().Add(15 * time.Second)},
		deadline: time.Now().Add(15 * time.Second)}
	connect := func() (net.Conn, *json.Decoder) {
		client, backend := net.Pipe()
		go s.handle(backend)
		_ = client.SetDeadline(time.Now().Add(3 * time.Second))
		if err := json.NewEncoder(client).Encode(request{Op: opAttach}); err != nil {
			t.Fatal(err)
		}
		d := json.NewDecoder(bufio.NewReader(client))
		var r response
		if err := d.Decode(&r); err != nil || r.Error != "" || r.PID != native.PID() {
			t.Fatalf("attach: %+v %v", r, err)
		}
		return client, d
	}
	client, d := connect()
	if err := json.NewEncoder(client).Encode(request{Op: opWrite, Data: []byte("first\r")}); err != nil {
		t.Fatal(err)
	}
	var got string
	for !strings.Contains(got, "SEEN:first") {
		var r response
		if err := d.Decode(&r); err != nil {
			t.Fatal(err)
		}
		got += string(r.Data)
	}
	_ = client.Close()
	until := time.Now().Add(time.Second)
	for {
		a, err := native.Attach(0)
		if err == nil {
			_ = a.Close()
			break
		}
		if time.Now().After(until) {
			t.Fatal("disconnect retained input lease")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, d = connect()
	defer func() { _ = client.Close() }()
	var replay response
	if err := d.Decode(&replay); err != nil || !strings.Contains(string(replay.Data), "SEEN:first") {
		t.Fatalf("reconnect replay: %+v %v", replay, err)
	}
	native.GateInput()
	if err := json.NewEncoder(client).Encode(request{Op: opWrite, Data: []byte("rejected\r")}); err != nil {
		t.Fatal(err)
	}
	var gated response
	if err := d.Decode(&gated); err != nil || gated.Written != 0 || !strings.Contains(gated.Error, "gated") {
		t.Fatalf("gated attachment: %+v %v", gated, err)
	}
	select {
	case <-native.Done():
		t.Fatal("attachment gate stopped active native process")
	default:
	}
	if r := s.activate(); r.Error != "" || r.PID != native.PID() {
		t.Fatalf("activate replay started new process: %+v", r)
	}
	s.mu.Lock()
	s.state = stateStopping
	s.mu.Unlock()
	if r := s.activate(); r.Error == "" {
		t.Fatal("launch gate reopened after move")
	}
}

// The root drives loopback fixture execution. This test consumes its real native
// artifact read-only so the command observer is checked against stock TUI output.
var tuiProofReport = flag.String("tui-proof-report", "", "optional production TUI report")

func TestRecordedStockTUIObserver(t *testing.T) {
	if *tuiProofReport == "" {
		t.Skip("root-owned stock TUI evidence not supplied")
	}
	data, err := os.ReadFile(*tuiProofReport)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Passed          bool   `json:"passed"`
		Version         string `json:"version"`
		ThreadID        string `json:"nativeThreadId"`
		PrivateEvidence string `json:"privateEvidenceDirectory"`
	}
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Version != "codex-cli 0.161.0" {
		t.Fatal("stock TUI proof did not pass at supported pin")
	}
	home := filepath.Join(filepath.Dir(report.PrivateEvidence), "0", "home", ".codex")
	approved := "Only the deterministic TUI fixture is approved. TUI_INITIAL_APPROVED_111"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(approved)))
	id, turn, err := observeTurn(home, report.ThreadID, hash, 8<<20)
	if err != nil || id != report.ThreadID || turn == "" {
		t.Fatalf("real native completed-turn observation: %s %s %v", id, turn, err)
	}
}

func TestBootstrapConfigurationIsImmutable(t *testing.T) {
	deadline := time.Now().UTC().Add(time.Minute)
	s := &server{config: config{Harness: harness.NativeCodex, Deadline: deadline}, deadline: deadline,
		state: stateUnconfigured, done: make(chan struct{})}
	approved := config{Harness: harness.NativeCodex, Deadline: deadline, Instructions: instructions,
		InstructionDigest: digest(),
		Env:               []string{}, RolloutMaxBytes: 8192}
	if r := s.configure(&approved); r.Error != "" || r.State != statePrepared {
		t.Fatalf("configure: %+v", r)
	}
	if r := s.configure(&approved); r.Error != "" {
		t.Fatalf("configure replay: %+v", r)
	}
	changed := approved
	changed.Model = "another-model"
	if r := s.configure(&changed); r.Error == "" {
		t.Fatal("mutable configured model accepted")
	}
	changed = approved
	changed.Deadline = deadline.Add(time.Hour)
	if r := s.configure(&changed); r.Error == "" {
		t.Fatal("bootstrap deadline extended")
	}
}
