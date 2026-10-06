package packages_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/packages"
)

const (
	testPypiHost  = "pypi.org"
	testMethodGet = "GET"
)

const (
	pythonFilesHost   = "files.pythonhosted.org"
	demoToolIndexPath = "/simple/demo-tool/"
)

func testScope(data []byte) sprooziv1alpha1.PackagesInstallScope {
	h := sha256.Sum256(data)
	return sprooziv1alpha1.PackagesInstallScope{Ecosystems: []sprooziv1alpha1.PackageEcosystem{sprooziv1alpha1.PackageEcosystemPyPI}, MaxDownloadBytes: 1024, Artifacts: []sprooziv1alpha1.PackageArtifact{{Name: "demo-tool", Version: "1.0.0", SHA256: hex.EncodeToString(h[:]), Filename: "demo_tool-1.0.0-py3-none-any.whl"}}}
}

func TestNormalizeName(t *testing.T) {
	got, err := packages.NormalizeName(" Demo_Tool ")
	if err != nil || got != "demo-tool" {
		t.Fatalf("NormalizeName() = %q, %v", got, err)
	}
}

func TestAuthorizePathOnlyAllowsExactMetadataAndWheel(t *testing.T) {
	scope := testScope([]byte("wheel"))
	for _, tc := range []struct {
		name, method, host, path string
		allowed                  bool
	}{
		{"metadata", testMethodGet, testPypiHost, demoToolIndexPath, true},
		{"wheel", testMethodGet, pythonFilesHost, "/packages/aa/demo_tool-1.0.0-py3-none-any.whl", true},
		{"wrong version", testMethodGet, pythonFilesHost, "/packages/demo_tool-2.0.0-py3-none-any.whl", false},
		{"source", testMethodGet, pythonFilesHost, "/packages/demo_tool-1.0.0.tar.gz", false},
		{"search", testMethodGet, testPypiHost, "/search?q=demo-tool", false},
		{"post", "POST", testPypiHost, demoToolIndexPath, false},
		{"wrong host", testMethodGet, "evil.example", demoToolIndexPath, false},
	} {
		d := packages.AuthorizePath(scope, packages.Request{Method: tc.method, Host: tc.host, Path: tc.path})
		if d.Allowed != tc.allowed {
			t.Errorf("%s allowed=%v reason=%q, want %v", tc.name, d.Allowed, d.Reason, tc.allowed)
		}
	}
}

func TestVerifyArtifactBoundsAndDigest(t *testing.T) {
	data := []byte("wheel-bytes")
	scope := testScope(data)
	a := &scope.Artifacts[0]
	got, err := packages.VerifyArtifact(scope, a, bytes.NewReader(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("VerifyArtifact() = %q, %v", got, err)
	}
	if _, err := packages.VerifyArtifact(scope, a, strings.NewReader("wrong")); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	scope.MaxDownloadBytes = 3
	if _, err := packages.VerifyArtifact(scope, a, bytes.NewReader(data)); err == nil {
		t.Fatal("oversized artifact was accepted")
	}
}

func TestArtifactURLRequiresExactApprovedWheel(t *testing.T) {
	scope := testScope([]byte("wheel"))
	a := scope.Artifacts[0]
	if err := packages.ArtifactURL("https://files.pythonhosted.org/packages/x/"+a.Filename, a); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://files.pythonhosted.org/packages/x/" + a.Filename, "https://files.pythonhosted.org/other.whl?x=1", "https://evil.example/" + a.Filename} {
		if err := packages.ArtifactURL(raw, a); err == nil {
			t.Errorf("accepted out-of-scope URL %q", raw)
		}
	}
}

func TestVerifyMetadataRejectsUnlistedLinks(t *testing.T) {
	scope := testScope([]byte("wheel"))
	valid := `<html><a href="https://files.pythonhosted.org/packages/x/demo_tool-1.0.0-py3-none-any.whl#sha256=` + scope.Artifacts[0].SHA256 + `">wheel</a></html>`
	if got, err := packages.VerifyMetadata(scope, strings.NewReader(valid)); err != nil || string(got) != valid {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, body := range []string{
		`<a href="https://files.pythonhosted.org/packages/x/other-1.0.whl">other</a>`,
		`<a href="http://files.pythonhosted.org/packages/x/demo_tool-1.0.0-py3-none-any.whl">insecure</a>`,
		`<a href="https://evil.example/demo_tool-1.0.0-py3-none-any.whl">evil</a>`,
	} {
		if _, err := packages.VerifyMetadata(scope, strings.NewReader(body)); err == nil {
			t.Errorf("accepted untrusted metadata link %q", body)
		}
	}
}

func TestValidateScopeRequiresCompleteLock(t *testing.T) {
	scope := testScope([]byte("wheel"))
	scope.Artifacts[0].Dependencies = []sprooziv1alpha1.PackageDependency{{Name: "missing", Version: "1.0"}}
	if err := packages.ValidateScope(scope); err == nil {
		t.Fatal("accepted incomplete dependency lock")
	}
	scope.Artifacts = append(scope.Artifacts, sprooziv1alpha1.PackageArtifact{Name: "missing", Version: "1.0", Filename: "missing-1.0-py3-none-any.whl", SHA256: strings.Repeat("a", 64)})
	if err := packages.ValidateScope(scope); err != nil {
		t.Fatalf("complete lock rejected: %v", err)
	}
}
