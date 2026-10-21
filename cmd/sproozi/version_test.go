//nolint:goconst // Command spellings are independent behavior inputs.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

func TestHelpAndVersionNeedNoHome(t *testing.T) {
	t.Setenv("SPROOZI_CONFIG", "does-not-exist")
	for _, arg := range []string{"--help", "-h", "help", "version", "--version"} {
		output := captureCLIOutput(t, []string{arg})
		if strings.Contains(arg, "version") {
			if output != "sproozi dev (source build; use the checkout you built from)\n" {
				t.Fatalf("%s: %q", arg, output)
			}
		} else if !strings.Contains(output, "Usage: sproozi COMMAND [flags]\n") {
			t.Fatalf("%s: missing usage in %q", arg, output)
		}
	}
}

func captureCLIOutput(t *testing.T, args []string) string {
	t.Helper()
	var output bytes.Buffer
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	return output.String()
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

func TestInitCreatesStarterConfigurationAndPreservesFiles(t *testing.T) {
	path := filepath.Join(testutil.PrivateDir(t), "Application Support")
	file := filepath.Join(path, "config.json")
	output := captureCLIOutput(t, []string{actionInit, "--directory", path})
	if !strings.Contains(output, "Created configuration "+shellQuote(file)+"\n") ||
		!strings.Contains(output, `add an executor under "remote"`) ||
		!strings.Contains(output, "\n    sproozi init --local\n    Guide: "+localSessionsDoc+"\n") ||
		strings.Contains(output, "profile service") || strings.Contains(output, `replace "model"`) ||
		!strings.Contains(output, "Guide: "+localSetupDoc+"\n") || !strings.Contains(output, "Guide: "+remoteSetupDoc+"\n") ||
		!strings.Contains(output, "sproozi serve --config "+shellQuote(file)+"\n") ||
		!strings.Contains(output, "sproozi sessions --config "+shellQuote(file)+"\n") ||
		!strings.Contains(output, configDoc) {
		t.Fatalf("init did not name the configuration and next step: %q", output)
	}
	if shellQuote(file) == file {
		t.Fatalf("path with a space left unquoted: %s", file)
	}
	if err := privatefs.Check(path, true); err != nil {
		t.Fatal(err)
	}
	fixed, err := home.ReadConfig(file)
	if err != nil {
		t.Fatalf("starter configuration is invalid: %v", err)
	}
	if err = privatefs.Check(fixed.Directory, true); err != nil {
		t.Fatal(err)
	}
	output = captureCLIOutput(t, []string{actionInit, "--directory", path})
	if !strings.Contains(output, "Kept existing configuration") {
		t.Fatalf("init did not report the kept configuration: %q", output)
	}
	// An unusable existing configuration fails and is left unchanged.
	for _, data := range []string{"keep this", ""} {
		if err = testutil.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := cli([]string{actionInit, "--directory", path}, &stdout, &stderr); code != 1 ||
			!strings.Contains(stderr.String(), "configuration "+file+": ") || strings.Contains(stdout.String(), "Kept") {
			t.Fatalf("init accepted %q: exited %d: %q %q", data, code, stdout.String(), stderr.String())
		}
		if kept, e := os.ReadFile(file); e != nil || string(kept) != data {
			t.Fatalf("init changed the existing configuration: %q, %v", kept, e)
		}
	}
}

func TestStarterConfigurationNamesWhatEachCommandNeeds(t *testing.T) {
	// A short name keeps home's control socket within the platform limit.
	dir := filepath.Join(testutil.PrivateDir(t), "c")
	file := filepath.Join(dir, "config.json")
	captureCLIOutput(t, []string{actionInit, "--directory", dir})
	for _, tc := range []struct {
		args []string
		want []string
	}{
		// No daemon and no remote executor: nothing answered.
		{[]string{"sessions", "--config", file}, []string{
			"no sessions listed: the results above show that no executor answered: docker (home): no home daemon is listening",
			`start home with "sproozi serve --config ` + shellQuote(file) + `"`}},
		{[]string{"sessions", "--config", file, "--executor", "local"}, []string{
			`executor "local" is local.name, home's local placement for "move --to", not an executor; ` +
				`use --executor docker (home's executor name) or omit --executor`}},
		{[]string{"sessions", "--config", file, "--executor", "home"}, []string{
			`executor "home" names the home daemon, not an executor; use --executor docker (home's executor name) ` +
				`or omit --executor`}},
		{[]string{"status", uuid.NewString(), "--config", file, "--executor", "Bad Name"}, []string{
			`executor "Bad Name" is not a valid name; use 1 to 63`, "so no executor can have it"}},
		{[]string{"status", uuid.NewString(), "--config", file, "--executor", "elsewhere"}, []string{
			`executor "elsewhere" is not configured; use --executor docker (home's executor name), ` +
				`or add it under "remote" or "workSources" in ` + file}},
		{[]string{"serve", "--config", file}, []string{"is not ready for serve; set:\n  dockerBinary (absolute path)\n",
			"\n  dockerConfig (absolute path)\n", "\n  local.nativeImageID",
			"\n  gateway.providerTokenFile", "see " + localSetupDoc + "\n", "For each key, see " + configDoc + "\n"}},
		{[]string{"run", "--config", file, "--executor", "local", "--task", "x"}, []string{
			`run submits only to a remote executor configured under "remote" or "workSources" in ` + file,
			`"local" is local.name, home's local placement for "move --to", not an executor`, remoteSetupDoc}},
		{[]string{"run", "--config", file, "--executor", "elsewhere", "--task", "x"}, []string{
			`run submits only to a remote executor configured under "remote" or "workSources"`,
			`"elsewhere" is not configured`}},
		{[]string{"run", "--config", file, "--executor", "docker", "--task", "x"}, []string{
			`run does not accept --executor docker: docker is home's executor name, and run submits only to`,
			`"sproozi start --workspace PATH"`}},
		{[]string{"run", "--config", file, "--executor", "home", "--task", "x"}, []string{
			`run does not accept --executor home: home names the home daemon, whose executor name is docker, ` +
				`and run submits only to`}},
		{[]string{"move", uuid.NewString(), "--config", file, "--to", "docker"}, []string{
			`move --to docker: "docker" is home's executor name; use --to local (local.name) to move to this machine`}},
		{[]string{"move", uuid.NewString(), "--config", file, "--to", "home"}, []string{
			`move --to home: "home" names the home daemon; use --to local (local.name) to move to this machine`}},
		{[]string{"move", uuid.NewString(), "--config", file, "--to", "elsewhere"}, []string{
			`move --to "elsewhere" is not a configured placement; use one of: local (local.name), ` +
				`or add it under "remote" in ` + file}},
		{[]string{"start", "--config", file, "--workspace", dir}, []string{
			`start home with "sproozi serve --config ` + shellQuote(file) + `"`}},
		{[]string{"start", "notgit", "--config", file}, []string{
			`session UUID "notgit" is not a canonical UUID`, `start takes the directory as "--workspace PATH"`}},
	} {
		var stdout, stderr bytes.Buffer
		code := cli(tc.args, &stdout, &stderr)
		for _, want := range tc.want {
			if code != 1 || !strings.Contains(stderr.String(), want) {
				t.Fatalf("%v exited %d without %q: %q", tc.args, code, want, stderr.String())
			}
		}
		if (tc.args[0] == "move" || tc.args[0] == "start") && strings.Contains(stderr.String(), "to retry") {
			t.Errorf("%v offered a retry for a validation error: %q", tc.args, stderr.String())
		}
	}
	// Discovery still writes its catalogue when it fails.
	var stdout bytes.Buffer
	if err := run([]string{"sessions", "--config", file}, &stdout); err == nil ||
		!strings.Contains(stdout.String(), `{"executor":"docker","availability":"unavailable","error":"home_not_running"}`) {
		t.Fatalf("sessions with starter configuration: %q %v", stdout.String(), err)
	}
}

func TestInitAndServeNameInvalidAllowancesAndRemotes(t *testing.T) {
	dir := filepath.Join(testutil.PrivateDir(t), "configuration")
	file := filepath.Join(dir, "config.json")
	captureCLIOutput(t, []string{actionInit, "--directory", dir})
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	negative := strings.Replace(string(data), `"value": 200000`, `"value": -1`, 1)
	if err = testutil.WriteFile(file, []byte(negative), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{actionInit, "--directory", dir}, {"serve", "--config", file}} {
		var stdout, stderr bytes.Buffer
		if code := cli(args, &stdout, &stderr); code != 1 ||
			!strings.Contains(stderr.String(), "allowances.model.inference.units (") {
			t.Fatalf("%v exited %d: %q", args, code, stderr.String())
		}
	}
	empty := strings.Replace(string(data), `"remote": {}`, `"remote": {"engineering": {}}`, 1)
	if err = testutil.WriteFile(file, []byte(empty), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"sessions", "--config", file},
		{"run", "--config", file, "--executor", testExecutor, "--task", "x"}} {
		var stdout, stderr bytes.Buffer
		if code := cli(args, &stdout, &stderr); code != 1 ||
			!strings.Contains(stderr.String(), "remote.engineering.url: ") ||
			!strings.Contains(stderr.String(), "remote.engineering.tokenFile: ") {
			t.Fatalf("%v exited %d: %q", args, code, stderr.String())
		}
	}
}

func TestConfigurationErrorsNameTheFileAndNextStep(t *testing.T) {
	dir := testutil.PrivateDir(t)
	missing := filepath.Join(dir, "absent", "config.json")
	other := filepath.Join(dir, "absent", "other.json")
	malformed := filepath.Join(dir, "config.json")
	if err := testutil.WriteFile(malformed, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"sessions", "--config", missing},
			"no configuration at " + missing + `; run "sproozi init --directory ` +
				shellQuote(filepath.Dir(missing)) + `" to create it`},
		{[]string{"sessions", "--config", other}, "no configuration at " + other +
			`; create it, for example with "sproozi init --directory ` + shellQuote(filepath.Dir(other)) +
			`", which writes ` + shellQuote(filepath.Join(filepath.Dir(other), configFile)) + "; then pass --config " +
			shellQuote(filepath.Join(filepath.Dir(other), configFile)) + ", then see"},
		{[]string{"sessions", "--config", malformed},
			"configuration " + malformed + ": unexpected EOF; edit the file to correct it, see " + configDoc},
		{[]string{"serve", "--config", missing}, "no configuration at " + missing},
		{[]string{"run", "--task", "x", "--config", missing},
			`--executor NAME naming a remote executor configured under "remote" or "workSources" in ` + missing},
	} {
		var stdout, stderr bytes.Buffer
		if code := cli(tc.args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) ||
			strings.Contains(stderr.String(), "aside") {
			t.Fatalf("%v exited %d: %q", tc.args, code, stderr.String())
		}
	}
}

func TestRunHelpNamesOnlyRemoteExecutors(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{actionRun, "-h"}, &stdout); err != nil ||
		!strings.Contains(stdout.String(), `remote executor NAME configured under "remote" or "workSources"; `+
			"run does not accept docker") || strings.Contains(stdout.String(), "docker (home)") {
		t.Fatalf("run help: %q %v", stdout.String(), err)
	}
}

func TestMissingHomeNamesTheDaemon(t *testing.T) {
	err := privateHint(home.ErrControlUnavailable, "")
	if !strings.Contains(err.Error(), `listening on the private control socket; start home with "sproozi serve"`) {
		t.Fatalf("missing daemon hint: %v", err)
	}
	retry := privateHint(&retryError{err: home.ErrControlUnavailable, id: "S", operation: "O"}, "").Error()
	if !strings.HasSuffix(retry, "; to retry, repeat the command with --id S --operation O") ||
		!strings.Contains(retry, "start home with") {
		t.Fatalf("retry hint: %s", retry)
	}
}

func TestCatalogueFailsOnlyWhenNoOwnerAnswered(t *testing.T) {
	unavailable := executorAvailability{Executor: dockerExecutor, Availability: testUnavailable}
	answered := executorAvailability{Executor: testExecutor, Availability: testAvailable}
	partial := catalogue{Executors: []executorAvailability{unavailable, answered},
		problems: map[string]error{dockerExecutor: home.ErrControlUnavailable}}
	if err := partial.failure(""); err != nil {
		t.Fatalf("partial catalogue failed: %v", err)
	}
	silent := executorAvailability{Executor: testExecutor, Availability: testUnavailable, Error: "unavailable"}
	none := catalogue{Executors: []executorAvailability{unavailable, silent},
		problems: map[string]error{dockerExecutor: home.ErrControlUnavailable, testExecutor: errors.New("x")}}
	refused := fmt.Errorf("guidance: %w", &profile.RemoteError{Code: profile.CodeUnavailable,
		Transport: errors.New("connection refused")})
	none.problems[testExecutor] = refused
	if err := none.failure(""); !errors.Is(err, home.ErrControlUnavailable) ||
		!strings.Contains(err.Error(), "no executor answered: executor engineering: guidance: "+
			"session profile: unavailable (no HTTP response) (connection refused); and docker (home): ") ||
		strings.Contains(err.Error(), "unavailable) are unavailable") {
		t.Fatalf("empty catalogue: %v", err)
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
		{"no build info", "dev", "unknown", "unknown", nil,
			sourceBuildLine, "main"},
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

// A relative --config or --socket names a file from the working directory,
// as init --directory does, for every command.
func TestRelativePathFlagsResolveFromTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	relative := filepath.Join("cfg", "config.json")
	for _, args := range [][]string{{"serve", "--config", relative}, {"sessions", "--config", relative}} {
		var stdout, stderr bytes.Buffer
		cli(args, &stdout, &stderr)
		if strings.Contains(stderr.String(), "absolute") ||
			!strings.Contains(stderr.String(), filepath.Join(dir, "cfg", "config.json")) {
			t.Fatalf("%v: %q", args, stderr.String())
		}
	}
	got, err := absolutePath("home/control.sock")
	if err != nil || got != filepath.Join(dir, "home", "control.sock") {
		t.Fatalf("socket = %q, %v", got, err)
	}
}

// Path flags that cannot work name the path and the fix.
func TestPathFlagsExplainTheirFault(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(dir, strings.Repeat("s", 120), "control.sock")
	cases := map[string][]string{
		"--config needs a path": {"status", uuid.NewString(), "--config", ""},
		"init --directory " + shellQuote(file) + " is a file, not a directory": {"init", "--directory", file},
	}
	// init would keep an existing configuration, so the hint points at it.
	held := t.TempDir()
	if err := os.WriteFile(filepath.Join(held, configFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases[shellQuote(held)+" already holds "+configFile] =
		[]string{"sessions", "--config", filepath.Join(held, "other.json")}
	// Windows names a named pipe with --socket, which has no path length limit
	// and is never a file.
	if runtime.GOOS != goosWindows {
		cases["use a shorter --socket path"] = []string{"sessions", "--socket", long}
		cases["--socket "+shellQuote(dir)+" is a directory, not a socket"] =
			[]string{"status", uuid.NewString(), "--socket", dir}
		cases["--socket "+shellQuote(file)+" is a regular file, not a socket"] = []string{"sessions", "--socket", file}
	}
	for want, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := cli(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%v exited %d without %q: %q", args, code, want, stderr.String())
		}
	}
}
