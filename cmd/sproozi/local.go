package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"golang.org/x/term"
)

// Verbs of the local installation commands.
const (
	verbStatus    = "status"
	verbStart     = "start"
	verbStop      = "stop"
	verbAdd       = "add"
	verbPrepare   = "prepare"
	verbRenew     = "renew"
	verbRotateCA  = "rotate-ca"
	verbReplaceCA = "replace-ca"
)

func isSetupCommand(name string) bool {
	return slices.Contains([]string{actionInit, actionService, actionDoctor, actionCredentials, actionImages,
		actionCertificates}, name)
}

// parseVerb takes the command's verb, and for operand verbs one operand, from
// the leading positional arguments.
func parseVerb(c command, args []string, s *setupOptions) ([]string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && helpRequested(args[0]) {
			return args, nil
		}
		return nil, fmt.Errorf("%s requires one of %s; usage: %s", c.name, strings.Join(c.verbs, ", "), c.line())
	}
	if !slices.Contains(c.verbs, args[0]) {
		return nil, fmt.Errorf("%s does not support %q; usage: %s", c.name, args[0], c.line())
	}
	s.verb, args = args[0], args[1:]
	if slices.Contains(c.operandVerbs, s.verb) {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return nil, fmt.Errorf("%s %s requires an operand; usage: %s", c.name, s.verb, c.line())
		}
		s.operand, args = args[0], args[1:]
	}
	return args, nil
}

func parseSetupInputs(in inputs, s *setupOptions) error {
	if in.port != "" {
		port, err := strconv.ParseUint(in.port, 10, 16)
		if err != nil || port < 1024 {
			return errors.New("--port requires an unprivileged TCP port")
		}
		s.port = uint16(port)
	}
	if in.timeout != "" {
		timeout, err := time.ParseDuration(in.timeout)
		if err != nil || timeout <= 0 {
			return errors.New("--timeout requires a positive duration such as 20m")
		}
		s.timeout = timeout
	}
	if s.credentialStdin && s.credentialFile != "" {
		return errors.New("choose --credential-file or --credential-stdin")
	}
	return nil
}

// initLocal provisions the local installation. Progress goes to stderr; the
// final result goes to stdout, as JSON with --json.
func (q *options) initLocal(ctx context.Context) error {
	s := q.setup
	if q.directory != "" {
		return errors.New("init --local uses --root, not --directory")
	}
	kind := harness.NativeKind(q.kind)
	if kind == "" {
		kind = harness.NativeCodex
	}
	if err := kind.Validate(); err != nil {
		return errors.New("--harness must be codex or claude-code")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	options := localsetup.Options{Root: s.root, Harness: kind, Model: s.model,
		Image:       localsetup.ImageRequest{Reference: s.image, BuildSource: s.build},
		ProviderURL: s.providerURL, ProviderCAFile: s.providerCA, CheckCredential: !s.skipCheck,
		Port: s.port, StartService: !s.noService, Executable: executable, Timeout: s.timeout, Progress: os.Stderr}
	options.Credential = credentialInput(s)
	if s.instructionsFile != "" {
		data, err := os.ReadFile(s.instructionsFile)
		if err != nil || len(data) == 0 || len(data) > 16<<10 {
			return errors.New("--instructions-file requires a readable UTF-8 file of at most 16 KiB")
		}
		options.Instructions = strings.TrimSpace(string(data))
	}
	if options.Model == "" {
		root, _ := localsetup.ResolveRoot(s.root)
		inst, _ := localsetup.LoadInstallation(localsetup.Layout{Root: root})
		if !slices.ContainsFunc(inst.Harnesses, func(h localsetup.Harness) bool { return h.Harness == kind }) {
			if options.Model, err = ask(fmt.Sprintf("Model for %s", kind)); err != nil {
				return fmt.Errorf("%w; pass --model NAME", err)
			}
		}
	}
	result, err := localsetup.Init(ctx, options)
	if s.json {
		if e := json.NewEncoder(q.out).Encode(struct {
			localsetup.Result
			Error string `json:"error,omitempty"`
		}{result, errorText(err)}); e != nil {
			return e
		}
	}
	if err != nil {
		return err
	}
	if !s.json {
		_, _ = fmt.Fprintf(q.out, "Ready. Start a session with:\n  sproozi start --workspace \"$PWD\" --harness %s\n", kind)
	}
	return nil
}

func credentialInput(s setupOptions) localsetup.CredentialInput {
	switch {
	case s.credentialFile != "":
		return localsetup.CredentialInput{File: s.credentialFile}
	case s.credentialStdin:
		return localsetup.CredentialInput{Stdin: os.Stdin}
	}
	return localsetup.CredentialInput{Prompt: secret}
}

// secret reads a value from the terminal without echoing it.
func secret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no terminal for a hidden prompt; use --credential-file or --credential-stdin")
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s (input hidden): ", label)
	value, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(os.Stderr)
	return string(value), err
}

func ask(label string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal to ask for input")
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s: ", label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// installation loads the layout, journal and configuration for maintenance.
func installation(root string) (localsetup.Layout, localsetup.Installation, home.Config, error) {
	resolved, err := localsetup.ResolveRoot(root)
	if err != nil {
		return localsetup.Layout{}, localsetup.Installation{}, home.Config{}, err
	}
	l := localsetup.Layout{Root: resolved}
	if _, err = os.Stat(l.Installation()); err != nil {
		err = fmt.Errorf("no local installation at %s; run sproozi init --local", resolved)
		return l, localsetup.Installation{}, home.Config{}, err
	}
	inst, err := localsetup.LoadInstallation(l)
	if err != nil {
		return l, inst, home.Config{}, err
	}
	c, err := home.ReadConfig(l.Config())
	return l, inst, c, err
}

// maintain runs service, doctor, credentials, images and certificates.
func (q *options) maintain(ctx context.Context) error {
	s := q.setup
	l, inst, c, err := installation(s.root)
	if err != nil {
		return err
	}
	var out any
	switch q.action {
	case actionService:
		out, err = serviceCommand(ctx, l, c, s)
	case actionDoctor:
		report := localsetup.Doctor(ctx, l, s.checkProvider)
		out = report
		if !report.OK {
			err = errors.New("one or more checks failed")
		}
	case actionCredentials:
		out, err = credentialsCommand(ctx, l, inst, s)
	case actionImages:
		out, err = imagesCommand(ctx, l, inst, s)
	case actionCertificates:
		out, err = certificatesCommand(ctx, l, c, s)
	}
	if out != nil {
		if s.json {
			if e := json.NewEncoder(q.out).Encode(out); e != nil {
				return e
			}
		} else {
			printHuman(q.out, out)
		}
	}
	return err
}

func serviceCommand(ctx context.Context, l localsetup.Layout, c home.Config, s setupOptions) (any, error) {
	switch s.verb {
	case verbStart:
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return localsetup.StartService(ctx, l, c, executable, 2*time.Minute)
	case verbStop:
		return localsetup.StopService(ctx, l, c, s.leaveRunning, time.Minute)
	}
	return localsetup.ServiceState(ctx, l, c), nil
}

// credentialResult reports a stored provider credential and its effect.
type credentialResult struct {
	Stored    string `json:"stored"`
	Validated bool   `json:"validated,omitempty"`
	Service   string `json:"service"`
}

func credentialsCommand(ctx context.Context, l localsetup.Layout, inst localsetup.Installation,
	s setupOptions) (any, error) {
	if s.operand != "openai" && s.operand != "anthropic" {
		return nil, errors.New("credentials add requires openai or anthropic")
	}
	var selected *localsetup.Harness
	for i := range inst.Harnesses {
		if localsetup.ProviderFor(inst.Harnesses[i].Harness).Name == s.operand {
			selected = &inst.Harnesses[i]
		}
	}
	path, err := localsetup.StoreCredential(l, s.operand, credentialInput(s))
	if err != nil {
		return nil, err
	}
	result := credentialResult{Stored: path}
	if selected != nil && !s.skipCheck {
		err = localsetup.CheckCredential(ctx, selected.Harness, selected.ProviderURL, selected.ProviderCAFile, path)
		if err != nil {
			return result, err
		}
		result.Validated = true
	}
	result.Service, err = restartForCredential(ctx, l)
	return result, err
}

// restartForCredential restarts the home daemon when no session is active.
// The gateway reads provider credentials only at startup.
func restartForCredential(ctx context.Context, l localsetup.Layout) (string, error) {
	c, err := home.ReadConfig(l.Config())
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	restarted, err := localsetup.RestartIdle(ctx, l, c, executable)
	switch {
	case err != nil:
		return "still using the previous credential", err
	case restarted:
		return "restarted with the new credential", nil
	}
	return "not running; the credential is used when it starts", nil
}

// imageStatus reports the installed and release images.
type imageStatus struct {
	Installed *localsetup.Image `json:"installed"`
	Release   string            `json:"release"`
}

// preparedImage reports an image prepared by images prepare.
type preparedImage struct {
	Image  localsetup.Image `json:"image"`
	Active bool             `json:"active"`
	Note   string           `json:"note,omitempty"`
}

func imagesCommand(ctx context.Context, l localsetup.Layout, inst localsetup.Installation,
	s setupOptions) (any, error) {
	if s.verb == verbStatus {
		return imageStatus{Installed: inst.Image, Release: localsetup.ReleaseImage()}, nil
	}
	request := localsetup.ImageRequest{Reference: s.image, BuildSource: s.build, Progress: os.Stderr}
	image, active, err := localsetup.PrepareImage(ctx, l, request)
	if err != nil {
		return nil, err
	}
	result := preparedImage{Image: image, Active: active}
	if !active {
		result.Note = "prepared but not active: retained local sessions use the current image; " +
			"changing it needs a drained migration"
	}
	return result, nil
}

func certificatesCommand(ctx context.Context, l localsetup.Layout, c home.Config, s setupOptions) (any, error) {
	store, err := localsetup.PKIStore(l)
	if err != nil {
		return nil, err
	}
	switch s.verb {
	case verbRenew:
		err = store.Renew()
	case verbRotateCA:
		// The running service switches and retires once every running
		// allocation has the new trust or has independently stopped.
		err = store.PrepareRotation()
	case verbReplaceCA:
		if status := localsetup.ServiceState(ctx, l, c); len(status.Active) > 0 {
			return nil, fmt.Errorf("replacing the CA would remove trust from %d active session(s); cancel them first",
				len(status.Active))
		}
		// The daemon may be stopped while workers keep running.
		running, countErr := localsetup.RunningContainers(ctx, l)
		if countErr != nil || running > 0 {
			return nil, errors.Join(countErr, fmt.Errorf("%d container(s) still run on the installation network; "+
				"cancel or stop their sessions first", running))
		}
		err = store.ReplaceCA()
	}
	if err != nil {
		return nil, err
	}
	return localsetup.Certificates(store)
}

// ensureService starts a locally provisioned home daemon when a local
// session command finds it absent. Installations configured by hand are left
// to their administrator.
func (q *options) ensureService(ctx context.Context) error {
	if q.configPath == "" || q.fixed.Directory == "" {
		return nil
	}
	l := localsetup.Layout{Root: filepath.Dir(q.configPath)}
	if _, err := os.Stat(l.Installation()); err != nil || l.Config() != q.configPath {
		return nil
	}
	if status := localsetup.ServiceState(ctx, l, q.fixed); status.Running {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "Starting the local home daemon")
	_, err = localsetup.StartService(ctx, l, q.fixed, executable, 2*time.Minute)
	return err
}

func printHuman(w io.Writer, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		_, _ = fmt.Fprintln(w, string(data))
	}
}
