package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const imageBuildRecipe = "hack/release/docker-bake.hcl"
const bakeSetFlag = "--set"

var imagePrefixPattern = regexp.MustCompile(`^[a-z0-9]+([._:/-][a-z0-9]+)*$`)
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (q *options) validateImageBuild() error {
	building := q.action() == cmdImages && q.verb == verbBuild
	if !building {
		if q.imagePrefix != "" || q.imageTag != "" || q.platform != "" || q.push {
			return usagef("--image-prefix, --image-tag, --platform and --push apply only to images build")
		}
		if q.image != "" && q.build != "" {
			return usagef("choose --image or --build")
		}
		return nil
	}
	if q.build == "" {
		return usagef("images build needs --build SOURCE_DIRECTORY; " +
			"use the matching release source bundle or a development checkout")
	}
	if q.image != "" || q.root != "" {
		return usagef("images build does not use --image or --root; it builds without changing an installation")
	}
	if q.push && q.imagePrefix == "" {
		return usagef("--push needs --image-prefix REGISTRY/OWNER")
	}
	if q.imagePrefix == "" {
		q.imagePrefix = "sproozi-local"
	}
	if q.imageTag == "" {
		q.imageTag = build.version
	}
	if !imagePrefixPattern.MatchString(q.imagePrefix) || !imageTagPattern.MatchString(q.imageTag) {
		return usagef("invalid image repository prefix or tag")
	}
	if q.platform != "" {
		platforms := strings.Split(q.platform, ",")
		if len(platforms) > 1 && !q.push {
			return usagef("multiple platforms require --push; a local build loads one platform")
		}
		for _, platform := range platforms {
			if platform != "linux/amd64" && platform != "linux/arm64" {
				return usagef("--platform accepts linux/amd64 and linux/arm64")
			}
		}
	}
	return nil
}

// imageBuildArgs uses the release recipe, including its pinned client stages.
// The source directory is explicit: no pull failure ever triggers a build.
func (q *options) imageBuildArgs() ([]string, error) {
	for _, name := range []string{"go.mod", imageBuildRecipe, "Dockerfile",
		"hack/demo/codex.Dockerfile", "internal/runtime/docker/image/clients.Dockerfile",
		"internal/runtime/docker/image/Dockerfile", "hack/clients/install.sh"} {
		info, err := os.Stat(filepath.Join(q.build, name))
		if err != nil || info.IsDir() {
			return nil, fmt.Errorf("not a Sproozi source tree: missing %s", name)
		}
	}
	// A release bundle must match this CLI. A checkout is deliberately allowed
	// for custom builds; its recipes and dependency pins are used as supplied.
	metadata, err := os.ReadFile(filepath.Join(q.build, "release.json"))
	if err == nil {
		var source struct{ Version string }
		if err = json.Unmarshal(metadata, &source); err != nil || source.Version == "" {
			return nil, fmt.Errorf("invalid source release.json")
		}
		if build.version != sourceVersion && source.Version != build.version {
			return nil, fmt.Errorf("source release %s does not match CLI %s", source.Version, build.version)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	args := []string{"buildx", "bake", "-f", imageBuildRecipe, "--progress", "plain",
		bakeSetFlag, "native.tags=" + q.imagePrefix + "/sproozi-native:" + q.imageTag,
		bakeSetFlag, "control.tags=" + q.imagePrefix + "/sproozi:" + q.imageTag,
		bakeSetFlag, "*.labels.org.opencontainers.image.version=" + q.imageTag}
	if q.platform != "" {
		args = append(args, bakeSetFlag, "*.platform="+q.platform)
	}
	if q.push {
		args = append(args, "--push")
	} else {
		args = append(args, "--load")
	}
	return append(args, "native", "control"), nil
}

func (q *options) buildImages(ctx context.Context) error {
	args, err := q.imageBuildArgs()
	if err != nil {
		return err
	}
	// Building/publishing is an administrator operation, using their selected
	// Docker context, Buildx builder and registry login, outside a sandbox.
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = q.build
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("image build failed (Docker with Buildx is required): %w", err)
	}
	result := struct {
		Native  string `json:"native"`
		Control string `json:"control"`
		Pushed  bool   `json:"pushed"`
	}{q.imagePrefix + "/sproozi-native:" + q.imageTag, q.imagePrefix + "/sproozi:" + q.imageTag, q.push}
	if q.json {
		return writeJSON(q.out, result)
	}
	_, err = fmt.Fprintf(q.out, "Built %s\nBuilt %s\n", result.Native, result.Control)
	return err
}
