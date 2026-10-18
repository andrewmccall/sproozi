//go:build !windows

package docker

import (
	"errors"
	"os"
	"path/filepath"
)

func localDockerEndpoint(socket string) (string, error) {
	if !filepath.IsAbs(socket) {
		return "", errors.New("docker requires an absolute local Unix socket")
	}
	return "unix://" + socket, nil
}

func dockerProcessEnvironment(config string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + config, "LC_ALL=C"}
}
