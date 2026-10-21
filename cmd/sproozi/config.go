package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// repository is the public source of the documentation the CLI links to.
const repository = "https://github.com/andrewmccall/sproozi"

// releaseVersion matches the tag-derived versions that release builds set.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// docsRef is the release tag a release build documents, or main for any other build.
func docsRef(v string) string {
	if releaseVersion.MatchString(v) && !strings.Contains(v, "SNAPSHOT") {
		return "v" + v
	}
	return mainBranch
}

const mainBranch = "main"

// docURL links to a repository document at the ref matching this build.
func docURL(path string) string { return repository + "/blob/" + docsRef(build.version) + "/" + path }

// configDoc is the reference for a hand-written configuration.
var configDoc = docURL("cmd/sproozi/README.md")

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote makes a path safe to paste into the user's shell.
func shellQuote(s string) string {
	switch {
	case shellSafe.MatchString(s):
		return s
	case isWindows():
		return `"` + s + `"`
	default:
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
}

// config returns the configuration file this command reads: --config for a
// hand-written setup, otherwise the installation's.
func (q *options) config() (string, error) {
	if q.configPath != "" {
		return q.configPath, nil
	}
	root, err := localsetup.ResolveRoot(q.root)
	if err != nil {
		return "", err
	}
	return localsetup.Layout{Root: root}.Config(), nil
}

// load reads the configuration and finds the service's control socket. A
// command given only --socket reads no configuration.
func (q *options) load() error {
	if err := q.checkSocket(); err != nil {
		return err
	}
	if q.socketGiven && q.configPath == "" {
		return nil
	}
	path, err := q.config()
	if err != nil {
		return err
	}
	if q.fixed, err = readConfig(path); err != nil {
		return err
	}
	q.configPath = path
	if q.socket == "" {
		q.socket = home.SocketPath(q.fixed.Directory)
	}
	return nil
}

// readConfig reads a configuration and names the file and the next step in
// every failure.
func readConfig(path string) (home.Config, error) {
	fixed, err := home.ReadConfig(path)
	if err == nil {
		return fixed, nil
	}
	installation := filepath.Base(path) == "config.json"
	if info, e := os.Lstat(path); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return fixed, fmt.Errorf("configuration %s is a symbolic link; replace it with a copy of the file it points to", path)
	} else if e == nil && info.IsDir() {
		return fixed, fmt.Errorf("configuration %s is a directory; it must be a file", path)
	}
	var unsafe *privatefs.UnsafePathError
	var invalid *home.InvalidConfigError
	switch {
	case errors.As(err, &unsafe):
	case errors.As(err, &invalid):
		err = fmt.Errorf("configuration %s: %w; edit the file to correct it, see %s", path, err, configDoc)
	case errors.Is(err, os.ErrNotExist) && installation:
		err = fmt.Errorf("no Sproozi installation at %s; run \"sproozi init\" to set one up, or choose another "+
			"with --root", filepath.Dir(path))
	case errors.Is(err, os.ErrNotExist):
		err = fmt.Errorf("no configuration file at %s", path)
	default:
		err = fmt.Errorf("configuration %s: %w", path, err)
	}
	return fixed, err
}

// serveReady names every setting a hand-written configuration still needs.
func serveReady(path string, c home.Config) error {
	problems := c.ServeProblems()
	if problem := c.ProviderTokenProblem(); problem != "" {
		problems = append(problems, problem)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("configuration %s is not ready for the service; set:\n  %s\nFor each key, see %s",
		path, strings.Join(problems, "\n  "), configDoc)
}

// startCommand starts the service for a configuration: "service start" for
// an installation, "service run" in the foreground for a hand-written file.
func startCommand(configPath string) string {
	root := filepath.Dir(configPath)
	if _, err := os.Stat(localsetup.Layout{Root: root}.Installation()); err != nil || configPath == "" {
		return "sproozi service run --config " + shellQuote(configPath)
	}
	if resolved, err := localsetup.ResolveRoot(""); err == nil && resolved == root {
		return "sproozi service start"
	}
	return "sproozi service start --root " + shellQuote(root)
}

// serviceDown reports that the service is not running and how to start it.
type serviceDown struct {
	err     error
	command string
}

func (e *serviceDown) Error() string {
	return fmt.Sprintf("the Sproozi service is not running; start it with \"%s\"", e.command)
}

func (e *serviceDown) Unwrap() error { return e.err }

// retryError keeps the IDs that repeat a request whose outcome is unknown.
type retryError struct {
	err           error
	id, operation string
}

func (e *retryError) Error() string {
	return fmt.Sprintf("%v; the outcome is unknown: check with \"sproozi status %s\", and to try again repeat "+
		"the command with --operation %s", e.err, e.id, e.operation)
}

func (e *retryError) Unwrap() error { return e.err }

// explain turns an error into the line the user sees: plain names, and the
// repair for a private path.
func explain(err error) string {
	var retry *retryError
	var down *serviceDown
	switch {
	case errors.As(err, &retry):
		return (&retryError{err: errors.New(explain(retry.err)), id: retry.id, operation: retry.operation}).Error()
	case errors.As(err, &down):
		return down.Error()
	case errors.Is(err, home.ErrControlUnavailable):
		return "the Sproozi service is not running; start it with \"sproozi service start\""
	}
	message := err.Error()
	for _, prefix := range []string{"docker (home): ", "home: "} {
		message = strings.TrimPrefix(message, prefix)
	}
	var unsafe *privatefs.UnsafePathError
	if !errors.As(err, &unsafe) {
		return message
	}
	if info, e := os.Lstat(unsafe.Path); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return message + "; it must be a real file or directory, not a symbolic link"
	}
	if isWindows() {
		return message + "; make it owned by your account with access for no other user, see " +
			docURL("docs/guides/windows.md")
	}
	info, e := os.Lstat(unsafe.Path)
	if e != nil || filepath.Dir(unsafe.Path) == unsafe.Path {
		// Never suggest chmod on the root or on a path that is not there.
		return message + "; use a private directory you own instead"
	}
	mode := "600"
	if info.IsDir() {
		mode = "700"
	}
	return fmt.Sprintf("%s; it must be yours, not a link, with no group or other access: chmod %s %s",
		message, mode, shellQuote(unsafe.Path))
}

// checkSocket explains a --socket that cannot work: too long to bind, or
// naming something that is not a socket.
func (q *options) checkSocket() error {
	if !q.socketGiven {
		return nil
	}
	if problem := home.SocketPathProblem(q.socket); problem != "" {
		return usagef("%s; use a shorter --socket path", problem)
	}
	// On Windows the socket is a named pipe, and stat opens it as a client.
	if isWindows() {
		return nil
	}
	if info, err := os.Stat(q.socket); err == nil && info.Mode()&os.ModeSocket == 0 {
		kind := "a regular file"
		if info.IsDir() {
			kind = "a directory"
		}
		return usagef("--socket %s is %s, not a socket", shellQuote(q.socket), kind)
	}
	return nil
}
