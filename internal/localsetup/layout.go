// Package localsetup provisions a local Sproozi installation from a working
// Docker engine: images, an owned network, a probed gateway endpoint, a private
// CA, provider credentials, home configuration and a supervised home daemon.
// It never reads cluster credentials or contacts a Kubernetes API.
package localsetup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// Layout is the private per-user local installation.
//
//	config.json        trusted home configuration
//	installation.json  provisioning journal and owned resource identities
//	profile/           the user profile every session receives (never credentials or keys)
//	credentials/       gateway-owned upstream credentials; never worker-mounted
//	pki/               private CA keys, serving leaf and public trust
//	state/             home journals, accounting, grants and checkpoints
//	docker-cli/        private Docker CLI configuration
//	logs/              private service logs
type Layout struct{ Root string }

func (l Layout) Config() string       { return filepath.Join(l.Root, "config.json") }
func (l Layout) Installation() string { return filepath.Join(l.Root, "installation.json") }
func (l Layout) Profile() string      { return filepath.Join(l.Root, home.ProfileFolder) }
func (l Layout) Credentials() string  { return filepath.Join(l.Root, "credentials") }
func (l Layout) PKI() string          { return filepath.Join(l.Root, "pki") }
func (l Layout) State() string        { return filepath.Join(l.Root, "state") }
func (l Layout) DockerCLI() string    { return filepath.Join(l.Root, "docker-cli") }
func (l Layout) Logs() string         { return filepath.Join(l.Root, "logs") }
func (l Layout) ServiceLog() string   { return filepath.Join(l.Logs(), "service.log") }
func (l Layout) ServicePID() string   { return filepath.Join(l.Root, "service.pid") }

// DefaultRoot is ~/sproozi.
func DefaultRoot() (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sproozi"), nil
}

// pointerPath records an explicit non-default root for later commands.
func pointerPath() (string, error) {
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "sproozi", "root"), err
}

// ResolveRoot returns the explicit root, else the recorded root, else ~/sproozi.
func ResolveRoot(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if pointer, err := pointerPath(); err == nil {
		if data, err := privatefs.ReadFile(pointer, 4096); err == nil {
			root := strings.TrimSpace(string(data))
			if filepath.IsAbs(root) {
				return root, nil
			}
		}
	}
	return DefaultRoot()
}

// RememberRoot records a non-default root so later commands find it.
func RememberRoot(root string) error {
	def, err := DefaultRoot()
	if err != nil || root == def {
		return err
	}
	pointer, err := pointerPath()
	if err != nil {
		return err
	}
	if err = privatefs.EnsureDir(filepath.Dir(pointer)); err != nil {
		return err
	}
	return privatefs.Replace(filepath.Dir(pointer), filepath.Base(pointer), []byte(root+"\n"))
}

// DefaultConfigPath keeps an existing platform configuration authoritative
// unless it can do nothing yet and a local installation exists: then the
// local installation's config.json wins. New installations use it too.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	platform := filepath.Join(dir, "sproozi", "config.json")
	_, platformErr := os.Lstat(platform)
	root, err := ResolveRoot("")
	if err != nil {
		return "", err
	}
	l := Layout{Root: root}
	_, localErr := os.Lstat(l.Config())
	_, installErr := os.Lstat(l.Installation())
	installed := localErr == nil && installErr == nil
	switch {
	case platformErr == nil && (!installed || !unconfigured(platform)):
		return platform, nil
	case localErr == nil:
		return l.Config(), nil
	}
	return platform, nil
}

// unconfigured reports a configuration that can do nothing yet: home would
// refuse to serve it and it names no remote executor, as "sproozi init" first
// writes it. A configuration home cannot read is not unconfigured, so its
// error still reaches the user.
func unconfigured(path string) bool {
	c, err := home.ReadConfig(path)
	return err == nil && len(c.ServeProblems()) > 0 && len(c.WorkConnections()) == 0
}

// Installation is the provisioning journal. Each stage records what it
// created or verified so an interrupted setup resumes without duplicates.
type Installation struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	Docker    *Docker          `json:"docker,omitempty"`
	Image     *Image           `json:"image,omitempty"`
	Network   *Network         `json:"network,omitempty"`
	Endpoint  *Endpoint        `json:"endpoint,omitempty"`
	Harnesses []Harness        `json:"harnesses,omitempty"`
	Stages    map[string]Stage `json:"stages,omitempty"`
}

// Stage records a completed provisioning step.
type Stage struct {
	Completed time.Time `json:"completed"`
}

// Endpoint is the gateway address workers reach and the host bind serving it.
type Endpoint struct {
	GatewayIPv4 string `json:"gatewayIPv4"`
	ListenHost  string `json:"listenHost"`
	Method      string `json:"method"`
}

// Harness is one configured native harness and its gateway port.
type Harness struct {
	Harness        harness.NativeKind `json:"harness"`
	Name           string             `json:"name"`
	Port           uint16             `json:"port"`
	Model          string             `json:"model"`
	ProviderURL    string             `json:"providerURL"`
	ProviderCAFile string             `json:"providerCAFile,omitempty"`
	Credential     string             `json:"credential"`
}

func (i *Installation) harness(kind harness.NativeKind) *Harness {
	for index := range i.Harnesses {
		if i.Harnesses[index].Harness == kind {
			return &i.Harnesses[index]
		}
	}
	return nil
}

func (i *Installation) done(stage string) {
	if i.Stages == nil {
		i.Stages = map[string]Stage{}
	}
	i.Stages[stage] = Stage{Completed: time.Now().UTC()}
}

// LoadInstallation reads the journal, creating a new identity when absent.
func LoadInstallation(l Layout) (Installation, error) {
	data, err := privatefs.ReadFile(l.Installation(), 256<<10)
	if os.IsNotExist(err) {
		id := make([]byte, 4)
		if _, err = rand.Read(id); err != nil {
			return Installation{}, err
		}
		return Installation{Version: 1, ID: hex.EncodeToString(id)}, nil
	}
	if err != nil {
		return Installation{}, err
	}
	var i Installation
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&i); err != nil || i.Version != 1 || len(i.ID) != 8 {
		return Installation{}, fmt.Errorf("installation journal %s is corrupt; move it aside to start over", l.Installation())
	}
	return i, nil
}

func (l Layout) save(i Installation) error {
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	return privatefs.Replace(l.Root, filepath.Base(l.Installation()), append(data, '\n'))
}

// lock holds the single setup owner for this root.
func (l Layout) lock() (func(), error) {
	f, err := privatefs.Lock(filepath.Join(l.Root, "setup.lock"))
	if err != nil {
		return nil, errors.New("another sproozi init is running for this installation")
	}
	return func() { _ = privatefs.UnlockClose(f) }, nil
}

// rootEntries are the names a local installation root holds. ".DS_Store" is
// what macOS Finder leaves in any folder it shows.
var rootEntries = []string{"config.json", "installation.json", home.ProfileFolder, "credentials", "pki", "state",
	"docker-cli", "logs", "service.pid", "setup.lock", ".DS_Store"}

// checkRoot refuses a root that holds anything other than a Sproozi
// installation, such as a Git checkout. Setup writes credentials and keys
// there, and removing the installation removes the whole directory, so the
// installation must own it. A missing or empty root is accepted.
func (l Layout) checkRoot() error {
	entries, err := os.ReadDir(l.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fail("root", err, "choose a new directory with --root PATH")
	}
	var foreign []string
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			return fail("root", fmt.Errorf("%s is a Git checkout, not a Sproozi installation", l.Root),
				"choose a directory of its own for the installation with --root PATH, such as --root ~/.sproozi; "+
					"setup never writes credentials or keys into a checkout")
		}
		if !slices.Contains(rootEntries, name) && !strings.HasPrefix(name, "config.json.backup-") {
			foreign = append(foreign, name)
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	shown := strings.Join(foreign[:min(len(foreign), 3)], ", ")
	if len(foreign) > 3 {
		shown += fmt.Sprintf(" and %d more", len(foreign)-3)
	}
	return fail("root", fmt.Errorf("%s holds %s, which are not part of a Sproozi installation", l.Root, shown),
		"choose a new or empty directory for the installation with --root PATH, such as --root ~/.sproozi")
}

// ensureDirectories creates the private layout. Existing directories must
// already be private; they are never repaired.
func (l Layout) ensureDirectories() error {
	for _, dir := range []string{l.Root, l.Credentials(), l.PKI(), l.State(), l.DockerCLI(), l.Logs(), l.Profile()} {
		if err := privatefs.EnsureDir(dir); err != nil {
			return fmt.Errorf("private directory %s: %w", dir, err)
		}
	}
	// The profile's folders, so the user can see where each file goes. Each
	// launch archives the profile as it is then.
	for _, name := range append(agentprofile.HarnessFiles(), agentprofile.SkillsDirectory+"/") {
		if err := privatefs.EnsureDir(filepath.Join(l.Profile(), filepath.Dir(name))); err != nil {
			return err
		}
	}
	return nil
}
