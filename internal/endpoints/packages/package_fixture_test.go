package packages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	fixtureWheelFilename = "demo_tool-1.0.0-py3-none-any.whl"
)

const (
	fixturePackageName = "demo-tool"
)

const (
	fixturePackageVersion = "1.0.0"
)

func TestPackageWheelIsVerifiedBeforeForwarding(t *testing.T) {
	wheel := []byte("fixture-wheel")
	digest := sha256.Sum256(wheel)
	identity := protocolIdentity()
	identity.Run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	identity.Policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	identity.Policy.Spec.PackagesInstall = sprooziv1alpha1.PackagesInstallScope{
		Ecosystems:       []sprooziv1alpha1.PackageEcosystem{sprooziv1alpha1.PackageEcosystemPyPI},
		MaxDownloadBytes: 1024,
		Artifacts: []sprooziv1alpha1.PackageArtifact{{
			Name: fixturePackageName, Version: fixturePackageVersion, Filename: fixtureWheelFilename, SHA256: hex.EncodeToString(digest[:]),
		}},
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://files.pythonhosted.org/packages/f/fixture/demo_tool-1.0.0-py3-none-any.whl" {
			t.Fatalf("unexpected package upstream URL: %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(wheel)), Request: request}, nil
	})}
	h := protocolHandler(client, &protocolAuditWriter{})
	request := httptest.NewRequest(http.MethodGet, "https://files.pythonhosted.org/packages/f/fixture/demo_tool-1.0.0-py3-none-any.whl", nil)
	request.Host = pythonFilesHost
	request.Header.Set("Authorization", "Bearer projected-run-token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request.WithContext(gateway.WithIdentity(request.Context(), identity)))
	if response.Code != http.StatusOK || response.Body.String() != string(wheel) {
		t.Fatalf("verified wheel response = %d %q", response.Code, response.Body.String())
	}
}

func TestPackageWheelDigestMismatchIsDenied(t *testing.T) {
	identity := protocolIdentity()
	identity.Run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	identity.Policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	identity.Policy.Spec.PackagesInstall = sprooziv1alpha1.PackagesInstallScope{
		Ecosystems:       []sprooziv1alpha1.PackageEcosystem{sprooziv1alpha1.PackageEcosystemPyPI},
		MaxDownloadBytes: 1024,
		Artifacts:        []sprooziv1alpha1.PackageArtifact{{Name: fixturePackageName, Version: fixturePackageVersion, Filename: fixtureWheelFilename, SHA256: strings.Repeat("a", 64)}},
	}
	h := protocolHandler(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not-the-wheel")), Request: request}, nil
	})}, &protocolAuditWriter{})
	request := httptest.NewRequest(http.MethodGet, "https://files.pythonhosted.org/packages/f/demo_tool-1.0.0-py3-none-any.whl", nil)
	request.Host = pythonFilesHost
	request.Header.Set("Authorization", "Bearer projected-run-token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request.WithContext(gateway.WithIdentity(request.Context(), identity)))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "artifact verification") {
		t.Fatalf("digest mismatch response = %d %q", response.Code, response.Body.String())
	}
}

func TestPackageCapabilityDoesNotInheritGitHubGrant(t *testing.T) {
	identity := protocolIdentity()
	identity.Run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}
	identity.Policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}
	h := protocolHandler(&http.Client{}, &protocolAuditWriter{})
	request := httptest.NewRequest(http.MethodGet, "https://pypi.org/simple/demo-tool/", nil)
	request.Host = "pypi.org"
	request.Header.Set("Authorization", "Bearer projected-run-token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request.WithContext(gateway.WithIdentity(request.Context(), identity)))
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "packages.install") {
		t.Fatalf("missing package capability response = %d %q", response.Code, response.Body.String())
	}
}

func TestPackageRedirectIsNotFollowed(t *testing.T) {
	identity := protocolIdentity()
	identity.Run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	identity.Policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	identity.Policy.Spec.PackagesInstall = sprooziv1alpha1.PackagesInstallScope{
		Ecosystems: []sprooziv1alpha1.PackageEcosystem{sprooziv1alpha1.PackageEcosystemPyPI}, MaxDownloadBytes: 1024,
		Artifacts: []sprooziv1alpha1.PackageArtifact{{Name: fixturePackageName, Version: fixturePackageVersion, Filename: fixtureWheelFilename, SHA256: strings.Repeat("a", 64)}},
	}
	requests := 0
	h := protocolHandler(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://evil.example/other.whl"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: request}, nil
	})}, &protocolAuditWriter{})
	request := httptest.NewRequest(http.MethodGet, "https://files.pythonhosted.org/packages/f/demo_tool-1.0.0-py3-none-any.whl", nil)
	request.Host = pythonFilesHost
	request.Header.Set("Authorization", "Bearer projected-run-token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request.WithContext(gateway.WithIdentity(request.Context(), identity)))
	if response.Code != http.StatusBadGateway || requests != 1 || !strings.Contains(response.Body.String(), "redirect") {
		t.Fatalf("redirect handling = status %d requests %d body %q", response.Code, requests, response.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type protocolAuditWriter struct{}

func (*protocolAuditWriter) Log(audit.Event) {}
func protocolIdentity() *gateway.RunIdentity {
	return &gateway.RunIdentity{Run: &sprooziv1alpha1.AgentRun{}, Policy: &sprooziv1alpha1.AgentPolicy{}}
}
func protocolHandler(client *http.Client, logger audit.Writer) *Handler {
	return &Handler{Config: HandlerConfig{HTTPClient: client, AuditLogger: logger}}
}
