// Package home composes the trusted local owner, fixed placements and home journal.
// Its private Unix socket is an administrator control channel, never mounted in workers.
package home

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

type Config struct {
	Directory        string                          `json:"directory"`
	DockerBinary     string                          `json:"dockerBinary"`
	DockerSocket     string                          `json:"dockerSocket"`
	DockerConfig     string                          `json:"dockerConfig"`
	Local            runtimedocker.PlacementConfig   `json:"local"`
	Allowances       map[string]session.Allowance    `json:"allowances"`
	MaxDuration      string                          `json:"maxDuration"`
	Model            string                          `json:"model"`
	NativeOptions    []string                        `json:"nativeOptions,omitempty"`
	Gateway          GatewayConfig                   `json:"gateway"`
	Remote           map[string]profile.ClientConfig `json:"remote,omitempty"`
	WorkSources      map[string]profile.ClientConfig `json:"workSources,omitempty"`
	ProjectApprovals []ProjectApproval               `json:"projectApprovals,omitempty"`
}

// WorkConnections resolves remote work owners by name, for run, discovery,
// remote control and pull alike. A workSources entry wins; any other name uses
// its remote profile.
func (c Config) WorkConnections() map[string]profile.ClientConfig {
	connections := make(map[string]profile.ClientConfig, len(c.Remote)+len(c.WorkSources))
	maps.Copy(connections, c.Remote)
	maps.Copy(connections, c.WorkSources)
	return connections
}

// ProjectApproval is current local authority, qualified by authenticated source.
// It narrows home ceilings independently of imported project metadata.
type ProjectApproval struct {
	Source       string     `json:"source"`
	Project      project.ID `json:"project"`
	Capabilities []string   `json:"capabilities"`
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

func readPrivate(path string, max int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("home: absolute private file required")
	}
	return privatefs.ReadFile(path, max)
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
	if err = validateProjectApprovals(c); err != nil {
		return c, err
	}
	return c, nil
}
