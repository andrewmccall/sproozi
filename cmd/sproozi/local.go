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

func isSetupCommand(name string) bool {
	return slices.Contains([]string{actionInit, actionService, actionDoctor, actionCredentials, actionImages, actionCertificates}, name)
}

// parseVerb takes the command's verb, and for operand verbs one operand, from
// the leading positional arguments.
func parseVerb(c command, args []string, s *setupOptions) ([]string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
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
		return l, localsetup.Installation{}, home.Config{}, fmt.Errorf("no local installation at %s; run sproozi init --local", resolved)
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
	case "start":
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return localsetup.StartService(ctx, l, c, executable, 2*time.Minute)
	case "stop":
		return localsetup.StopService(ctx, l, c, s.leaveRunning, time.Minute)
	}
	return localsetup.ServiceState(ctx, l, c), nil
}

func credentialsCommand(ctx context.Context, l localsetup.Layout, inst localsetup.Installation, s setupOptions) (any, error) {
	var selected *localsetup.Harness
	for i := range inst.Harnesses {
		if localsetup.ProviderFor(inst.Harnesses[i].Harness).Name == s.operand {
			selected = &inst.Harnesses[i]
		}
	}
	if s.operand != "openai" && s.operand != "anthropic" {
		return nil, errors.New("credentials add requires openai or anthropic")
	}
	path, err := localsetup.StoreCredential(l, s.operand, credentialInput(s))
	if err != nil {
		return nil, err
	}
	result := map[string]any{"stored": path}
	if selected != nil && !s.skipCheck {
		if err = localsetup.CheckCredential(ctx, selected.Harness, selected.ProviderURL, selected.ProviderCAFile, path); err != nil {
			return result, err
		}
		result["validated"] = true
	}
	// The gateway reads provider credentials only at startup, so a running
	// service is restarted when no session is active.
	c, err := home.ReadConfig(l.Config())
	if err != nil {
		return result, err
	}
	status := localsetup.ServiceState(ctx, l, c)
	switch {
	case !status.Running:
		result["service"] = "not running; the credential is used when it starts"
	case len(status.Active) > 0:
		result["service"] = "still using the previous credential; it reads credentials at startup, so restart it with sproozi service stop and sproozi service start once no session is active"
	default:
		executable, err := os.Executable()
		if err != nil {
			return result, err
		}
		if _, err = localsetup.StopService(ctx, l, c, false, time.Minute); err != nil {
			return result, err
		}
		if _, err = localsetup.StartService(ctx, l, c, executable, 2*time.Minute); err != nil {
			return result, err
		}
		result["service"] = "restarted with the new credential"
	}
	return result, nil
}

func printHuman(w io.Writer, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		_, _ = fmt.Fprintln(w, string(data))
	}
}

func imagesCommand(ctx context.Context, l localsetup.Layout, inst localsetup.Installation, s setupOptions) (any, error) {
	if s.verb == "status" {
		return map[string]any{"installed": inst.Image, "release": localsetup.ReleaseImage()}, nil
	}
	image, active, err := localsetup.PrepareImage(ctx, l, localsetup.ImageRequest{Reference: s.image, BuildSource: s.build, Progress: os.Stderr})
	if err != nil {
		return nil, err
	}
	result := map[string]any{"image": image, "active": active}
	if !active {
		result["note"] = "prepared but not active: retained local sessions use the current image; changing it needs a drained migration"
	}
	return result, nil
}

func certificatesCommand(ctx context.Context, l localsetup.Layout, c home.Config, s setupOptions) (any, error) {
	store, err := localsetup.PKIStore(l)
	if err != nil {
		return nil, err
	}
	switch s.verb {
	case "renew":
		err = store.Renew()
	case "rotate-ca":
		// The running service switches and retires once every running
		// allocation has the new trust or has independently stopped.
		err = store.PrepareRotation()
	case "replace-ca":
		if status := localsetup.ServiceState(ctx, l, c); len(status.Active) > 0 {
			return nil, fmt.Errorf("replacing the CA would remove trust from %d active session(s); cancel them first", len(status.Active))
		}
		err = store.ReplaceCA()
	}
	if err != nil {
		return nil, err
	}
	return localsetup.Certificates(store)
}

// ensureService starts a locally provisioned home service when a local
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
	_, _ = fmt.Fprintln(os.Stderr, "Starting the local home service")
	_, err = localsetup.StartService(ctx, l, q.fixed, executable, 2*time.Minute)
	return err
}
