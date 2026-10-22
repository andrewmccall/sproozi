package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"golang.org/x/term"
)

// initLocal sets up this machine. Progress goes to standard error; the result
// goes to standard output.
func (q *options) initLocal(ctx context.Context) error {
	kind := harness.NativeKind(q.harness)
	if kind == "" {
		kind = harness.NativeCodex
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	options := localsetup.Options{Root: q.root, Harness: kind, Model: q.model,
		Image:       localsetup.ImageRequest{Reference: q.image, BuildSource: q.build},
		ProviderURL: q.providerURL, ProviderCAFile: q.providerCA, CheckCredential: !q.skipCheck,
		Port: q.port, StartService: !q.noService, Executable: executable, Timeout: q.timeout, Progress: os.Stderr,
		Credential: q.credentialInput()}
	if q.instructionsFile != "" {
		data, err := os.ReadFile(q.instructionsFile)
		if err != nil || len(data) == 0 || len(data) > 16<<10 {
			return usagef("--instructions-file needs a readable UTF-8 file of at most 16 KiB")
		}
		options.Instructions = strings.TrimSpace(string(data))
	}
	result, err := localsetup.Init(ctx, options)
	if q.json {
		if e := writeJSON(q.out, struct {
			localsetup.Result
			Error string `json:"error,omitempty"`
		}{result, errorText(err)}); e != nil {
			return e
		}
		return err
	}
	if err != nil {
		return err
	}
	switch {
	case result.Warning != "":
		_, err = fmt.Fprintf(q.out, "Saved. %s.\n", upperFirst(result.Warning))
	case q.noService:
		_, err = fmt.Fprintf(q.out, "Saved the configuration in %s. Start the service with: sproozi service start\n",
			result.Root)
	default:
		_, err = fmt.Fprintln(q.out, "Ready. In a Git repository, start an agent with:\n  sproozi run"+
			harnessFlag(kind, result.Harnesses))
	}
	return err
}

// harnessFlag is the --harness a run needs to select kind, if any.
func harnessFlag(kind harness.NativeKind, harnesses []localsetup.Harness) string {
	if len(harnesses) > 0 && harnesses[0].Harness != kind {
		return " --harness " + string(kind)
	}
	return ""
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (q *options) credentialInput() localsetup.CredentialInput {
	switch {
	case q.credentialFile != "":
		return localsetup.CredentialInput{File: q.credentialFile}
	case q.credentialStdin:
		return localsetup.CredentialInput{Stdin: os.Stdin}
	}
	return localsetup.CredentialInput{Prompt: secret}
}

// secret reads a value from the terminal without echoing it.
func secret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no terminal to ask for the key; use --credential-file or --credential-stdin")
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s (input hidden): ", label)
	value, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(os.Stderr)
	return string(value), err
}

// installation loads the layout, journal and configuration for maintenance.
func installation(root string) (localsetup.Layout, localsetup.Installation, home.Config, error) {
	resolved, err := localsetup.ResolveRoot(root)
	if err != nil {
		return localsetup.Layout{}, localsetup.Installation{}, home.Config{}, err
	}
	l := localsetup.Layout{Root: resolved}
	if _, err = os.Stat(l.Installation()); err != nil {
		err = fmt.Errorf("no Sproozi installation at %s; run \"sproozi init\" to set one up, or choose another with --root",
			resolved)
		return l, localsetup.Installation{}, home.Config{}, err
	}
	inst, err := localsetup.LoadInstallation(l)
	if err != nil {
		return l, inst, home.Config{}, err
	}
	c, err := home.ReadConfig(l.Config())
	if os.IsNotExist(err) {
		err = incompleteSetup(resolved)
	}
	return l, inst, c, err
}

// incompleteSetup reports an installation whose setup stopped before it
// wrote the configuration.
func incompleteSetup(root string) error {
	return fmt.Errorf("setup at %s is incomplete: it stopped before writing config.json; run \"sproozi init\" "+
		"to finish it", root)
}

// printer writes a setup command's result as text.
type printer interface{ text(io.Writer) }

// maintain runs service, doctor, credentials, images and certificates.
func (q *options) maintain(ctx context.Context) error {
	l, inst, c, err := installation(q.root)
	if err != nil {
		return err
	}
	var out printer
	switch q.action() {
	case cmdService:
		out, err = q.serviceCommand(ctx, l, c)
	case cmdDoctor:
		report := localsetup.Doctor(ctx, l, q.checkProvider)
		out = doctorReport(report)
		if !report.OK {
			err = errors.New("one or more checks failed")
		}
	case cmdCredentials:
		out, err = q.credentialsCommand(ctx, l, inst)
	case cmdImages:
		out, err = q.imagesCommand(ctx, l, inst)
	case cmdCertificates:
		out, err = q.certificatesCommand(ctx, l, c)
	}
	if out != nil {
		if q.json {
			if e := writeJSON(q.out, out); e != nil {
				return e
			}
		} else {
			out.text(q.out)
		}
	}
	return err
}

// serviceReport is the service's state and what this command did to it.
type serviceReport struct {
	localsetup.ServiceStatus
	// Changed says this command started or stopped the service.
	Changed bool `json:"changed"`
	verb    string
}

func (s serviceReport) text(w io.Writer) {
	switch {
	case s.verb == verbStop && s.Changed:
		_, _ = fmt.Fprintln(w, "Stopped the Sproozi service.")
	case s.verb == verbStop:
		_, _ = fmt.Fprintln(w, "The Sproozi service was not running.")
	case s.verb == verbStart && s.Changed:
		_, _ = fmt.Fprintf(w, "Started the Sproozi service (process %d).\n", s.PID)
	case s.Running:
		_, _ = fmt.Fprintf(w, "The Sproozi service is running (process %d) with %d active session(s).\n",
			s.PID, len(s.Active))
	default:
		_, _ = fmt.Fprintln(w, "The Sproozi service is not running. Session commands start it when they need it;\n"+
			"to start it now: sproozi service start")
	}
	if s.RestartPending != "" {
		_, _ = fmt.Fprintln(w, "Restart pending: "+s.RestartPending+".")
	}
	if s.Error != "" {
		_, _ = fmt.Fprintln(w, "Problem: "+s.Error)
	}
	_, _ = fmt.Fprintln(w, "Log: "+s.Log)
}

func (q *options) serviceCommand(ctx context.Context, l localsetup.Layout, c home.Config) (printer, error) {
	before := localsetup.ServiceState(ctx, l, c)
	report := serviceReport{verb: q.verb}
	var err error
	switch q.verb {
	case verbStart:
		executable, e := os.Executable()
		if e != nil {
			return nil, e
		}
		if report.ServiceStatus, err = localsetup.StartService(ctx, l, c, executable, 2*time.Minute); err != nil {
			return nil, err
		}
		report.Changed = !before.Running
	case verbStop:
		// A refused stop prints only its reason: the service still runs.
		if report.ServiceStatus, err = localsetup.StopService(ctx, l, c, q.leaveRunning, time.Minute); err != nil {
			return nil, err
		}
		report.Changed = before.Running
	default:
		report.ServiceStatus = before
	}
	return report, err
}

type doctorReport localsetup.Report

func (r doctorReport) text(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAIL"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, c.Component, c.Detail)
		if c.Next != "" {
			_, _ = fmt.Fprintf(tw, "\t\tnext: %s\n", c.Next)
		}
	}
	_ = tw.Flush()
	if r.OK {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	}
}

// providers maps each agent and provider name to the provider key it uses,
// from the harness registry's model protocols.
var providers = func() map[string]string {
	out := map[string]string{providerOpenAI: providerOpenAI, providerAnthropic: providerAnthropic}
	for _, kind := range harness.ManagedKinds() {
		h, _ := harness.Managed(kind)
		out[string(kind)] = string(h.Protocol)
	}
	return out
}()

const (
	providerOpenAI    = string(harness.ProtocolOpenAI)
	providerAnthropic = string(harness.ProtocolAnthropic)
)

// credentialEntry reports one provider's stored key, never its value.
type credentialEntry struct {
	Provider  string   `json:"provider"`
	Harnesses []string `json:"harnesses,omitempty"`
	Stored    bool     `json:"stored"`
	Checked   bool     `json:"checked,omitempty"`
	Service   string   `json:"service,omitempty"`
}

type credentialReport []credentialEntry

func (r credentialReport) text(w io.Writer) {
	if len(r) == 1 && r[0].Service != "" {
		e := r[0]
		checked := ""
		if e.Checked {
			checked = "; the provider accepted it"
		}
		_, _ = fmt.Fprintf(w, "Stored the %s key%s. The service is %s.\n", e.Provider, checked, e.Service)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROVIDER\tKEY\tUSED BY")
	for _, e := range r {
		stored := "missing"
		if e.Stored {
			stored = "stored"
		}
		used := strings.Join(e.Harnesses, ", ")
		if used == "" {
			used = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Provider, stored, used)
	}
	_ = tw.Flush()
}

func (q *options) credentialsCommand(ctx context.Context, l localsetup.Layout,
	inst localsetup.Installation) (printer, error) {
	if q.verb == verbList {
		report := make(credentialReport, 0, 2)
		for _, provider := range []string{providerOpenAI, providerAnthropic} {
			e := credentialEntry{Provider: provider, Stored: localsetup.HasCredential(l, provider)}
			for _, h := range inst.Harnesses {
				if localsetup.ProviderFor(h.Harness).Name == provider && h.Credential != "" {
					e.Harnesses = append(e.Harnesses, string(h.Harness))
				}
			}
			report = append(report, e)
		}
		return report, nil
	}
	provider, ok := providers[q.operand]
	if !ok {
		return nil, usagef("credentials set takes codex, claude-code, openai or anthropic")
	}
	var selected *localsetup.Harness
	for i := range inst.Harnesses {
		if localsetup.ProviderFor(inst.Harnesses[i].Harness).Name != provider {
			continue
		}
		if inst.Harnesses[i].Credential == "" {
			return nil, usagef("%s signs in to %s itself, so Sproozi stores no key for it; run /login in its session",
				inst.Harnesses[i].Harness, provider)
		}
		selected = &inst.Harnesses[i]
	}
	path, err := localsetup.StoreCredential(l, provider, q.credentialInput())
	if err != nil {
		return nil, err
	}
	entry := credentialEntry{Provider: provider, Stored: true}
	if selected != nil {
		entry.Harnesses = []string{string(selected.Harness)}
		if !q.skipCheck {
			err = localsetup.CheckCredential(ctx, selected.Harness, selected.ProviderURL, selected.ProviderCAFile, path)
			if err != nil {
				return nil, err
			}
			entry.Checked = true
		}
	}
	entry.Service, err = restartForCredential(ctx, l)
	return credentialReport{entry}, err
}

// restartForCredential restarts the service when no session is active. It
// reads provider keys only when it starts.
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
	var active *localsetup.SessionsActiveError
	switch {
	case errors.As(err, &active):
		return "still using the previous key: " + err.Error(), nil
	case err != nil:
		return "still using the previous key", err
	case restarted:
		return "restarted with the new key", nil
	}
	return "not running; it uses the key when it starts", nil
}

// imageReport reports the installed and release images, or a prepared one.
type imageReport struct {
	Installed *localsetup.Image `json:"installed,omitempty"`
	Release   string            `json:"release,omitempty"`
	Prepared  *localsetup.Image `json:"prepared,omitempty"`
	Active    bool              `json:"active,omitempty"`
	Note      string            `json:"note,omitempty"`
}

func (r imageReport) text(w io.Writer) {
	if r.Prepared != nil {
		_, _ = fmt.Fprintf(w, "Prepared %s (%s).\n", r.Prepared.Reference, r.Prepared.ID)
		if r.Note != "" {
			_, _ = fmt.Fprintln(w, upperFirst(r.Note)+".")
		}
		return
	}
	installed := "none"
	if r.Installed != nil {
		installed = fmt.Sprintf("%s (%s, from %s)", r.Installed.Reference, r.Installed.ID, r.Installed.Source)
	}
	release := r.Release
	if release == "" {
		release = "none: this is a development build; use --image or --build"
	}
	_, _ = fmt.Fprintf(w, "Installed  %s\nRelease    %s\n", installed, release)
}

func (q *options) imagesCommand(ctx context.Context, l localsetup.Layout,
	inst localsetup.Installation) (printer, error) {
	if q.verb == verbStatus {
		return imageReport{Installed: inst.Image, Release: localsetup.ReleaseImage()}, nil
	}
	request := localsetup.ImageRequest{Reference: q.image, BuildSource: q.build, Progress: os.Stderr}
	image, active, err := localsetup.PrepareImage(ctx, l, request)
	if err != nil {
		return nil, err
	}
	result := imageReport{Prepared: &image, Active: active}
	if !active {
		result.Note = "prepared but not in use: retained sessions keep the current image; changing it needs them " +
			"removed first"
	}
	return result, nil
}

type certificateReport localsetup.CertificateStatus

func (s certificateReport) text(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Authority     generation %d, fingerprint %s\n", s.Serving, s.Fingerprint)
	_, _ = fmt.Fprintf(w, "              expires %s; rotation begins %s\n", formatTime(s.CANotAfter),
		formatTime(s.RotateAt))
	_, _ = fmt.Fprintf(w, "Certificate   expires %s; renews %s\n", formatTime(s.LeafNotAfter), formatTime(s.RenewAt))
	if s.Rotation != nil {
		waiting := "nothing"
		if len(s.Waiting) > 0 {
			waiting = "sessions " + strings.Join(s.Waiting, ", ")
		}
		_, _ = fmt.Fprintf(w, "Rotation      to generation %d, waiting for %s\n", s.Rotation.To, waiting)
	}
}

func (q *options) certificatesCommand(ctx context.Context, l localsetup.Layout, c home.Config) (printer, error) {
	store, err := localsetup.PKIStore(l)
	if err != nil {
		return nil, err
	}
	switch q.verb {
	case verbRenew:
		err = store.Renew()
	case verbRotateCA:
		// The running service switches and retires once every running
		// session has the new trust or has independently stopped.
		err = store.PrepareRotation()
	case verbReplaceCA:
		if status := localsetup.ServiceState(ctx, l, c); len(status.Active) > 0 {
			return nil, fmt.Errorf("replacing the authority would remove trust from %d running session(s); "+
				"cancel them first", len(status.Active))
		}
		// The service may be stopped while agents keep running.
		running, countErr := localsetup.RunningContainers(ctx, l)
		if countErr != nil || running > 0 {
			return nil, errors.Join(countErr, fmt.Errorf("%d container(s) still run on the installation's network; "+
				"cancel or stop their sessions first", running))
		}
		err = store.ReplaceCA()
	}
	if err != nil {
		return nil, err
	}
	status, err := localsetup.Certificates(store)
	return certificateReport(status), err
}

// versionReport is what "version" prints.
type versionReport struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	Date        string `json:"date"`
	NativeImage string `json:"nativeImage,omitempty"`
}

func (q *options) version() error {
	if q.json {
		return writeJSON(q.out, versionReport{Version: build.version, Commit: build.commit, Date: build.date,
			NativeImage: localsetup.ReleaseImage()})
	}
	_, err := fmt.Fprintln(q.out, build)
	if image := localsetup.ReleaseImage(); err == nil && image != "" {
		_, err = fmt.Fprintf(q.out, "agent image %s\n", image)
	}
	return err
}
