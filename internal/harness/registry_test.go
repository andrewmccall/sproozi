package harness

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/nativeconfig"
	"sigs.k8s.io/yaml"
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

// Each harness declares what loads its MCP file. A bridge must be one of its
// shipped launch files, with a runtime the test hook can run.
func TestEveryHarnessDeclaresItsMCPLoader(t *testing.T) {
	for _, h := range registry {
		l := h.Loader
		switch {
		case l.Native == (l.Bridge != ""):
			t.Errorf("harness %q must set exactly one of Loader.Native (its client reads %s) or Loader.Bridge "+
				"(a launch file that does) in %s", h.Selector, h.MCP.File, registryFile)
		case l.Bridge != "" && h.LaunchFiles[l.Bridge] == "":
			t.Errorf("harness %q declares MCP bridge %s, which is not one of its LaunchFiles; put it in "+
				"internal/harness/launchfiles and add it with launchFilesNamed in %s", h.Selector, l.Bridge, registryFile)
		case l.Bridge != "" && len(l.Runtime) == 0:
			t.Errorf("harness %q's MCP bridge %s needs a Loader.Runtime, such as node, in %s", h.Selector, l.Bridge,
				registryFile)
		}
		for name, content := range h.LaunchFiles {
			if strings.TrimSpace(content) == "" {
				t.Errorf("harness %q launch file %s is empty", h.Selector, name)
			}
			if shipped, err := fs.ReadFile(launchFiles, "launchfiles/"+name); err != nil || string(shipped) != content {
				t.Errorf("harness %q launch file %s is not internal/harness/launchfiles/%s; put it there and add it "+
					"with launchFilesNamed in %s, so .dockerignore ships it in the image build context",
					h.Selector, name, name, registryFile)
			}
		}
	}
}

// exampleElsewhere exempts a harness whose stock runtime example lives outside
// examples/harnesses, naming the file that shows it.
var exampleElsewhere = map[string]string{"codex": "examples/mcp/agentruntime.yaml"}

// Every harness has a stock runtime example and a row in the reference's
// selector table, which claims to match this registry.
func TestEveryHarnessHasAnExampleAndAReferenceRow(t *testing.T) {
	reference, err := os.ReadFile("../../docs/reference/harnesses.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(reference)
	if start := strings.Index(section, "## Harness selectors"); start >= 0 {
		section = section[start:]
		if end := strings.Index(section[3:], "\n## "); end >= 0 {
			section = section[:end+3]
		}
	}
	rows := map[string]string{}
	for _, m := range regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\| (yes|no) \\|").FindAllStringSubmatch(section, -1) {
		rows[m[1]] = m[2]
	}
	for _, h := range registry {
		example := "examples/harnesses/agentruntime-" + h.Selector + ".yaml"
		if other, ok := exampleElsewhere[h.Selector]; ok {
			example = other
		}
		if problem := exampleProblem(h, example); problem != "" {
			t.Errorf("harness %q: %s", h.Selector, problem)
		}
		managed := "no"
		if h.Native != "" {
			managed = "yes"
		}
		if got, ok := rows[h.Selector]; !ok || got != managed {
			t.Errorf("harness %q needs a row \"| `%s` | %s |\" in the Harness selectors table of "+
				"docs/reference/harnesses.md", h.Selector, h.Selector, managed)
		}
		delete(rows, h.Selector)
	}
	for selector := range rows {
		t.Errorf("docs/reference/harnesses.md lists %q, which is not registered in %s", selector, registryFile)
	}
}

// exampleProblem checks that a stock runtime example is a valid AgentRuntime
// selecting the harness whose containers read its MCP file, bridge or a launch
// file from the run contract.
func exampleProblem(h Harness, example string) string {
	data, err := os.ReadFile("../../" + example)
	if err != nil {
		return fmt.Sprintf("has no stock runtime example: add %s, starting from "+
			"examples/harnesses/agentruntime-opencode.yaml", example)
	}
	var rt api.AgentRuntime
	if err = yaml.UnmarshalStrict(data, &rt); err != nil || rt.Kind != "AgentRuntime" {
		return fmt.Sprintf("%s is not a valid AgentRuntime (%v); start from "+
			"examples/harnesses/agentruntime-opencode.yaml", example, err)
	}
	if rt.Spec.ClientConfig.Harness != h.Selector {
		return fmt.Sprintf("%s must set spec.clientConfig.harness: %s", example, h.Selector)
	}
	spec, err := json.Marshal(rt.Spec)
	if err != nil {
		return err.Error()
	}
	read := []string{h.MCP.File}
	if h.Loader.Bridge != "" {
		read = append(read, h.Loader.Bridge)
	}
	for name := range h.LaunchFiles {
		read = append(read, name)
	}
	for _, name := range read {
		if strings.Contains(string(spec), "/etc/sproozi/contract/"+name) {
			return ""
		}
	}
	return fmt.Sprintf("%s's containers never read /etc/sproozi/contract/ for any of %v; its command must load "+
		"the MCP file or the launch file that does", example, read)
}

// The image build context must ship every launch file.
func TestDockerignoreShipsLaunchFiles(t *testing.T) {
	data, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^!internal/harness/launchfiles/?$`).Match(data) {
		t.Fatal(".dockerignore must re-include internal/harness/launchfiles/, or the manager image build " +
			"fails for every embedded launch file")
	}
}
