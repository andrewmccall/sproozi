// Package home composes the trusted local owner, fixed placements and home journal.
// Its private Unix socket is an administrator control channel, never mounted in workers.
package home

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

type Config struct {
	Directory     string                          `json:"directory"`
	DockerBinary  string                          `json:"dockerBinary"`
	DockerSocket  string                          `json:"dockerSocket"`
	DockerConfig  string                          `json:"dockerConfig"`
	Local         runtimedocker.PlacementConfig   `json:"local"`
	Allowances    map[string]session.Allowance    `json:"allowances"`
	MaxDuration   string                          `json:"maxDuration"`
	Model         string                          `json:"model"`
	NativeOptions []string                        `json:"nativeOptions,omitempty"`
	Gateway       GatewayConfig                   `json:"gateway"`
	Remote        map[string]profile.ClientConfig `json:"remote,omitempty"`
	WorkSources   map[string]profile.ClientConfig `json:"workSources,omitempty"`
}

type GatewayConfig struct {
	Listen                  string                        `json:"listen"`
	CertificateFile         string                        `json:"certificateFile"`
	KeyFile                 string                        `json:"keyFile"`
	TrustBundleFile         string                        `json:"trustBundleFile"`
	ProviderURL             string                        `json:"providerURL"`
	ProviderTokenFile       string                        `json:"providerTokenFile"`
	ProviderCAFile          string                        `json:"providerCAFile,omitempty"`
	PricingFile             string                        `json:"pricingFile,omitempty"`
	MCPAuthority            string                        `json:"mcpAuthority,omitempty"`
	MCPRegistryFile         string                        `json:"mcpRegistryFile,omitempty"`
	MCPCredentialsDirectory string                        `json:"mcpCredentialsDirectory,omitempty"`
	MCPPolicy               map[string]api.MCPServerScope `json:"mcpPolicy,omitempty"`
	GitHub                  *GitHubConfig                 `json:"github,omitempty"`
	Kubernetes              *KubernetesConfig             `json:"kubernetes,omitempty"`
}

// Credentials are private gateway-owned files, never worker environment values.
type GitHubConfig struct {
	AppID          int64                      `json:"appID"`
	InstallationID int64                      `json:"installationID"`
	PrivateKeyFile string                     `json:"privateKeyFile"`
	Scope          api.GitHubPullRequestScope `json:"scope"`
}

// KubernetesConfig selects one exact API endpoint and its restricted credential.
// It intentionally does not import kubeconfig contexts, auth plugins or exec commands.
type KubernetesConfig struct {
	UpstreamURL string                  `json:"upstreamURL"`
	TokenFile   string                  `json:"tokenFile"`
	CAFile      string                  `json:"caFile"`
	Scope       api.KubernetesReadScope `json:"scope"`
}

func private(path string, directory bool) error {
	i, err := os.Lstat(path)
	if err != nil {
		return err
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || i.Mode()&os.ModeSymlink != 0 || i.IsDir() != directory || (!directory && !i.Mode().IsRegular()) || i.Mode().Perm()&0077 != 0 || s.Uid != uint32(os.Geteuid()) {
		return errors.New("home: path must be owned and private")
	}
	return nil
}

func readPrivate(path string, max int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("home: absolute private file required")
	}
	if err := private(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("home: file exceeds bound")
	}
	return b, nil
}

func decode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("home: trailing JSON")
	}
	return nil
}

func ReadConfig(path string) (Config, error) {
	var c Config
	b, err := readPrivate(path, 256<<10)
	if err != nil {
		return c, err
	}
	if err = decode(b, &c); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.Directory) || c.Model == "" || c.Local.Name == "" || len(c.Allowances) == 0 {
		return c, errors.New("home: fixed directory, model, placement and ceilings required")
	}
	duration, err := time.ParseDuration(c.MaxDuration)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return c, errors.New("home: duration must be positive and at most 24 hours")
	}
	return c, nil
}
