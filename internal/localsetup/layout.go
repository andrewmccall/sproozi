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
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// Layout is the private per-user local installation.
//
//	config.json        trusted home configuration
//	installation.json  provisioning journal and owned resource identities
//	agents/            shared native agent settings (never credentials or keys)
//	credentials/       gateway-owned upstream credentials; never worker-mounted
//	pki/               private CA keys, serving leaf and public trust
//	state/             home journals, accounting, grants and checkpoints
//	docker-cli/        private Docker CLI configuration
//	logs/              private service logs
type Layout struct{ Root string }

func (l Layout) Config() string       { return filepath.Join(l.Root, "config.json") }
func (l Layout) Installation() string { return filepath.Join(l.Root, "installation.json") }
func (l Layout) Agents() string       { return filepath.Join(l.Root, "agents") }
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

// DefaultConfigPath keeps an existing platform configuration authoritative.
// New installations use the local root's config.json.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	platform := filepath.Join(dir, "sproozi", "config.json")
	if _, err = os.Lstat(platform); err == nil {
		return platform, nil
	}
	root, err := ResolveRoot("")
	if err != nil {
		return "", err
	}
	local := Layout{Root: root}.Config()
	if _, err = os.Lstat(local); err == nil {
		return local, nil
	}
	return platform, nil
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

// ensureDirectories creates the private layout. Existing directories must
// already be private; they are never repaired.
func (l Layout) ensureDirectories() error {
	for _, dir := range []string{l.Root, l.Credentials(), l.PKI(), l.State(), l.DockerCLI(), l.Logs(), l.Agents()} {
		if err := privatefs.EnsureDir(dir); err != nil {
			return fmt.Errorf("private directory %s: %w", dir, err)
		}
	}
	// Shared native settings. Workers receive only allowlisted instruction
	// files from these directories, freshly on every visit.
	for _, name := range append(home.AgentFiles(harness.NativeCodex), home.AgentFiles(harness.NativeClaude)...) {
		if err := privatefs.EnsureDir(filepath.Join(l.Agents(), filepath.Dir(name))); err != nil {
			return err
		}
	}
	return nil
}
