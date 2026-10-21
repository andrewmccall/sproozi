package manifests_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/mcppolicy"
	policy "github.com/andrewmccall/sproozi/internal/policy/kubernetes"
)

// mcpExample is one directory holding a registry and the resources that use it.
type mcpExample struct {
	registry *mcp.Registry
	policy   api.AgentPolicy
	template api.AgentTemplate
	run      api.AgentRun
	runtime  api.AgentRuntime
}

func readStrict(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = yaml.UnmarshalStrict(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func loadMCPExample(t *testing.T, dir string) mcpExample {
	t.Helper()
	var e mcpExample
	data, err := os.ReadFile(filepath.Join(dir, "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e.registry, err = mcp.LoadRegistry(data, t.TempDir()); err != nil {
		t.Fatalf("%s/servers.json: %v", dir, err)
	}
	readStrict(t, filepath.Join(dir, "agentpolicy.yaml"), &e.policy)
	readStrict(t, filepath.Join(dir, "agenttemplate.yaml"), &e.template)
	readStrict(t, filepath.Join(dir, "agentrun.yaml"), &e.run)
	readStrict(t, filepath.Join(dir, "agentruntime.yaml"), &e.runtime)
	return e
}

// Every MCP example is consistent offline: the registry loads, every granted
// server is registered and scoped, the scopes compile, the run asks only for
// what the policy allows, admission accepts the run, and the runtime's
// harness receives a connection for each granted server.
func TestMCPExamplesAreConsistent(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(repoRoot(t), "examples", "*", "servers.json"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no MCP examples found: %v", err)
	}
	// Examples are applied side by side, so a copied example must rename its
	// resources rather than replace another example's.
	owners := map[string]string{}
	for _, registryFile := range dirs {
		dir := filepath.Dir(registryFile)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			e := loadMCPExample(t, dir)
			for _, object := range []struct{ kind, namespace, name string }{
				{"AgentPolicy", e.policy.Namespace, e.policy.Name}, {"AgentTemplate", e.template.Namespace, e.template.Name},
				{"AgentRuntime", e.runtime.Namespace, e.runtime.Name},
				{"AgentRun", e.run.Namespace, e.run.Name + e.run.GenerateName},
			} {
				key := object.kind + " " + object.namespace + "/" + object.name
				if other, taken := owners[key]; taken {
					t.Errorf("%s is defined by both %s and %s; give each example its own names", key,
						filepath.Base(other), filepath.Base(dir))
				}
				owners[key] = dir
			}
			registered := e.registry.Names()
			var granted []string
			for _, capability := range e.policy.Spec.AllowedCapabilities {
				name, named := capability.MCPServerName()
				if !named {
					continue
				}
				granted = append(granted, string(capability))
				if !slices.Contains(registered, name) {
					t.Errorf("policy grants %s, but servers.json registers only %v", capability, registered)
				}
				if _, err := mcppolicy.CompileScope(e.policy.Spec.MCPServers[name]); err != nil {
					t.Errorf("policy mcpServers.%s: %v", name, err)
				}
				if e.policy.Spec.Budgets[capability].MaxUnits <= 0 {
					t.Errorf("policy has no finite budget for %s", capability)
				}
			}
			for name := range e.policy.Spec.MCPServers {
				if !slices.Contains(e.policy.Spec.AllowedCapabilities, api.CapabilityKind("mcp."+name)) {
					t.Errorf("policy scopes mcpServers.%s but does not allow mcp.%s", name, name)
				}
			}
			for _, capability := range e.run.Spec.Capabilities {
				if !slices.Contains(e.policy.Spec.AllowedCapabilities, capability) {
					t.Errorf("run asks for %s, which the policy does not allow", capability)
				}
			}
			if e.run.Spec.TemplateRef.Name != e.template.Name || e.template.Spec.PolicyRef.Name != e.policy.Name ||
				e.template.Spec.RuntimeRef.Name != e.runtime.Name {
				t.Errorf("run, template, policy and runtime names do not connect")
			}
			if denial := policy.Evaluate(e.policy.Spec, e.template.Spec, e.run.Spec, e.runtime.Spec); denial != nil {
				t.Errorf("admission denied the example run: %s", denial.Message)
			}
			config, err := harness.MCPConfig(e.runtime.Spec.ClientConfig.Harness, granted, e.runtime.Spec.GatewayEndpoint)
			if err != nil {
				t.Fatalf("MCP client configuration: %v", err)
			}
			rendered := strings.Join(mapValues(config), "\n")
			for _, capability := range granted {
				if !strings.Contains(rendered, "/mcp/"+strings.TrimPrefix(capability, "mcp.")) {
					t.Errorf("%s client configuration has no connection for %s", e.runtime.Spec.ClientConfig.Harness, capability)
				}
			}
		})
	}
}

func mapValues(m map[string]string) []string {
	values := make([]string, 0, len(m))
	for _, value := range m {
		values = append(values, value)
	}
	return values
}
