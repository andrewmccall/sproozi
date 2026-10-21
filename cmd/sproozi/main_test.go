package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the tests their own home and configuration directories, so
// the default configuration path, recorded installation root and kubeconfig
// never come from the account running them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sproozi-cli-home-")
	if err != nil {
		panic(err)
	}
	for name, value := range map[string]string{"HOME": dir, "USERPROFILE": dir,
		"XDG_CONFIG_HOME": filepath.Join(dir, ".config"), "APPDATA": filepath.Join(dir, "AppData"),
		"KUBECONFIG": filepath.Join(dir, ".kube", "config")} {
		if err = os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
