//go:build !windows

package docker

import (
	"path/filepath"
	"testing"
)

func fixtureDockerEndpoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "docker.sock")
}

func assertDockerPlatformEnvironment(t *testing.T, _ map[string]string, _ string) {
	t.Helper()
}
