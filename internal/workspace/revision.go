package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/privatefs"
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
// source. scratch must be private storage. Restore rejects the archive before
// checkout unless the expanded size of every Git object plus the selected files
// fits maxBytes (checkObjects), so scratch use stays within a small multiple of
// maxBytes: the bundle, its stored objects, the restored files and the clean
// checkout. Restore cannot bound Git's time or memory; callers handling an
// untrusted archive must pass a context with a deadline. The caller must
// independently authorize the new execution.
func AtRevision(ctx context.Context, data []byte, revision, scratch string, maxBytes int64) ([]byte, error) {
	meta, err := Inspect(data, maxBytes)
	if err != nil {
		return nil, err
	}
	if revision == "" || revision != meta.Revision {
		return nil, errors.New("workspace: request the exact full recorded Git revision")
	}
	dir, err := privatefs.TempDir(scratch, ".sproozi-revision-")
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

// Empty captures a fresh repository holding one empty commit: the seed for a
// task whose native worker fetches its own source. scratch must be private
// storage, as for AtRevision.
func Empty(ctx context.Context, scratch string, maxBytes int64) ([]byte, error) {
	dir, err := privatefs.TempDir(scratch, ".sproozi-empty-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	repo := filepath.Join(dir, "workspace")
	g := gitRunner{home: dir, limit: maxBytes}
	if _, err = g.run(ctx, dir, "init", "--quiet", "--template=", repo); err != nil {
		return nil, err
	}
	if _, err = g.run(ctx, repo, "commit", "--quiet", "--allow-empty", "-m", "Initialize controlled workspace"); err != nil {
		return nil, err
	}
	return Capture(ctx, repo, nil, maxBytes)
}
