//go:build windows

package docker

import (
	"errors"
	"os"
	"strings"
)

const localPipePrefix = "npipe:////./pipe/"

func localDockerEndpoint(endpoint string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(endpoint), localPipePrefix) {
		return "", errors.New("docker requires an explicit local Windows named-pipe endpoint")
	}
	name := endpoint[len(localPipePrefix):]
	if name == "" || strings.ContainsAny(name, `/\\:`) || name == "." || name == ".." {
		return "", errors.New("docker requires an ordinary local Windows pipe name")
	}
	return localPipePrefix + name, nil
}

func dockerProcessEnvironment(config string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + config, "USERPROFILE=" + config, "LC_ALL=C"}
	for _, name := range []string{"SystemRoot", "ComSpec", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
