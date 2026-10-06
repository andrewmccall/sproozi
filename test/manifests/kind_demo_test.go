package manifests_test

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestKindDemoSeparatesAgentsFromLocalAPIServerNode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "hack", "kind-cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Nodes []struct {
			Role string `json:"role"`
		} `json:"nodes"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Nodes) != 2 || config.Nodes[0].Role != "control-plane" || config.Nodes[1].Role != "worker" {
		t.Fatal("Kind demo must use a worker: standard NetworkPolicy cannot deny traffic to a Pod's own API-server node")
	}
}
