//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

const testSocketFlag = "--socket"

// runCLI runs one command line and returns its exit status and output.
func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// writeTestConfig writes the smallest configuration commands accept, as an
// installation when installed is set, with the named clusters.
func writeTestConfig(t *testing.T, root string, installed bool) string {
	t.Helper()
	if err := privatefs.EnsureDir(root); err != nil {
		t.Fatal(err)
	}
	data := `{"directory":` + strconv.Quote(filepath.Join(root, "state")) + `,
 "local":{"harness":"codex","name":"local"}, "maxDuration":"30m",
 "allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}}`
	path := filepath.Join(root, "config.json")
	if err := testutil.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if installed {
		if err := testutil.WriteFile(filepath.Join(root, "installation.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// shortTempDir is a private directory whose control socket path fits the
// platform's limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short; Windows uses a named pipe, so any
	// temporary directory works there.
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = ""
	}
	// privatefs makes the directory private by the platform's rules (mode bits
	// or an owner-only ACL), as an installation root must be.
	dir, err := privatefs.TempDir(base, "sz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeService answers control requests with reply and records them.
func fakeService(t *testing.T, reply func(home.Request) home.Response) (string, *[]home.Request) {
	t.Helper()
	socket := home.SocketPath(testutil.PrivateDir(t))
	l, err := listenTestControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var requests []home.Request
	// Like the service, answer every connection: a Windows named pipe has no
	// backlog for a dial made while another connection is being served.
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			var request home.Request
			if json.NewDecoder(c).Decode(&request) == nil {
				requests = append(requests, request)
				_ = json.NewEncoder(c).Encode(reply(request))
			}
			_ = c.Close()
		}
	}()
	return socket, &requests
}

func TestTopLevelHelpGroupsCommandsInPlainWords(t *testing.T) {
	code, out, _ := runCLI("--help")
	if code != 0 || !strings.Contains(out, "Usage: sproozi COMMAND [ARGS] [flags]\n") {
		t.Fatalf("help exited %d: %q", code, out)
	}
	for _, c := range commands {
		listed := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(commandLabel(c)) + ` `).MatchString(out)
		if listed != (c.group != hidden) {
			t.Errorf("%s listed=%v in help", c.name, listed)
		}
	}
	for _, heading := range []string{"\nSessions:\n", "\nRemote:\n", "\nSetup:\n"} {
		if !strings.Contains(out, heading) {
			t.Errorf("help lacks %q", heading)
		}
	}
	for _, jargon := range []string{"home", "placement", "execution", "visit", "journal", "local.name", "serve",
		"daemon", "executor", "docker "} {
		if strings.Contains(strings.ToLower(out), jargon) {
			t.Errorf("top-level help uses %q", jargon)
		}
	}
	if again, bare, stderr := runCLI(); again != 2 || bare != "" || stderr != out {
		t.Fatalf("bare sproozi exited %d", again)
	}
	if code, help, _ := runCLI("help"); code != 0 || help != out {
		t.Fatalf("help command exited %d", code)
	}
}

// Every command's help lists each visible flag on one line and no hidden one.
func TestCommandHelpListsVisibleFlagsOnce(t *testing.T) {
	for _, c := range commands {
		code, out, _ := runCLI("help", c.name)
		_, viaFlag, _ := runCLI(c.name, "-h")
		if code != 0 || out != viaFlag || !strings.HasPrefix(out, "Usage: "+c.usageLine()+"\n") {
			t.Fatalf("%s help exited %d: %q", c.name, code, out)
		}
		_, flagLines, _ := strings.Cut(out, "\nFlags:\n")
		for _, name := range c.flags {
			if n := len(regexp.MustCompile(`(?m)^  (-., )?\s*--`+name+`( |$)`).FindAllString(flagLines, -1)); n != 1 {
				t.Errorf("%s help lists --%s %d times", c.name, name, n)
			}
			if c.flagHelp(name) == "" {
				t.Errorf("%s --%s has no description", c.name, name)
			}
		}
		for _, name := range c.hiddenFlags {
			if strings.Contains(out, "--"+name+" ") {
				t.Errorf("%s help shows hidden --%s", c.name, name)
			}
		}
	}
	if code, _, stderr := runCLI("help", "stauts"); code != 2 || !strings.Contains(stderr, `did you mean "status"?`) {
		t.Fatalf("unknown help topic exited %d: %q", code, stderr)
	}
}

// A command line that cannot run as written exits 2 before reading anything.
func TestUsageErrorsExitTwoBeforeReadingAnything(t *testing.T) {
	t.Setenv("SPROOZI_ROOT", filepath.Join(t.TempDir(), "absent"))
	for want, args := range map[string][]string{
		`unknown command "stat"; did you mean "status"?`:            {"stat"},
		`unknown command "start"`:                                   {"start"},
		"sessions does not accept --jsn; did you mean --json?":      {"ls", "--jsn"},
		"--harness must be codex, claude-code or copilot":           {"run", "--harness", "bogus"},
		"status needs a session ID":                                 {"status"},
		"--task applies only to a --remote run":                     {"run", "--task", "x"},
		"give --repository NAME instead of PATH":                    {"run", "--remote", "repo", "--task", "x"},
		"a --remote run needs --task TEXT or --task-file FILE":      {"run", "--remote"},
		"a --remote run does not take agent arguments":              {"run", "--context", "c", "--task", "x", "--", "-x"},
		"move needs --local (to this machine) or --remote":          {"move", "abc"},
		"move needs --local (to this machine) or --remote (to a":    {"move", "abc", "--local", "--context", "c"},
		`"notanid" is not a session ID`:                             {"stop", "notanid"},
		`credentials does not support "sett"; did you mean "set"?`:  {"credentials", "sett"},
		"service needs one of status, start, stop":                  {"service"},
		"stop: unexpected argument":                                 {"stop", "abc", "def"},
		"stop does not accept --executor":                           {"stop", "abc", "--executor", "docker"},
		"--port must be a port number":                              {"init", "--port", "80"},
		"choose --credential-file or --credential-stdin":            {"init", "--credential-file", "k", "--credential-stdin"},
		"help takes one command name":                               {"help", "run", "stop"},
		"attach does not accept --detach":                           {"attach", "abc", "--detach"},
		"resume does not accept --model; put arguments for the age": {"resume", "abc", "--model", "x"},
	} {
		code, _, stderr := runCLI(args...)
		if code != 2 || !strings.HasPrefix(stderr, "error: ") || !strings.Contains(stderr, want) {
			t.Errorf("%v exited %d without %q: %q", args, code, want, stderr)
		}
	}
}

// Operands may come before or after flags; the agent's arguments follow --.
func TestParseOperandsAndAgentArguments(t *testing.T) {
	id := uuid.NewString()
	q, err := parse([]string{cmdResume, "--root", "/r", id, "-d", "--", "--model", "gpt-5", "--", "-c"})
	root, _ := filepath.Abs("/r") // a drive-qualified path on Windows
	if err != nil || q.id != id || !q.detach || q.root != root ||
		!slices.Equal(q.harnessArgs, []string{"--model", "gpt-5", "--", "-c"}) {
		t.Fatalf("resume: %+v %v", q, err)
	}
	q, err = parse([]string{cmdRun, "repo", "--harness", "claude-code", "--", "--search"})
	if wd, _ := os.Getwd(); err != nil || q.path != filepath.Join(wd, "repo") || q.harness != "claude-code" ||
		!slices.Equal(q.harnessArgs, []string{"--search"}) || q.remote {
		t.Fatalf("run: %+v %v", q, err)
	}
	q, err = parse([]string{cmdStop, "--id", id, "--operation", id, testSocketFlag, "/s/control.sock"})
	if err != nil || q.id != id || q.operation != id || !q.socketGiven {
		t.Fatalf("hidden flags: %+v %v", q, err)
	}
	if q, err = parse([]string{cmdStatus, "3f2b", "--context", "c"}); err != nil || !q.remote || q.id != "3f2b" {
		t.Fatalf("context implies remote: %+v %v", q, err)
	}
	if _, err = parse([]string{cmdStop, id, "--", "--search"}); err == nil {
		t.Fatal("stop accepted agent arguments")
	}
}

func TestVersionNeedsNoInstallation(t *testing.T) {
	t.Setenv("SPROOZI_ROOT", filepath.Join(t.TempDir(), "absent"))
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if code, out, _ := runCLI(args...); code != 0 || out != sourceBuildLine+"\n" {
			t.Fatalf("%v exited %d: %q", args, code, out)
		}
	}
	code, out, _ := runCLI("version", "--json")
	var report versionReport
	if code != 0 || json.Unmarshal([]byte(out), &report) != nil || report.Version != "dev" {
		t.Fatalf("version --json exited %d: %q", code, out)
	}
}

func TestMatchIDFindsOneSession(t *testing.T) {
	ids := []string{"3f2b8c1e-0000-4000-8000-000000000001", "3f2b9999-0000-4000-8000-000000000002"}
	if id, err := matchID("3f2b8", ids); err != nil || id != ids[0] {
		t.Fatalf("unique prefix: %s %v", id, err)
	}
	if _, err := matchID("3f2b", ids); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if _, err := matchID("ab", ids); err == nil || !strings.Contains(err.Error(), "no session ID starts with ab") {
		t.Fatalf("unknown prefix: %v", err)
	}
}

// Output calls this machine "local" and shows a deadline only while it applies.
func TestStatusTextNamesThisMachineLocal(t *testing.T) {
	q := options{fixed: home.Config{Harnesses: []home.LocalHarness{{Name: "local-claude"}}}}
	id := uuid.NewString()
	deadline := time.Now().Add(-time.Hour)
	s := q.publicStatus(home.Status{ID: id, Harness: "claude-code", Phase: "retained", Ownership: "home",
		Placement: "local-claude", Deadline: deadline})
	if s.Ownership != localName || s.Placement != localName {
		t.Fatalf("public status: %+v", s)
	}
	if text := statusText(cmdStatus, s, false); !strings.Contains(text, "Placement  local") ||
		strings.Contains(text, "Deadline") {
		t.Fatalf("retained status: %q", text)
	}
	s.Phase = "active"
	if text := statusText(cmdStatus, s, false); !strings.Contains(text, "Deadline   "+formatTime(deadline)) {
		t.Fatalf("active status: %q", text)
	}
	for action, want := range map[string]string{
		cmdStop: "sproozi resume " + shortID(id), cmdCancel: "sproozi rm " + shortID(id), cmdRemove: "Deleted session " + id,
		cmdPull: "sproozi resume " + shortID(id), cmdMove: "Moved session " + id + " to local",
	} {
		if text := statusText(action, s, false); !strings.Contains(text, want) {
			t.Errorf("%s: %q lacks %q", action, text, want)
		}
	}
	if text := statusText(cmdRun, s, true); !strings.Contains(text, "sproozi attach "+shortID(id)) {
		t.Errorf("detached run: %q", text)
	}
}

// sessions prints a table, or one JSON document, naming this machine local.
func TestSessionsListsThisMachineAsLocal(t *testing.T) {
	id := uuid.NewString()
	socket, requests := fakeService(t, func(home.Request) home.Response {
		return home.Response{Sessions: []home.Status{{ID: id, Harness: "codex", Ownership: "home", Phase: "retained",
			Placement: "local", Deadline: time.Now().Add(-time.Hour)}}}
	})
	code, out, stderr := runCLI("ls", testSocketFlag, socket, "--project", "sproozi-dev")
	if code != 0 || !strings.HasPrefix(out, "ID ") ||
		!strings.Contains(out, shortID(id)+"  codex    retained  local      -") {
		t.Fatalf("table exited %d: %q %q", code, out, stderr)
	}
	if (*requests)[0].Project != "sproozi-dev" {
		t.Fatalf("project filter lost: %+v", *requests)
	}
	code, out, _ = runCLI("sessions", testSocketFlag, socket, "--json")
	var found catalogue
	if code != 0 || json.Unmarshal([]byte(out), &found) != nil || len(found.Sessions) != 1 ||
		found.Sessions[0].Executor != localName || found.Sessions[0].Ownership != localName ||
		found.Executors[0].Executor != localName {
		t.Fatalf("JSON exited %d: %q", code, out)
	}
}

// sessions never starts the service; it says how to.
func TestSessionsNeverStartsTheService(t *testing.T) {
	root := shortTempDir(t)
	writeTestConfig(t, root, true)
	code, out, stderr := runCLI("ls", "--root", root)
	if code != 1 || out != "" || stderr != "error: the Sproozi service is not running, so this machine's sessions "+
		`can't be listed; start it with "sproozi service start --root `+shellQuote(root)+`" (run, attach, stop and resume `+
		"start it too)\n" {
		t.Fatalf("exited %d: %q %q", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "service.pid")); !os.IsNotExist(err) {
		t.Fatal("sessions started the service")
	}
}

func TestMissingInstallationNamesInit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	code, _, stderr := runCLI("status", "abcd", "--root", root)
	if code != 1 || !strings.Contains(stderr, "no Sproozi installation at "+root+`; run "sproozi init"`) {
		t.Fatalf("exited %d: %q", code, stderr)
	}
}

// A unique prefix names the session; only an unknown outcome offers a retry.
func TestSessionPrefixesAndRetryHints(t *testing.T) {
	id := uuid.NewString()
	reply := home.Response{Status: &home.Status{ID: id, Phase: "retained", Ownership: "home"}}
	socket, requests := fakeService(t, func(r home.Request) home.Response {
		if r.Action == home.ActionSessions {
			return home.Response{Sessions: []home.Status{{ID: id}}}
		}
		return reply
	})
	code, out, _ := runCLI("stop", id[:6], testSocketFlag, socket)
	last := (*requests)[len(*requests)-1]
	if code != 0 || last.Action != home.ActionStop || last.ID != id || !strings.Contains(out, "Stopped session "+id) {
		t.Fatalf("stop by prefix exited %d: %q %+v", code, out, last)
	}
	for _, tc := range []struct {
		response home.Response
		want     string
		retry    bool
	}{
		{home.Response{Error: "no session " + id + " on this machine", UnknownSession: true},
			"no session " + id + ` on this machine; "sproozi ls" lists sessions`, false},
		{home.Response{Error: "home: session is not stopped", Rejected: true}, "session is not stopped", false},
		{home.Response{Error: "home: docker fixed operation failed"}, "docker fixed operation failed", true},
	} {
		reply = tc.response
		code, _, stderr := runCLI("stop", id, testSocketFlag, socket)
		if code != 1 || !strings.Contains(stderr, tc.want) || strings.Contains(stderr, "--operation") != tc.retry ||
			strings.Contains(stderr, "home:") {
			t.Errorf("%+v exited %d: %q", tc.response, code, stderr)
		}
	}
}

// Cancelling a cancelled session succeeds without another request.
func TestCancelIsIdempotent(t *testing.T) {
	id := uuid.NewString()
	socket, requests := fakeService(t, func(home.Request) home.Response {
		return home.Response{Status: &home.Status{ID: id, Phase: "cancelled"}}
	})
	code, out, _ := runCLI("cancel", id, testSocketFlag, socket)
	if code != 0 || !strings.Contains(out, "Cancelled session "+id) || len(*requests) != 1 ||
		(*requests)[0].Action != home.ActionStatus {
		t.Fatalf("exited %d: %q %+v", code, out, *requests)
	}
}

// writeKubeconfig writes a kubeconfig with the given contexts, each in
// namespace team-a, and points KUBECONFIG at it.
func writeKubeconfig(t *testing.T, contexts ...string) string {
	t.Helper()
	entries := make([]string, 0, len(contexts))
	for _, name := range contexts {
		entries = append(entries, "- name: "+name+"\n  context: {cluster: c, user: u, namespace: team-a}")
	}
	path := filepath.Join(testutil.PrivateDir(t), "kubeconfig")
	data := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster: {server: \"https://127.0.0.1:1\"}\n" +
		"users:\n- name: u\n  user: {token: \"" + strings.Repeat("t", 40) + "\"}\ncontexts:\n" +
		strings.Join(entries, "\n") + "\n"
	if err := testutil.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	return path
}

// Move and pull send the service the context selection, which it reaches
// itself; a move home and a pull without a sproozi context send none.
func TestMoveAndPullSendTheSelectionToTheService(t *testing.T) {
	kubeconfig := writeKubeconfig(t, "sproozi", "other")
	id := uuid.NewString()
	socket, requests := fakeService(t, func(q home.Request) home.Response {
		return home.Response{Status: &home.Status{ID: id, Phase: "retained", Placement: "engineering"}}
	})
	for _, args := range [][]string{
		{"move", id, "--remote"},
		{"move", id, "--context", "other", "--namespace", "team-b"},
		{"move", id, "--local"},
		{"pull", id},
	} {
		if code, _, errOut := runCLI(append(args, testSocketFlag, socket)...); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, errOut)
		}
	}
	got := *requests
	toDefault, toOther, back, pulled := got[0].Remote, got[1].Remote, got[2], got[3]
	if len(got) != 4 || toDefault == nil || toDefault.Context != "sproozi" || toDefault.Namespace != "team-a" ||
		toDefault.Kubeconfig[0] != kubeconfig || toOther.Context != "other" || toOther.Namespace != "team-b" ||
		back.Remote != nil || back.Destination != home.LocalAlias ||
		pulled.Remote == nil || pulled.Action != home.ActionPull {
		t.Fatalf("requests: %+v", got)
	}
	writeKubeconfig(t, "other")
	if code, _, _ := runCLI("pull", id, testSocketFlag, socket); code != 0 || (*requests)[4].Remote != nil {
		t.Fatalf("pull without a sproozi context: %+v", (*requests)[4])
	}
	if code, _, errOut := runCLI("pull", id, "--context", "missing", testSocketFlag, socket); code == 0 ||
		!strings.Contains(errOut, `--context "missing" is neither a context`) {
		t.Fatalf("unknown context: %d %s", code, errOut)
	}
}

func TestRemoteSeamExplainsAMissingSproozi(t *testing.T) {
	writeKubeconfig(t, "other")
	_, err := resolveRemote(t.Context(), "", "")
	if err == nil || !strings.Contains(err.Error(), `no context named "sproozi"`) {
		t.Fatalf("default context: %v", err)
	}
}

// The command reference's flag table must list exactly each command's
// visible flags.
func TestCommandReferenceListsEachCommandsFlags(t *testing.T) {
	data, err := os.ReadFile("../../docs/reference/session-commands.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	_, section, _ := strings.Cut(text, "\n## Command flags\n")
	section, _, _ = strings.Cut(section, "\n## ")
	row := regexp.MustCompile("(?m)^\\| `([a-z]+)` \\| (.*) \\|$")
	flagName := regexp.MustCompile("`--([a-z-]+)`")
	documented := map[string]bool{}
	for _, match := range row.FindAllStringSubmatch(section, -1) {
		c, ok := findCommand(match[1])
		if !ok {
			t.Fatalf("reference documents unknown command %s", match[1])
		}
		documented[c.name] = true
		var got []string
		for _, f := range flagName.FindAllStringSubmatch(match[2], -1) {
			got = append(got, f[1])
		}
		if !slices.Equal(got, c.flags) {
			t.Errorf("reference lists %s flags %v; command shows %v", c.name, got, c.flags)
		}
	}
	for _, c := range commands {
		if !documented[c.name] && len(c.flags) > 0 {
			t.Errorf("reference omits %s", c.name)
		}
	}
}

func TestDocumentationLinksMatchTheBuild(t *testing.T) {
	for v, want := range map[string]string{"0.1.0": "v0.1.0", "0.2.0-rc.1": "v0.2.0-rc.1", "dev": "main",
		"0.1.1-SNAPSHOT-abc1234": "main", "0.1.1-SNAPSHOT": "main"} {
		if got := docsRef(v); got != want {
			t.Errorf("docsRef(%q) = %q, want %q", v, got, want)
		}
	}
	if configDoc != "https://github.com/andrewmccall/sproozi/blob/main/cmd/sproozi/README.md" {
		t.Fatalf("source build links %s", configDoc)
	}
}

func TestSourceBuildsReportTheirCommit(t *testing.T) {
	vcs := []debug.BuildSetting{{Key: "vcs.revision", Value: "75ebc11"}, {Key: "vcs.time", Value: "2026-10-19T12:00:00Z"}}
	for _, tc := range []struct {
		name          string
		v, c, d       string
		info          *debug.BuildInfo
		want, wantRef string
	}{
		{"no build info", "dev", "unknown", "unknown", nil, sourceBuildLine, "main"},
		{"go run", "dev", "unknown", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			sourceBuildLine, "main"},
		{"go build in a checkout", "dev", "unknown", "unknown", &debug.BuildInfo{
			Main:     debug.Module{Version: "v0.1.1-0.20261019120000-75ebc11eabcd+dirty"},
			Settings: append(vcs, debug.BuildSetting{Key: "vcs.modified", Value: "true"})},
			"sproozi dev (commit 75ebc11 with uncommitted changes, built 2026-10-19T12:00:00Z (commit time))", "main"},
		{"go install at a tag", "dev", "unknown", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}},
			"sproozi 0.2.0 (commit unknown, built unknown)", "v0.2.0"},
		{"release", "0.1.0", "abc", "2026-01-01", &debug.BuildInfo{Settings: vcs},
			"sproozi 0.1.0 (commit abc, built 2026-01-01)", "v0.1.0"},
	} {
		b := describeBuild(tc.v, tc.c, tc.d, tc.info)
		if b.String() != tc.want || docsRef(b.version) != tc.wantRef {
			t.Errorf("%s: %q links %s, want %q and %s", tc.name, b, docsRef(b.version), tc.want, tc.wantRef)
		}
	}
}

// With --json, an error is one JSON object on standard error with the next
// step; flags given before the command work.
func TestJSONErrorsAndFlagsBeforeTheCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	code, out, stderr := runCLI("--json", "ls", "--root", root)
	var reply struct {
		Error, Next string
		ExitCode    int
	}
	if code != 1 || out != "" || json.Unmarshal([]byte(stderr), &reply) != nil ||
		reply.Error != "no Sproozi installation at "+root || !strings.Contains(reply.Next, `"sproozi init"`) ||
		reply.ExitCode != 1 {
		t.Fatalf("exited %d: %q %q", code, out, stderr)
	}
	if code, _, stderr = runCLI("stop", "--json"); code != 2 || !strings.Contains(stderr, `"exitCode":2`) {
		t.Fatalf("usage error exited %d: %q", code, stderr)
	}
	if code, _, stderr = runCLI("run", "--include-ignored", "/etc/hosts"); code != 2 ||
		!strings.Contains(stderr, `--include-ignored "/etc/hosts": ignored file`) || strings.Contains(stderr, " -include") {
		t.Fatalf("flag value error exited %d: %q", code, stderr)
	}
}

// A setup that stopped before writing its configuration says so.
func TestIncompleteSetupNamesInit(t *testing.T) {
	root := shortTempDir(t)
	if err := testutil.WriteFile(filepath.Join(root, "installation.json"), []byte(`{"version":1,"id":"0a1b2c3d"}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor", "--root", root}, {"status", "abcd", "--root", root}} {
		if code, _, stderr := runCLI(args...); code != 1 || !strings.Contains(stderr, "setup at "+root+
			` is incomplete: it stopped before writing config.json; run "sproozi init" to finish it`) {
			t.Errorf("%v exited %d: %q", args, code, stderr)
		}
	}
}

// Following a retry hint repeats the same request: the same session and the
// same operation. A run names the session it created, and a plain repeat of
// pull continues the same pull.
func TestRetryHintsRepeatTheSameRequest(t *testing.T) {
	id := uuid.NewString()
	socket, requests := fakeService(t, func(r home.Request) home.Response {
		if r.Action == home.ActionSessions {
			return home.Response{Sessions: []home.Status{{ID: id}}}
		}
		return home.Response{Error: "connection lost"}
	})
	hinted := regexp.MustCompile(`repeat the command with ((?:--id \S+ )?--operation \S+)`)
	last := func() home.Request { return (*requests)[len(*requests)-1] }
	repo := t.TempDir()
	for _, args := range [][]string{{"run", repo, "-d"}, {"stop", id}, {"resume", id, "-d"}, {"cancel", id},
		{"move", id, "--local"}} {
		_, _, stderr := runCLI(append(args, testSocketFlag, socket)...)
		match := hinted.FindStringSubmatch(stderr)
		if match == nil {
			t.Fatalf("%v gave no retry hint: %q", args, stderr)
		}
		first := last()
		runCLI(append(append(args, strings.Fields(match[1])...), testSocketFlag, socket)...)
		if again := last(); again.ID != first.ID || again.OperationID != first.OperationID || again.Action != first.Action {
			t.Errorf("%v: following %q sent %+v after %+v", args, match[1], again, first)
		}
	}
	runCLI("pull", id, testSocketFlag, socket)
	first := last()
	runCLI("pull", id, testSocketFlag, socket)
	if again := last(); again.Action != home.ActionPull || again.OperationID != first.OperationID {
		t.Errorf("a repeated pull used operation %s after %s", again.OperationID, first.OperationID)
	}
}

func TestMisspeltCommandsAndVerbsAreSuggested(t *testing.T) {
	// The typos are built from parts so a spelling fixer leaves them alone.
	for args, want := range map[string]string{"st" + "po": `did you mean "stop"?`,
		"service st" + "rat": `did you mean "start"?`,
		"credentials lst":    `did you mean "list"?`, "resuem x": `did you mean "resume"?`} {
		if code, _, stderr := runCLI(strings.Fields(args)...); code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("%s exited %d: %q", args, code, stderr)
		}
	}
}

// Documentation paths in messages become links, since a CLI has no checkout.
func TestDocumentationPathsBecomeLinks(t *testing.T) {
	got := explain(errors.New("create it as docs/reference/remote-executors.md shows (docs/guides/install.md)"))
	if !strings.Contains(got, "as "+docURL("docs/reference/remote-executors.md")+" shows") ||
		!strings.Contains(got, "("+docURL("docs/guides/install.md")+")") {
		t.Fatalf("links: %s", got)
	}
	if line := detachedLine(uuid.Nil.String()); !strings.Contains(line, "sproozi attach 00000000") {
		t.Fatalf("detached line: %s", line)
	}
}

// "help help" describes help instead of suggesting the name just typed.
func TestHelpHelpDescribesHelp(t *testing.T) {
	code, out, errOut := runCLI("help", "help")
	if code != 0 || !strings.Contains(out, "sproozi help [command]") || strings.Contains(errOut, "Did you mean") {
		t.Fatalf("help help = %d %q %q", code, out, errOut)
	}
}
