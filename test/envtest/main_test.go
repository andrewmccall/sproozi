package envtest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if dir, ok := findLocalKubebuilderAssets(); ok {
			_ = os.Setenv("KUBEBUILDER_ASSETS", dir)
		}
	}
	os.Exit(m.Run())
}

func findLocalKubebuilderAssets() (string, bool) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	entries, err := filepath.Glob(filepath.Join(root, "bin", "k8s", "*"))
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if hasRequiredEnvtestBinaries(entry) {
			return entry, true
		}
	}
	return "", false
}

func hasRequiredEnvtestBinaries(dir string) bool {
	for _, name := range []string{"etcd", "kube-apiserver", "kubectl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}
