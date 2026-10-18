package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Restore validates the complete archive and reconstructs Git state in a private
// sibling directory before publishing target. Target must not exist; its parent
// must be a trusted directory without concurrent writers. No config or hooks from
// the original repository are installed. A caller handling untrusted Git packs
// must pass a context with a deadline; see the package byte-limit contract.
func Restore(ctx context.Context, data []byte, target string, maxBytes int64) error {
	m, bundle, files, err := decode(data, maxBytes)
	if err != nil {
		return err
	}
	parent, target, err := restoreDestination(target)
	if err != nil {
		return err
	}
	staging, err := privatefs.TempDir(parent, ".sproozi-workspace-restore-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// A Kubernetes fsGroup parent can add setgid despite MkdirTemp's 0700.
	// Clear inherited special modes on our own staging before Git creates children.
	if err = os.Chmod(staging, 0700); err != nil {
		return err
	}
	repo := filepath.Join(staging, "repo")
	if err = os.Mkdir(repo, 0700); err != nil {
		return err
	}
	if err = os.Chmod(repo, 0700); err != nil {
		return err
	}
	g := gitRunner{home: staging, limit: maxBytes}
	if _, err = g.run(ctx, repo, "init", "--quiet", "--template=", "--object-format="+m.Format); err != nil {
		return err
	}
	if err = restoreGit(ctx, g, repo, m, bundle, maxBytes); err != nil {
		return err
	}
	// read-tree installs the captured index; overwrite its working files from raw,
	// bounded archive bytes and remove indexed files deleted in the working tree.
	selected := map[string]bool{}
	for _, e := range m.Files {
		selected[e.Path] = true
	}
	names, err := g.run(ctx, repo, "ls-files", "-z")
	if err != nil {
		return err
	}
	for _, p := range splitZero(names) {
		if !selected[p] {
			if err = os.Remove(filepath.Join(repo, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	// Remove empty indexed directories before files that replace those directories.
	if err = removeEmptyDirs(repo); err != nil {
		return err
	}
	for i, e := range m.Files {
		file := filepath.Join(repo, filepath.FromSlash(e.Path))
		if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return err
		}
		if err = os.WriteFile(file, files[i], os.FileMode(e.Mode)); err != nil {
			return err
		}
		if err = os.Chmod(file, os.FileMode(e.Mode)); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(filepath.Join(repo, ".git", "info"), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(m.Exclude), 0600); err != nil {
		return err
	}
	if _, err = g.run(ctx, repo, "update-ref", "-d", indexRef); err != nil {
		return err
	}
	// Persist safe local defaults, never the source configuration.
	for _, setting := range [][2]string{{"core.hooksPath", os.DevNull}, {"core.fsmonitor", gitFalse}, {"core.autocrlf", gitFalse}, {"core.fileMode", "true"}, {"core.ignoreCase", gitFalse}} {
		if _, err = g.run(ctx, repo, "config", "--local", setting[0], setting[1]); err != nil {
			return err
		}
	}
	if err = inspect(repo); err != nil {
		return err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return errors.New("workspace: destination appeared during restore")
	}
	return os.Rename(repo, target)
}

func restoreDestination(target string) (string, string, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return "", "", errors.New("workspace: destination must not exist")
	}
	parent := filepath.Dir(target)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", "", err
	}
	if _, reparse, inspectErr := inspectFilesystemPath(parent, parentInfo); inspectErr != nil {
		return "", "", inspectErr
	} else if reparse {
		return "", "", errors.New("workspace: restore parent must not be a reparse point")
	}
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", err
	}
	return canonical, filepath.Join(canonical, filepath.Base(target)), nil
}

func removeEmptyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == filepath.Join(root, ".git") {
			return filepath.SkipDir
		}
		if p != root && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, v := range slices.Backward(dirs) {
		entries, err := os.ReadDir(v)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err = os.Remove(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func decode(data []byte, max int64) (manifest, []byte, [][]byte, error) {
	var m manifest
	if max < 1 || int64(len(data)) > max {
		return m, nil, nil, errors.New("workspace: archive exceeds byte limit")
	}
	// Reject duplicate JSON keys separately: encoding/json otherwise accepts them.
	reader := bytes.NewReader(data)
	tr := tar.NewReader(reader)
	ar := archiveReader{tar: tr, max: max}
	read := ar.read
	meta, err := read("manifest.json")
	if err != nil {
		return m, nil, nil, err
	}
	m, err = decodeManifest(meta, max)
	if err != nil {
		return m, nil, nil, err
	}
	bundle, err := read("repository.bundle")
	if err != nil {
		return m, nil, nil, err
	}
	if err = verifyEntry(m.Bundle, bundle, max); err != nil {
		return m, nil, nil, err
	}
	files := make([][]byte, 0, len(m.Files))
	seen := map[string]bool{}
	aliases := map[string]string{}
	total := int64(0)
	for i, e := range m.Files {
		if !validPath(e.Path) || seen[e.Path] || (e.Mode < 0 || e.Mode > 0777) {
			return m, nil, nil, errors.New("workspace: invalid file descriptor")
		}
		if err = recordPortablePath(aliases, e.Path); err != nil {
			return m, nil, nil, err
		}
		seen[e.Path] = true
		b, err := read(fmt.Sprintf("files/%08d", i))
		if err != nil {
			return m, nil, nil, err
		}
		if err = verifyEntry(e, b, max-total); err != nil {
			return m, nil, nil, err
		}
		total += e.Size
		files = append(files, b)
	}
	for p := range seen {
		for parent := filepath.ToSlash(filepath.Dir(p)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if seen[parent] {
				return m, nil, nil, errors.New("workspace: file/directory collision")
			}
		}
	}
	if _, err = tr.Next(); err != io.EOF {
		return m, nil, nil, errors.New("workspace: unexpected archive contents")
	}
	// tar.Reader accepts bytes after its end markers; reject nonzero trailing data.
	tail, err := io.ReadAll(reader)
	if err != nil {
		return m, nil, nil, err
	}
	for _, b := range tail {
		if b != 0 {
			return m, nil, nil, errors.New("workspace: trailing archive data")
		}
	}
	return m, bundle, files, nil
}
func verifyEntry(e entry, b []byte, max int64) error {
	if e.Size < 0 || e.Size > max || int64(len(b)) != e.Size || len(e.Digest) != 64 || digest(b) != e.Digest {
		return errors.New("workspace: entry size or digest mismatch")
	}
	return nil
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("workspace: excessive JSON nesting")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			tok, err = d.Token()
			if err != nil {
				return err
			}
			key, ok := tok.(string)
			if !ok || keys[key] {
				return errors.New("workspace: duplicate JSON key")
			}
			keys[key] = true
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("workspace: invalid JSON")
	}
	_, err = d.Token()
	return err
}

func restoreGit(ctx context.Context, g gitRunner, repo string, m manifest, bundle []byte, maxBytes int64) error {
	var err error
	source := filepath.Join(filepath.Dir(repo), "repository.bundle")
	if err = os.WriteFile(source, bundle, 0600); err != nil {
		return err
	}
	if _, err = g.run(ctx, repo, "bundle", "verify", source); err != nil {
		return err
	}
	// This private inert repository has no user worktree yet. The bundle may
	// include Git's default unborn HEAD branch, which normal fetch refuses to update.
	if _, err = g.run(ctx, repo, "fetch", "--quiet", "--no-tags", "--update-head-ok", source, "+refs/*:refs/*"); err != nil {
		return err
	}
	total := int64(0)
	for _, f := range m.Files {
		total += f.Size
	}
	if err = checkObjects(ctx, g, repo, maxBytes-total); err != nil {
		return err
	}
	if err = checkTree(ctx, g, repo, m.Head); err != nil {
		return err
	}
	if err = checkTree(ctx, g, repo, m.Index); err != nil {
		return err
	}
	actual, err := g.run(ctx, repo, "rev-parse", "--verify", indexRef+"^{tree}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(actual)) != m.Index {
		return errors.New("workspace: index checkpoint identity mismatch")
	}
	if m.Branch != "" {
		if !strings.HasPrefix(m.Branch, "refs/heads/") {
			return errors.New("workspace: invalid HEAD branch")
		}
		if _, err = g.run(ctx, repo, "check-ref-format", m.Branch); err != nil {
			return err
		}
		actual, err = g.run(ctx, repo, "rev-parse", "--verify", m.Branch)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(actual)) != m.Head {
			return errors.New("workspace: HEAD branch identity mismatch")
		}
	}
	if _, err = g.run(ctx, repo, "checkout", "--quiet", "--detach", m.Head); err != nil {
		return err
	}
	if m.Branch != "" {
		if _, err = g.run(ctx, repo, "symbolic-ref", "HEAD", m.Branch); err != nil {
			return err
		}
	}
	if _, err = g.run(ctx, repo, "read-tree", "--reset", "-u", m.Index); err != nil {
		return err
	}
	return nil
}

type archiveReader struct {
	tar *tar.Reader
	max int64
}

func (a archiveReader) read(name string) ([]byte, error) {

	h, err := a.tar.Next()
	if err != nil {
		return nil, err
	}
	if h.Name != name || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > a.max || h.Mode != 0600 || h.Linkname != "" || len(h.PAXRecords) > 0 {
		return nil, errors.New("workspace: invalid archive entry")
	}
	b, err := io.ReadAll(io.LimitReader(a.tar, a.max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != h.Size {
		return nil, errors.New("workspace: incomplete archive entry")
	}
	return b, nil

}

func decodeManifest(meta []byte, max int64) (manifest, error) {
	var m manifest
	var err error
	jd := json.NewDecoder(bytes.NewReader(meta))
	if err = uniqueJSON(jd, 0); err != nil {
		return m, err
	}
	if _, err = jd.Token(); err != io.EOF {
		return m, errors.New("workspace: trailing JSON")
	}
	jd = json.NewDecoder(bytes.NewReader(meta))
	jd.DisallowUnknownFields()
	if err = jd.Decode(&m); err != nil {
		return m, err
	}
	if m.Version != version || (m.Format != sha1Format && m.Format != sha256Format) || !oid(m.Head, m.Format) || !oid(m.Index, m.Format) || len(m.Files) > maxEntries || int64(len(m.Exclude)) > max {
		return m, errors.New("workspace: invalid manifest")
	}
	if m.Bundle.Path != "repository.bundle" || m.Bundle.Mode != 0600 {
		return m, errors.New("workspace: invalid bundle descriptor")
	}
	return m, nil
}
