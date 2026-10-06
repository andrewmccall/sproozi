// Package packages contains the fail-closed policy and artifact checks for the
// packages.install capability. It deliberately has no network client: callers
// must validate a request before forwarding it and verify bytes before serving
// them to a sandbox.
package packages

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	pypiHost  = "pypi.org"
	methodGet = "GET"
)

const (
	pythonFilesHost = "files.pythonhosted.org"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var metadataHrefPattern = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*["']([^"']+)["']`)

// Request is the small, parsed portion of a PyPI request that policy can trust.
type Request struct {
	Method string
	Host   string
	Path   string
}

// Decision explains a package request denial without including credentials or
// arbitrary upstream data.
type Decision struct {
	Allowed  bool
	Reason   string
	Artifact *sprooziv1alpha1.PackageArtifact
}

// ValidateScope checks the administrator-provided lock before any request is
// accepted. Every dependency must itself be an exact artifact entry.
func ValidateScope(scope sprooziv1alpha1.PackagesInstallScope) error {
	if !containsEcosystem(scope.Ecosystems, sprooziv1alpha1.PackageEcosystemPyPI) || scope.AllowSourceBuilds || scope.MaxDownloadBytes <= 0 {
		return fmt.Errorf("scope must explicitly enable wheel-only PyPI downloads")
	}
	seen := map[string]struct{}{}
	for i := range scope.Artifacts {
		a := &scope.Artifacts[i]
		if !validArtifact(a) {
			return fmt.Errorf("artifact %q is invalid", a.Name)
		}
		key := a.Name + "@" + a.Version
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate artifact %s", key)
		}
		seen[key] = struct{}{}
	}
	for _, a := range scope.Artifacts {
		for _, dep := range a.Dependencies {
			name, err := NormalizeName(dep.Name)
			if err != nil || name != dep.Name || dep.Version == "" {
				return fmt.Errorf("dependency %q is invalid", dep.Name)
			}
			if _, ok := seen[dep.Name+"@"+dep.Version]; !ok {
				return fmt.Errorf("dependency %s@%s is not locked", dep.Name, dep.Version)
			}
		}
	}
	return nil
}

// NormalizeName applies the PEP 503 canonicalization used by the simple API.
func NormalizeName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.Trim(name, "-")
	name = regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-")
	if name == "" || len(name) > 128 || strings.ContainsAny(name, `/\\`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("invalid package name")
	}
	return name, nil
}

// AuthorizePath allows only the pinned PyPI simple metadata path or one exact
// wheel path from the policy. It rejects query strings, fragments, redirects,
// source distributions, uploads, searches, and arbitrary CDN paths.
func AuthorizePath(scope sprooziv1alpha1.PackagesInstallScope, req Request) Decision {
	if req.Method != methodGet && req.Method != "HEAD" {
		return Decision{Reason: "method_not_allowed"}
	}
	if req.Host != pypiHost && req.Host != pythonFilesHost {
		return Decision{Reason: "host_not_allowed"}
	}
	if !containsEcosystem(scope.Ecosystems, sprooziv1alpha1.PackageEcosystemPyPI) || scope.AllowSourceBuilds || scope.MaxDownloadBytes <= 0 {
		return Decision{Reason: "package_scope_unavailable"}
	}
	if strings.ContainsAny(req.Path, "?#") || strings.Contains(req.Path, "..") {
		return Decision{Reason: "ambiguous_path"}
	}
	if after, ok := strings.CutPrefix(req.Path, "/simple/"); ok {
		name := strings.TrimSuffix(after, "/")
		normalized, err := NormalizeName(name)
		if err != nil || normalized != name {
			return Decision{Reason: "package_metadata_not_normalized"}
		}
		if !hasArtifactName(scope.Artifacts, normalized) {
			return Decision{Reason: "package_not_allowlisted"}
		}
		return Decision{Allowed: true}
	}
	for i := range scope.Artifacts {
		a := &scope.Artifacts[i]
		if a.Filename != "" && strings.HasSuffix(req.Path, "/"+a.Filename) {
			if !validArtifact(a) {
				return Decision{Reason: "artifact_policy_invalid"}
			}
			return Decision{Allowed: true, Artifact: a}
		}
	}
	return Decision{Reason: "artifact_not_allowlisted"}
}

// VerifyArtifact reads at most MaxDownloadBytes+1 bytes and verifies the exact
// configured wheel digest. No bytes are returned when the check fails.
func VerifyArtifact(scope sprooziv1alpha1.PackagesInstallScope, artifact *sprooziv1alpha1.PackageArtifact, r io.Reader) ([]byte, error) {
	if artifact == nil || scope.MaxDownloadBytes <= 0 || !validArtifact(artifact) {
		return nil, fmt.Errorf("artifact policy is invalid")
	}
	limit := scope.MaxDownloadBytes
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("artifact exceeds download limit")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return nil, fmt.Errorf("artifact digest mismatch")
	}
	return b, nil
}

// VerifyMetadata bounds and validates a PyPI simple-index response. Every
// wheel link must resolve to an exact artifact in the lock; unlisted wheels,
// source distributions, credentials, query parameters, and unexpected hosts
// are rejected before metadata reaches pip.
func VerifyMetadata(scope sprooziv1alpha1.PackagesInstallScope, r io.Reader) ([]byte, error) {
	if scope.MaxDownloadBytes <= 0 {
		return nil, fmt.Errorf("package scope is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(r, scope.MaxDownloadBytes+1))
	if err != nil || int64(len(body)) > scope.MaxDownloadBytes {
		return nil, fmt.Errorf("metadata exceeds download limit")
	}
	matches := metadataHrefPattern.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("metadata contains no artifact links")
	}
	for _, match := range matches {
		raw := string(match[1])
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || (u.Host != pypiHost && u.Host != pythonFilesHost) {
			return nil, fmt.Errorf("metadata link is outside the approved registry")
		}
		filename := ""
		for _, artifact := range scope.Artifacts {
			if artifact.Filename != "" && strings.HasSuffix(u.Path, "/"+artifact.Filename) {
				filename = artifact.Filename
				if u.Fragment != "" && u.Fragment != "sha256="+artifact.SHA256 {
					return nil, fmt.Errorf("metadata digest fragment does not match artifact")
				}
				break
			}
		}
		if filename == "" {
			return nil, fmt.Errorf("metadata contains an unlisted artifact")
		}
	}
	return body, nil
}

func validArtifact(a *sprooziv1alpha1.PackageArtifact) bool {
	name, err := NormalizeName(a.Name)
	return err == nil && name == a.Name && validVersion(a.Version) && validFilename(a.Filename) && digestPattern.MatchString(a.SHA256)
}

func validVersion(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\?#\x00\r\n") {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validFilename(value string) bool {
	if value == "" || len(value) > 255 || strings.ContainsAny(value, "/\\?#\x00\r\n") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(value), ".whl")
}

func hasArtifactName(artifacts []sprooziv1alpha1.PackageArtifact, name string) bool {
	for i := range artifacts {
		if artifacts[i].Name == name && validArtifact(&artifacts[i]) {
			return true
		}
	}
	return false
}

func containsEcosystem(ecosystems []sprooziv1alpha1.PackageEcosystem, want sprooziv1alpha1.PackageEcosystem) bool {
	return slices.Contains(ecosystems, want)
}

// ArtifactURL validates that a URL is HTTPS, has no credentials/query/fragment,
// and points to the exact filename approved by the policy. Redirect targets must
// be passed through this function again; callers must not follow redirects blindly.
func ArtifactURL(raw string, artifact sprooziv1alpha1.PackageArtifact) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Host != pythonFilesHost && u.Host != pypiHost) {
		return fmt.Errorf("artifact URL is outside the approved registry")
	}
	if !validArtifact(&artifact) || !strings.HasSuffix(u.Path, "/"+artifact.Filename) {
		return fmt.Errorf("artifact URL does not match the approved artifact")
	}
	return nil
}
