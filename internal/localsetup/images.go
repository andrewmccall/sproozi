package localsetup

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Image is the approved native image. One image supplies the separate
// worker, checkpoint-helper and network-anchor roles; the placement fixes each
// role's command and privileges.
type Image struct {
	Reference string `json:"reference"`
	ID        string `json:"id"`
	Source    string `json:"source"`
	Contract  string `json:"contract"`
}

// Image sources, in the order setup prefers them.
const (
	SourceRelease  = "release"
	SourceOverride = "override"
	SourceBuild    = "build"
)

// helperContract is the sproozi-network probe interface setup requires.
const helperContract = "1"

//go:embed release-images.json
var releaseManifest []byte

// ReleaseImage is the digest-pinned native image published with this CLI
// release, or empty for a development build.
func ReleaseImage() string {
	var m struct {
		Native string `json:"native"`
	}
	if json.Unmarshal(releaseManifest, &m) != nil {
		return ""
	}
	return m.Native
}

var (
	pinnedReference = regexp.MustCompile(`^[a-z0-9]+([._/:-][a-z0-9]+)*@sha256:[0-9a-f]{64}$`)
	contentID       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ImageRequest selects how the native image is obtained.
type ImageRequest struct {
	// Reference overrides the release image with a digest-pinned reference or
	// a local content ID.
	Reference string
	// BuildSource builds from a Sproozi source tree with the pinned recipes.
	BuildSource string
	Progress    io.Writer
}

// ResolveImage pulls, reuses or builds the native image and verifies its
// helper contract. It never silently substitutes another source.
func ResolveImage(ctx context.Context, d Docker, installation string, r ImageRequest) (Image, error) {
	var image Image
	var err error
	switch {
	case r.BuildSource != "":
		image, err = buildImage(ctx, d, installation, r.BuildSource, r.Progress)
	case r.Reference != "":
		image, err = pullImage(ctx, d, r.Reference, SourceOverride, r.Progress)
	case ReleaseImage() != "":
		image, err = pullImage(ctx, d, ReleaseImage(), SourceRelease, r.Progress)
	default:
		return Image{}, fail("image", errors.New("this development build has no release image"), "pass --image REPOSITORY@sha256:DIGEST or --build SOURCE_DIRECTORY")
	}
	if err != nil {
		return Image{}, err
	}
	return image, verifyContract(ctx, d, &image)
}

// VerifyImage checks a recorded image is still present with the same content.
func VerifyImage(ctx context.Context, d Docker, image Image) error {
	id, err := d.Output(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image.ID)
	if err != nil || strings.TrimSpace(id) != image.ID {
		return fail("image", fmt.Errorf("recorded image %s is no longer present", image.ID), "run sproozi images prepare")
	}
	return nil
}

func pullImage(ctx context.Context, d Docker, reference, source string, progress io.Writer) (Image, error) {
	if contentID.MatchString(reference) {
		if err := VerifyImage(ctx, d, Image{ID: reference}); err != nil {
			return Image{}, err
		}
		return Image{Reference: reference, ID: reference, Source: source}, nil
	}
	if !pinnedReference.MatchString(reference) {
		return Image{}, fail("image", fmt.Errorf("%q is not digest-pinned", reference), "use REPOSITORY@sha256:DIGEST")
	}
	if _, err := d.Output(ctx, nil, "image", "inspect", "--format", "{{.Id}}", reference); err != nil {
		if progress == nil {
			progress = io.Discard
		}
		_, _ = fmt.Fprintf(progress, "Pulling %s\n", reference)
		var stderr strings.Builder
		if err = d.Stream(ctx, nil, progress, &stderr, "pull", "--quiet", reference); err != nil {
			return Image{}, fail("registry", fmt.Errorf("pull failed: %s", strings.TrimSpace(stderr.String())), "check registry access, or build locally with --build SOURCE_DIRECTORY")
		}
	}
	id, err := d.Output(ctx, nil, "image", "inspect", "--format", "{{.Id}}", reference)
	id = strings.TrimSpace(id)
	if err != nil || !contentID.MatchString(id) {
		return Image{}, fail("image", fmt.Errorf("pulled image has no content ID: %v", err), "")
	}
	return Image{Reference: reference, ID: id, Source: source}, nil
}

// buildImage runs the repository's pinned three-stage recipe through Docker.
// The build context is the source tree; workspaces, agent state, credentials
// and keys live under the installation root, never in it.
func buildImage(ctx context.Context, d Docker, installation, source string, progress io.Writer) (Image, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return Image{}, err
	}
	for _, required := range []string{"go.mod", "hack/demo/codex.Dockerfile", "internal/runtime/docker/image/clients.Dockerfile", "internal/runtime/docker/image/Dockerfile", "hack/clients/install.sh"} {
		if _, err = os.Stat(filepath.Join(source, required)); err != nil {
			return Image{}, fail("image", fmt.Errorf("%s is not a Sproozi source tree: missing %s", source, required), "pass the extracted release source bundle or a checkout")
		}
	}
	if progress == nil {
		progress = io.Discard
	}
	prefix := "sproozi-local-" + installation
	steps := []struct{ file, tag, arg string }{
		{"hack/demo/codex.Dockerfile", prefix + "/clients:codex", ""},
		{"internal/runtime/docker/image/clients.Dockerfile", prefix + "/clients:all", "CODEX_CLIENTS_IMAGE=" + prefix + "/clients:codex"},
		{"internal/runtime/docker/image/Dockerfile", prefix + "/native:local", "CLIENTS_IMAGE=" + prefix + "/clients:all"},
	}
	for _, step := range steps {
		_, _ = fmt.Fprintf(progress, "Building %s\n", step.tag)
		args := []string{"build", "--progress", "plain", "-f", filepath.Join(source, step.file), "-t", step.tag, "--label", "sproozi.com/installation=" + installation}
		if step.arg != "" {
			args = append(args, "--build-arg", step.arg)
		}
		var stderr tail
		if err = d.Stream(ctx, nil, progress, io.MultiWriter(progress, &stderr), append(args, source)...); err != nil {
			return Image{}, fail("image", fmt.Errorf("build of %s failed: %s", step.tag, stderr.String()), "inspect the build output above")
		}
	}
	id, err := d.Output(ctx, nil, "image", "inspect", "--format", "{{.Id}}", prefix+"/native:local")
	id = strings.TrimSpace(id)
	if err != nil || !contentID.MatchString(id) {
		return Image{}, fail("image", errors.New("built image has no content ID"), "")
	}
	return Image{Reference: prefix + "/native:local", ID: id, Source: SourceBuild}, nil
}

func verifyContract(ctx context.Context, d Docker, image *Image) error {
	out, err := d.Output(ctx, nil, "run", "--rm", "--network", "none", "--user", "1000:1000", "--cap-drop", "ALL", "--read-only",
		"--security-opt", "no-new-privileges", "--entrypoint", "/usr/local/bin/sproozi-network", image.ID, "probe", "contract")
	contract := strings.TrimSpace(out)
	if err != nil || contract != helperContract {
		return fail("image", fmt.Errorf("image %s does not provide helper contract %s (got %q)", image.ID, helperContract, contract), "use a native image built for this Sproozi release")
	}
	image.Contract = contract
	return nil
}

// tail keeps the last few KiB of a stream for an error message.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.b)) }
