package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CaptureLocal is Capture for a workspace on the user's own machine. It also
// applies the effective core.excludesFile that the user's Git would apply, so
// a file ignored only by the repository config, the global config or the
// default global ignore file is not copied unless selectedIgnored names it.
// Call it only in a trusted process that runs as the workspace owner.
func CaptureLocal(ctx context.Context, root string, selectedIgnored []string, maxBytes int64) ([]byte, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	excludes, err := userExcludesFile(ctx, root)
	if err != nil {
		return nil, err
	}
	return capture(ctx, root, selectedIgnored, excludes, maxBytes)
}

// userExcludesFile resolves core.excludesFile as Git does for root: the
// repository value wins over the global value, which wins over the XDG
// default. It reads values with --no-includes and never runs Git inside the
// repository, so no repository or global hooks, filters or includes run. It
// returns "" when no excludes file exists.
func userExcludesFile(ctx context.Context, root string) (string, error) {
	if _, err := sourceEntry(root, ".git/config"); err != nil {
		return "", err
	}
	value, err := configPath(ctx, "--file", filepath.Join(root, ".git", "config"))
	if err != nil {
		return "", err
	}
	if value == "" {
		if value, err = configPath(ctx, "--global"); err != nil {
			return "", err
		}
	}
	if value == "" {
		config := os.Getenv("XDG_CONFIG_HOME")
		if config == "" {
			home := os.Getenv("HOME")
			if home == "" {
				var err error
				home, err = os.UserHomeDir()
				if err != nil {
					return "", nil
				}
			}
			config = filepath.Join(home, ".config")
		}
		value = filepath.Join(config, "git", "ignore")
	}
	if !filepath.IsAbs(value) {
		// Git opens a relative excludes file from the top of the work tree.
		value = filepath.Join(root, value)
	}
	value = filepath.Clean(value)
	info, err := os.Stat(value)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	return value, nil
}

// configPath reads core.excludesFile from one config scope with the caller's
// environment, so ~ and the global file resolve for the real user.
func configPath(ctx context.Context, scope ...string) (string, error) {
	args := append([]string{"config"}, scope...)
	args = append(args, "--no-includes", "--type=path", "--get", "core.excludesFile")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = os.TempDir()
	out := &boundedBuffer{limit: 8192}
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", nil // the key is not set
		}
		return "", fmt.Errorf("workspace: read core.excludesFile: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	value := strings.TrimSpace(out.String())
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("workspace: invalid core.excludesFile")
	}
	return value, nil
}
