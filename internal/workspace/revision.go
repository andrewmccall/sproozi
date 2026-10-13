package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Metadata separates a committed Git revision from the complete unfinished
// workspace archive. Inspect validates declarations; Restore verifies objects.
type Metadata struct {
	Revision     string `json:"revision"`
	ObjectFormat string `json:"objectFormat"`
	SHA256       string `json:"sha256"`
}

func Inspect(data []byte, maxBytes int64) (Metadata, error) {
	m, _, _, err := decode(data, maxBytes)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Revision: m.Head, ObjectFormat: m.Format, SHA256: digest(data)}, nil
}

// AtRevision creates a clean, detached workspace-only archive at recorded HEAD.
// It reads retained objects, performs no network operation and never changes the
// source. scratch must be private quota-backed storage with process limits, as
// required by Restore. The caller must independently authorize the new execution.
func AtRevision(ctx context.Context, data []byte, revision, scratch string, maxBytes int64) ([]byte, error) {
	meta, err := Inspect(data, maxBytes)
	if err != nil {
		return nil, err
	}
	if revision == "" || revision != meta.Revision {
		return nil, errors.New("workspace: request the exact full recorded Git revision")
	}
	dir, err := os.MkdirTemp(scratch, ".sproozi-revision-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	repo := filepath.Join(dir, "workspace")
	if err = Restore(ctx, data, repo, maxBytes); err != nil {
		return nil, err
	}
	g := gitRunner{home: dir, limit: maxBytes}
	if _, err = g.run(ctx, repo, "checkout", "--quiet", "--force", "--detach", revision); err != nil {
		return nil, err
	}
	if _, err = g.run(ctx, repo, "clean", "--quiet", "-fdx"); err != nil {
		return nil, err
	}
	return Capture(ctx, repo, nil, maxBytes)
}
