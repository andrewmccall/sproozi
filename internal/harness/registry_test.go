package harness

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/nativeconfig"
)

// These guards fail on a half-added harness and name the next file to change.
// docs/contributing/add-a-harness.md lists every step.

const (
	typesFile    = "../../api/v1alpha1/agentruntime_types.go"
	crdFile      = "../../config/crd/bases/sproozi.com_agentruntimes.yaml"
	registryFile = "internal/harness/registry.go"
)

func markerSelectors(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(typesFile)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\+kubebuilder:validation:Enum=([^\n]+)\n\s*Harness string`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("no +kubebuilder:validation:Enum marker on RuntimeClientConfig.Harness in %s", typesFile)
	}
	return strings.Split(strings.TrimSpace(string(m[1])), ";")
}

func crdSelectors(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(crdFile)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^(\s+)harness:\n(?:\s+description:[\s\S]*?)?\s+enum:\n((?:\s+- [a-z0-9-]+\n)+)`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("no clientConfig.harness enum in %s; run make manifests", crdFile)
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(m[2])), "\n") {
		out = append(out, strings.TrimPrefix(strings.TrimSpace(line), "- "))
	}
	return out
}

func TestRegistryMatchesTheCRDEnum(t *testing.T) {
	marker, crd, registered := markerSelectors(t), crdSelectors(t), Selectors()
	for _, s := range registered {
		if !slices.Contains(marker, s) {
			t.Errorf("harness %q is registered in %s but missing from the +kubebuilder:validation:Enum marker on "+
				"RuntimeClientConfig.Harness in api/v1alpha1/agentruntime_types.go; add it there, then run make manifests",
				s, registryFile)
		}
	}
	for _, s := range marker {
		if !slices.Contains(registered, s) {
			t.Errorf("harness %q is admitted by the CRD enum in api/v1alpha1/agentruntime_types.go but not registered; "+
				"add a Harness entry for it to the registry in %s", s, registryFile)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(marker)), slices.Sorted(slices.Values(crd))) {
		t.Errorf("config/crd/bases/sproozi.com_agentruntimes.yaml lists %v but the marker lists %v; run make manifests",
			crd, marker)
	}
}

// Every harness receives the run's MCP servers and the sproozi control
// connection (ADR 0025, ADR 0039); an unregistered selector is refused.
func TestEveryHarnessRendersTheControlConnection(t *testing.T) {
	const gateway = "https://sproozi-gateway.test"
	for _, h := range registry {
		files, err := ContractFiles(h.Selector, []string{"mcp.github"}, gateway, "instructions", "{}")
		if err != nil {
			t.Errorf("harness %q contract: %v; fix its entry in %s", h.Selector, err, registryFile)
			continue
		}
		config, ok := files[h.MCP.File]
		if h.MCP.File == "" || !ok {
			t.Errorf("harness %q has no MCP configuration file; set MCP.File in %s", h.Selector, registryFile)
			continue
		}
		for _, want := range []string{gateway + "/mcp/sproozi", gateway + "/mcp/github"} {
			if !strings.Contains(config, want) {
				t.Errorf("harness %q's %s lacks %s; give its MCPFormat in %s an Entry that renders every connection",
					h.Selector, h.MCP.File, want, registryFile)
			}
		}
		for key := range ManagedContract() {
			if _, ok := files[key]; !ok {
				t.Errorf("harness %q's contract lacks the Sproozi integration file %s", h.Selector, key)
			}
		}
	}
	if _, err := MCPConfig("not-registered", nil, gateway); err == nil {
		t.Fatal("an unregistered harness rendered MCP configuration")
	}
}

// The managed tier is a subset of the registry, and each managed kind has the
// wiring outside this package that has no registry of its own.
func TestManagedKindsAreWired(t *testing.T) {
	directories := map[string]bool{}
	for _, h := range registry {
		if h.Native == "" {
			if nativeconfig.ValidateHarness(h.Selector, "", nil) == nil {
				t.Errorf("internal/nativeconfig accepts native session options for %q, which has no Native kind in %s",
					h.Selector, registryFile)
			}
			continue
		}
		if string(h.Native) != h.Selector {
			t.Errorf("managed harness %q must use its selector as its Native kind in %s", h.Selector, registryFile)
		}
		if h.NativeDirectory == "" || directories[h.NativeDirectory] {
			t.Errorf("managed harness %q needs its own NativeDirectory in %s", h.Selector, registryFile)
		}
		directories[h.NativeDirectory] = true
		if h.Protocol != ProtocolOpenAI && h.Protocol != ProtocolAnthropic {
			t.Errorf("managed harness %q needs a model Protocol in %s, and the gateway handler for it", h.Selector, registryFile)
		}
		if err := h.Native.Validate(); err != nil {
			t.Errorf("managed harness %q: %v", h.Selector, err)
		}
		if err := nativeconfig.ValidateHarness(h.Selector, "", nil); err != nil {
			t.Errorf("managed harness %q is not accepted by nativeconfig.ValidateHarness; add it in "+
				"internal/nativeconfig/claude.go: %v", h.Selector, err)
		}
	}
	if NativeKind("not-registered").Validate() == nil {
		t.Fatal("an unregistered managed kind validated")
	}
}
