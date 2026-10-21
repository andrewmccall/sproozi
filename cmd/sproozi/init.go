//nolint:goconst // Starter configuration keys read best as JSON literals.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

const actionInit = "init"

// configFile is the configuration leaf that init writes and commands read.
const configFile = "config.json"

// repository is the public source of the documentation the CLI links to.
const repository = "https://github.com/andrewmccall/sproozi"

// placeholderModel is the model earlier starter configurations wrote, which an
// administrator had to replace. Starter configurations now omit the model, so
// the harness uses its own default.
const placeholderModel = "ADMINISTRATOR_SELECTED_MODEL"

// releaseVersion matches the tag-derived versions that release builds set.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// docsRef is the release tag a release build documents, or main for any other build.
func docsRef(v string) string {
	if releaseVersion.MatchString(v) && !strings.Contains(v, "SNAPSHOT") {
		return "v" + v
	}
	return "main"
}

// docURL links to a repository document at the ref matching this build.
func docURL(path string) string { return repository + "/blob/" + docsRef(build.version) + "/" + path }

// configDoc is the reference for the configuration format and executors.
var configDoc = docURL("cmd/sproozi/README.md")

// localSetupDoc builds the images, network and certificates that serve needs.
var localSetupDoc = docURL("docs/guides/session-mobility.md") + "#prepare-the-administrator-owned-setup"

// localSessionsDoc provisions local Docker sessions with init --local.
var localSessionsDoc = docURL("docs/guides/local-sessions.md")

// remoteSetupDoc configures a remote executor for run and sessions.
var remoteSetupDoc = docURL("docs/guides/portable-workflows.md")

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// goosWindows names the platform whose paths and sockets differ.
const goosWindows = "windows"

// shellQuote makes a path safe to paste into the user's shell.
func shellQuote(s string) string {
	switch {
	case shellSafe.MatchString(s):
		return s
	case runtime.GOOS == goosWindows:
		return `"` + s + `"`
	default:
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
}

// starterConfig is the smallest configuration home.ReadConfig accepts. It lets
// client commands reach home and configured executors; serve still needs the
// images, Docker paths and gateway credentials that only an administrator has.
func starterConfig(homeDirectory string) map[string]any {
	return map[string]any{
		"directory": homeDirectory,
		"local":     map[string]any{"harness": "codex", "name": "local"},
		"allowances": map[string]any{"model.inference": map[string]any{
			"units":      map[string]any{"bounded": true, "value": 200000},
			"costMicros": map[string]any{"bounded": false, "value": 0},
		}},
		"maxDuration": "30m",
		"remote":      map[string]any{},
	}
}

func initialize(directory string, out io.Writer) error {
	if directory == "" {
		config, err := defaultConfigPath()
		if err != nil {
			return err
		}
		directory = filepath.Dir(config)
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(absolute); statErr == nil && !info.IsDir() {
		return fmt.Errorf("init --directory %s is a file, not a directory; name the directory to hold config.json",
			shellQuote(absolute))
	}
	if err = privatefs.EnsureDir(absolute); err != nil {
		return err
	}
	path := filepath.Join(absolute, configFile)
	created := false
	if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		state := filepath.Join(absolute, "home")
		data, e := json.MarshalIndent(starterConfig(state), "", "  ")
		if e != nil {
			return e
		}
		if e = privatefs.EnsureDir(state); e != nil {
			return e
		}
		if e = privatefs.Replace(absolute, configFile, append(data, '\n')); e != nil {
			return e
		}
		created = true
	} else if err != nil {
		return err
	} else if fixed, e := readConfig(path); e != nil {
		return e
	} else if problems := fixed.AllowanceProblems(); len(problems) > 0 {
		return fmt.Errorf("configuration %s: set %s; see %s", path, strings.Join(problems, ", "), configDoc)
	}
	return printNextStep(out, path, created)
}

// serveCommand is the command that starts home with the configuration the
// user named; the default configuration needs no --config.
func serveCommand(configPath string) string {
	if def, err := defaultConfigPath(); configPath == "" || (err == nil && def == configPath) {
		return "sproozi serve"
	}
	return "sproozi serve --config " + shellQuote(configPath)
}

// startHomeCommand starts home for the configuration: the detached service
// for a local installation's configuration, otherwise serve in the foreground.
func startHomeCommand(configPath string) string {
	if configPath == "" {
		configPath, _ = defaultConfigPath()
	}
	root := filepath.Dir(configPath)
	if _, err := os.Stat(localsetup.Layout{Root: root}.Installation()); err != nil || configPath == "" {
		return serveCommand(configPath)
	}
	if recorded, err := localsetup.ResolveRoot(""); err == nil && recorded == root {
		return "sproozi service start"
	}
	return "sproozi service start --root " + shellQuote(root)
}

// configArgument returns the --config value on a command line, if any.
func configArgument(args []string) string {
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--"+configFlag+"="); ok {
			return value
		}
		if arg == "--"+configFlag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func printNextStep(out io.Writer, path string, created bool) error {
	verb := "Kept existing"
	if created {
		verb = "Created"
	}
	flag := ""
	if def, err := defaultConfigPath(); err != nil || def != path {
		flag = " --config " + shellQuote(path)
	}
	_, err := fmt.Fprintf(out, `%s configuration %s
For local Docker sessions, run this instead; it provisions everything they need:
    sproozi init --local
    Guide: %s
  Session commands use that installation while this file names no remote
  executor and is not ready for serve.
For remote work, add an executor under "remote" in this file, then run
    sproozi sessions%s
    Guide: %s
To configure local sessions by hand, add the Docker paths, the "local" images,
network, instructions, limits and sizes, and the "gateway" certificates and
provider to this file; serve names any setting still missing. Then run
    sproozi serve%s
    Guide: %s
Configuration reference: %s
`, verb, shellQuote(path), localSessionsDoc, flag, remoteSetupDoc, flag, localSetupDoc, configDoc)
	return err
}

// initCommand is the init command that recreates path, or "" when init cannot.
func initCommand(path string) string {
	if def, err := defaultConfigPath(); err == nil && def == path {
		return "sproozi init"
	}
	if filepath.Base(path) != configFile {
		return ""
	}
	return "sproozi init --directory " + shellQuote(filepath.Dir(path))
}

// readConfig reads the global configuration and names the file and next step
// in every failure.
func readConfig(path string) (home.Config, error) {
	fixed, err := home.ReadConfig(path)
	if err == nil {
		return fixed, nil
	}
	dir := filepath.Dir(path)
	written := filepath.Join(dir, configFile)
	recreate := `create it, for example with "sproozi init --directory ` + shellQuote(dir) + `", which writes ` +
		shellQuote(written) + `; then pass --config ` + shellQuote(written)
	if _, statErr := os.Lstat(written); statErr == nil {
		// init keeps an existing configuration, such as a live home's.
		recreate = shellQuote(dir) + ` already holds ` + configFile + `; pass --config ` + shellQuote(written) +
			`, or run "sproozi init --directory DIR" with another directory and pass --config DIR/` + configFile
	}
	if command := initCommand(path); command != "" {
		recreate = `run "` + command + `" to create it`
	}
	if info, e := os.Lstat(path); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return fixed, fmt.Errorf("configuration %s is a symbolic link; it must be a regular file: "+
			"replace the link with a copy of the file it points to, or remove it and %s; see %s", path, recreate, configDoc)
	} else if e == nil && info.IsDir() {
		return fixed, fmt.Errorf("configuration %s is a directory; it must be a regular file: "+
			"move the directory aside and %s; see %s", path, recreate, configDoc)
	}
	var unsafe *privatefs.UnsafePathError
	var invalid *home.InvalidConfigError
	switch {
	case errors.As(err, &unsafe):
	case errors.As(err, &invalid):
		err = fmt.Errorf("configuration %s: %w; edit the file to correct it, see %s", path, err, configDoc)
	case errors.Is(err, os.ErrNotExist):
		err = fmt.Errorf("no configuration at %s; %s, then see %s", path, recreate, configDoc)
	default:
		err = fmt.Errorf("configuration %s: %w; correct it, or move it aside and %s; see %s", path, err, recreate, configDoc)
	}
	return fixed, err
}

// serveReady names every setting serve still needs, by JSON key.
func serveReady(path string, c home.Config) error {
	problems := c.ServeProblems()
	if problem := c.ProviderTokenProblem(); problem != "" {
		problems = append(problems, problem)
	}
	if c.Model == placeholderModel {
		problems = append([]string{"model (replace " + placeholderModel +
			" with a model name, or remove it to use the harness's default)"}, problems...)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("configuration %s is not ready for serve; set:\n  %s\n"+
		"To build the images, network and certificates these settings name, see %s\nFor each key, see %s",
		path, strings.Join(problems, "\n  "), localSetupDoc, configDoc)
}

// retryError keeps the stable IDs that repeat a request whose outcome is unknown.
type retryError struct {
	err           error
	id, operation string
}

func (e *retryError) Error() string {
	return fmt.Sprintf("%v; to retry, repeat the command with --id %s --operation %s", e.err, e.id, e.operation)
}

func (e *retryError) Unwrap() error { return e.err }

// privateHint adds the repair to an unsafe private path error or a missing
// home daemon.
func privateHint(err error, configPath string) error {
	var retry *retryError
	if errors.As(err, &retry) {
		return &retryError{err: privateHint(retry.err, configPath), id: retry.id, operation: retry.operation}
	}
	if errors.Is(err, home.ErrControlUnavailable) {
		command := startHomeCommand(configPath)
		if command != serveCommand(configPath) {
			return fmt.Errorf("%w; start home with \"%s\"", err, command)
		}
		return fmt.Errorf("%w; start home with \"%s\", see %s", err, command, configDoc)
	}
	var unsafe *privatefs.UnsafePathError
	if !errors.As(err, &unsafe) {
		return err
	}
	if info, e := os.Lstat(unsafe.Path); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w; it must be a real file or directory, not a symbolic link", err)
	}
	if runtime.GOOS == goosWindows {
		return fmt.Errorf("%w; make it owned by your account with access for no other user, see %s",
			err, docURL("docs/guides/windows.md"))
	}
	info, e := os.Lstat(unsafe.Path)
	if e != nil || filepath.Dir(unsafe.Path) == unsafe.Path {
		// Never suggest chmod on the root or on a path that is not there.
		return fmt.Errorf("%w; use a private directory you own instead", err)
	}
	mode := "600"
	if info.IsDir() {
		mode = "700"
	}
	return fmt.Errorf("%w; it must be yours, not a link, with no group or other access: chmod %s %s",
		err, mode, shellQuote(unsafe.Path))
}
