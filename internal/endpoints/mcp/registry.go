// Package mcp bridges registered remote tools under named run capabilities.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/idna"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/mcppolicy"
)

type Registry struct{ servers map[string]registration }

// CredentialDirectory is mounted only in the trusted gateway. Registry edits
// cannot turn unrelated gateway files into bearer credentials.
const CredentialDirectory = "/var/run/secrets/sproozi/mcp"

type registration struct {
	endpoint *url.URL
	token    string
}

// LoadRegistry snapshots endpoints and token bytes together. Registration and
// credential changes require gateway restart; policy remains live.
func LoadRegistry(data []byte, credentialsDir string) (*Registry, error) {
	if _, err := mcppolicy.DecodeObject(data); err != nil {
		return nil, fmt.Errorf("invalid MCP registry JSON")
	}
	var document struct {
		Servers map[string]struct {
			URL             string `json:"url"`
			BearerTokenFile string `json:"bearerTokenFile,omitempty"`
		} `json:"servers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || document.Servers == nil || len(document.Servers) > 32 {
		return nil, fmt.Errorf("invalid or unbounded MCP registry")
	}
	registry := &Registry{servers: map[string]registration{}}
	for name, config := range document.Servers {
		if _, valid := api.CapabilityKind("mcp." + name).MCPServerName(); !valid {
			return nil, fmt.Errorf("invalid MCP registration name")
		}
		endpoint, err := url.Parse(config.URL)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
			endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawPath != "" {
			return nil, fmt.Errorf("MCP registration %s requires an exact HTTPS URL", name)
		}
		// Canonical authority spelling keeps provider reservations equivalent
		// to destination routes: 0443 and trailing-dot DNS aliases cannot bypass it.
		host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
		if ip := net.ParseIP(host); ip != nil {
			host = ip.String()
		} else {
			host, err = idna.Lookup.ToASCII(host)
			if err != nil {
				return nil, fmt.Errorf("invalid MCP provider hostname")
			}
		}
		port := endpoint.Port()
		if port == "" {
			port = "443"
		}
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 || host == "" {
			return nil, fmt.Errorf("invalid MCP provider authority")
		}
		endpoint.Host = net.JoinHostPort(host, strconv.Itoa(value))
		reg := registration{endpoint: endpoint}
		if config.BearerTokenFile != "" {
			path, err := credentialPath(credentialsDir, config.BearerTokenFile)
			if err != nil {
				return nil, fmt.Errorf("MCP registration %s credential path is invalid", name)
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("MCP registration %s credential is unavailable", name)
			}
			data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
			closeErr := file.Close()
			reg.token = strings.TrimSpace(string(data))
			if err != nil || closeErr != nil || len(data) > 16<<10 || reg.token == "" || strings.ContainsAny(reg.token, " \t\r\n") {
				return nil, fmt.Errorf("MCP registration %s credential is invalid", name)
			}
		}
		registry.servers[name] = reg
	}
	return registry, nil
}

// Kubernetes Secret mounts use symlinks, so accept links within the dedicated
// credential directory while rejecting lexical and resolved escapes.
func credentialPath(root, path string) (string, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) || !within(root, path) {
		return "", fmt.Errorf("credential is outside MCP mount")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil || !within(resolvedRoot, resolvedPath) {
		return "", fmt.Errorf("credential link is outside MCP mount")
	}
	return resolvedPath, nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Authorities reserves remote providers against weaker destination fallback.
// These are not workload routes and do not need inspection certificates.
func (r *Registry) Authorities() []string {
	result := make([]string, 0, len(r.servers))
	for _, registration := range r.servers {
		port := registration.endpoint.Port()
		if port == "" {
			port = "443"
		}
		result = append(result, net.JoinHostPort(strings.ToLower(registration.endpoint.Hostname()), port))
	}
	slices.Sort(result)
	return slices.Compact(result)
}
