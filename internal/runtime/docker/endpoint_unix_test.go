//go:build !windows

package docker

import (
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"
)

func fixtureDockerEndpoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(testutil.PrivateDir(t), "docker.sock")
}

func assertDockerPlatformEnvironment(t *testing.T, _ map[string]string, _ string) {
	t.Helper()
}
