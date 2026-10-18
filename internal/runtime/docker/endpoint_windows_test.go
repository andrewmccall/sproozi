//go:build windows

package docker

import (
	"os"
	"strings"
	"testing"
)

func fixtureDockerEndpoint(t *testing.T) string {
	t.Helper()
	return "npipe:////./pipe/sproozi-docker-test"
}

func assertDockerPlatformEnvironment(t *testing.T, env map[string]string, config string) {
	t.Helper()
	if env["USERPROFILE"] != config || env["SYSTEMROOT"] == "" || env["TEMP"] == "" || env["TMP"] == "" {
		t.Fatalf("missing required Windows process environment: %#v", env)
	}
	for name := range env {
		if strings.HasPrefix(strings.ToUpper(name), "DOCKER_") {
			t.Fatalf("inherited Docker environment %s", name)
		}
	}
	if os.Getenv("SystemRoot") != "" && env["SYSTEMROOT"] != os.Getenv("SystemRoot") {
		t.Fatalf("SystemRoot=%q want %q", env["SYSTEMROOT"], os.Getenv("SystemRoot"))
	}
}

func TestWindowsDockerEndpointRejectsRemoteAndNonPipeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"tcp://127.0.0.1:2375", "npipe:////remote/pipe/docker", "//./pipe/docker", "npipe:////./pipe/../docker"} {
		if _, err := localDockerEndpoint(endpoint); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
}
