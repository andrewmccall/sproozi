package manifests_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

// serviceAddress matches in-cluster names such as sproozi-work.sproozi-sessions.svc.
var serviceAddress = regexp.MustCompile(`\b([a-z0-9]([-a-z0-9]*[a-z0-9])?)\.([a-z0-9]([-a-z0-9]*[a-z0-9])?)\.svc\b`)

// Moving a component between namespaces once left an example pointing at a
// Service that no longer existed, and the acceptance script rewrote the URL
// before deploying. Every in-cluster address that the install, the examples
// or the docs name must be a Service that the install or an example declares.
func TestServiceAddressesNameDeclaredServices(t *testing.T) {
	root := repoRoot(t)
	declared := map[string]bool{}
	for _, svc := range objectsOfKind(renderInstall(t), "Service") {
		declared[metadataName(svc)+"."+metadataNamespace(svc)] = true
	}
	var sources []string
	for _, dir := range []string{"examples", "config", "docs", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			switch filepath.Ext(path) {
			case ".yaml", ".yml", ".md":
				sources = append(sources, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sources = append(sources, filepath.Join(root, "README.md"))
	for _, path := range sources {
		if !strings.HasPrefix(path, filepath.Join(root, "examples")) {
			continue
		}
		for _, svc := range objectsOfKind(decodeFile(t, path), "Service") {
			declared[metadataName(svc)+"."+metadataNamespace(svc)] = true
		}
	}
	if !declared["sproozi-gateway.sproozi-system"] {
		t.Fatalf("install does not declare the gateway Service: %v", declared)
	}
	for _, path := range sources {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range serviceAddress.FindAllSubmatch(text, -1) {
			name, namespace := string(m[1]), string(m[3])
			if name == "kubernetes" && namespace == "default" {
				continue
			}
			if !declared[name+"."+namespace] {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s names %s.%s.svc, but no Service %s/%s is declared by the install or an example",
					rel, name, namespace, namespace, name)
			}
		}
	}
}

// decodeFile returns every object in a YAML file, skipping files that are not
// Kubernetes manifests.
func decodeFile(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var docs []map[string]any
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		if len(doc) != 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}
