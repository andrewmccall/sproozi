// Package workspace checkpoints Git history, the index and selected working files.
// Call Capture only after every workspace writer has stopped. Restore publishes a
// new, absent destination; it never merges into an existing worktree. Git config,
// hooks, credentials and linked worktree metadata are not transferred.
//
// maxBytes bounds the encoded archive, selected file bytes and inspected Git object
// sizes. Git fetches the bundle before object-size inspection, so it stores at
// most the bundle's compressed objects before Restore rejects an oversized
// archive and before any checkout. That inspection does not bound Git's time or
// memory: callers handling untrusted archives must pass a context with a
// deadline, and should run under process limits where the platform offers them.
package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

const version = 1
const sha1Format = "sha1"
const sha256Format = "sha256"
const gitFalse = "false"
const indexRef = "refs/sproozi-checkpoint/index"
const maxEntries = 10000

type entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   int64  `json:"mode"`
	Digest string `json:"digest"`
}
type manifest struct {
	Version int     `json:"version"`
	Head    string  `json:"head"`
	Branch  string  `json:"branch"`
	Index   string  `json:"index"`
	Format  string  `json:"format"`
	Exclude string  `json:"exclude"`
	Bundle  entry   `json:"bundle"`
	Files   []entry `json:"files"`
}
type gitRunner struct {
	home     string
	limit    int64
	excludes string
}

// ErrByteLimit matches a capture or restore that exceeds its byte limit.
var ErrByteLimit = errors.New("workspace: byte limit exceeded")

// ErrUnbornRepository matches a repository with no commit yet.
var ErrUnbornRepository = errors.New("workspace: the repository has no commits")

// ErrNotRepository matches a capture root that is not the top-level directory
// of an ordinary Git repository.
var ErrNotRepository = errors.New("workspace: ordinary Git directory required")

type boundedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.limit-int64(b.Len()) {
		return 0, ErrByteLimit
	}
	return b.Buffer.Write(p)
}
func (g gitRunner) run(ctx context.Context, root string, args ...string) ([]byte, error) {
	base := make([]string, 0, 20+len(args))
	base = append(base, "-c", "core.hooksPath="+os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.attributesFile="+os.DevNull, "-c", "core.autocrlf=false", "-c", "core.fileMode=true", "-c", "core.longpaths=true", "-c", "core.ignoreCase=false", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true")
	if g.excludes != "" {
		base = append(base, "-c", "core.excludesFile="+g.excludes)
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = root
	cmd.Env = append(gitPlatformEnvironment(), "PATH="+os.Getenv("PATH"), "HOME="+g.home, "XDG_CONFIG_HOME="+g.home, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_AUTHOR_NAME=Sproozi", "GIT_AUTHOR_EMAIL=checkpoint@invalid", "GIT_COMMITTER_NAME=Sproozi", "GIT_COMMITTER_EMAIL=checkpoint@invalid")
	out := &boundedBuffer{limit: g.limit}
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("workspace: git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
func validPath(p string) bool {
	if p == "" || p == "." || len(p) > 4096 || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00\r\n:<>\"|?*") {
		return false
	}
	for _, character := range p {
		if character < 32 {
			return false
		}
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || windowsReservedName(part) || strings.EqualFold(strings.TrimRight(part, ". "), ".git") || strings.EqualFold(part, ".gitmodules") {
			return false
		}
	}
	return true
}

func windowsReservedName(name string) bool {
	base := name
	extension := ""
	if i := strings.IndexByte(base, '.'); i >= 0 {
		extension = base[i+1:]
		base = base[:i]
	}
	// A short-name alias such as GIT~1 can address .git or another existing
	// long name on NTFS. Reject that spelling before restoring any file.
	if prefix, suffix, found := strings.Cut(base, "~"); found && prefix != "" && suffix != "" && len(base) <= 8 && len(extension) <= 3 {
		digits := true
		for _, character := range suffix {
			if character < '0' || character > '9' {
				digits = false
			}
		}
		if digits {
			return true
		}
	}
	switch strings.ToUpper(base) {
	case "CON", "CONIN$", "CONOUT$", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	default:
		return false
	}
}

// recordPortablePath rejects names which address the same path on a
// case-insensitive Windows filesystem, including aliases in parent directories.
func recordPortablePath(seen map[string]string, name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		actual := strings.Join(parts[:i+1], "/")
		key := strings.ToLower(actual)
		if previous, ok := seen[key]; ok && previous != actual {
			return fmt.Errorf("workspace: Windows path alias %q conflicts with %q", actual, previous)
		}
		seen[key] = actual
	}
	return nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func descriptor(p string, b []byte, mode int64) entry {
	return entry{p, int64(len(b)), mode, digest(b)}
}
func oid(s, format string) bool {
	n := 40
	if format == sha256Format {
		n = 64
	}
	if len(s) != n {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == n/2 && strings.ToLower(s) == s
}
func readBounded(name string, max int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrByteLimit
	}
	return b, nil
}

// inspect rejects unsupported filesystem layouts rather than following them.
func inspect(root string) error {
	base, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !base.IsDir() || base.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace: root must be a real directory")
	}
	volume, reparse, err := inspectFilesystemPath(root, base)
	if err != nil {
		return err
	}
	if reparse {
		return errors.New("workspace: root must not be a reparse point")
	}
	count := 0
	return filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > maxEntries {
			return errors.New("workspace: too many filesystem entries")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entryVolume, entryReparse, err := inspectFilesystemPath(p, info)
		if err != nil {
			return err
		}
		if entryVolume != volume {
			return errors.New("workspace: external mounts are unsupported")
		}
		if entryReparse || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return errors.New("workspace: special permissions unsupported")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("workspace: unsupported file type at %s", p)
		}
		if p != root && d.IsDir() && d.Name() == ".git" && filepath.Dir(p) != root {
			return errors.New("workspace: nested repositories are unsupported")
		}
		return nil
	})
}

// sourceEntry inspects only selected paths and their parents. Excluded ignored
// sockets, symlinks and nested repositories are never traversed or transferred.
// The caller has stopped all writers, so validated parents cannot change during
// the subsequent read. Missing working paths represent tracked deletions.
func sourceEntry(root, relative string) (os.FileInfo, error) {
	base, err := os.Lstat(root)
	if err != nil || !base.IsDir() || base.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace: root must be a real directory")
	}
	volume, reparse, err := inspectFilesystemPath(root, base)
	if err != nil || reparse {
		return nil, errors.New("workspace: root must be a real directory")
	}
	current := root
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		entryVolume, entryReparse, inspectErr := inspectFilesystemPath(current, info)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if entryVolume != volume || entryReparse || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return nil, fmt.Errorf("workspace: unsupported selected path %q", relative)
		}
		if i < len(parts)-1 && !info.IsDir() || !info.IsDir() && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("workspace: unsupported selected file type %q", relative)
		}
		if info.IsDir() && current != filepath.Join(root, ".git") {
			if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
				return nil, errors.New("workspace: selected nested repository unsupported")
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
		if i == len(parts)-1 {
			return info, nil
		}
	}
	return nil, errors.New("workspace: empty source selection")
}

// Capture returns a bounded archive without altering root or its Git metadata.
// selectedIgnored names individual ignored regular files relative to root.
// Unborn, conflicted, shallow/partial, sparse/split-index, submodule, symlink and
// linked-worktree repositories are unsupported in this first version.
// Capture applies .gitignore files and .git/info/exclude but no core.excludesFile;
// use CaptureLocal for a workspace on the user's own machine.
func Capture(ctx context.Context, root string, selectedIgnored []string, maxBytes int64) ([]byte, error) {
	return capture(ctx, root, selectedIgnored, "", maxBytes)
}

func capture(ctx context.Context, root string, selectedIgnored []string, excludes string, maxBytes int64) ([]byte, error) {
	if err := ValidateIgnoredPaths(selectedIgnored); err != nil {
		return nil, err
	}
	if maxBytes < 1 {
		return nil, errors.New("workspace: positive byte limit required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	gi, err := sourceEntry(root, ".git")
	if err != nil || gi == nil || !gi.IsDir() {
		return nil, ErrNotRepository
	}
	scratch, err := privatefs.TempDir("", "sproozi-workspace-capture-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	g := gitRunner{home: scratch, limit: maxBytes, excludes: excludes}
	// Read only this specific config value; never execute commands with source config.
	format := sha1Format
	if _, err = sourceEntry(root, ".git/config"); err != nil {
		return nil, err
	}
	configured, configErr := g.run(ctx, scratch, "config", "--no-includes", "--file", filepath.Join(root, ".git", "config"), "--get", "extensions.objectFormat")
	if configErr == nil {
		format = strings.TrimSpace(string(configured))
	}
	if format != sha1Format && format != sha256Format {
		return nil, errors.New("workspace: unsupported Git object format")
	}
	repo := filepath.Join(scratch, "repo")
	if err = os.Mkdir(repo, 0700); err != nil {
		return nil, err
	}
	if _, err = g.run(ctx, repo, "init", "--quiet", "--template=", "--object-format="+format); err != nil {
		return nil, err
	}
	if err = copyGit(root, repo, maxBytes); err != nil {
		return nil, err
	}
	args := []string{"--git-dir=" + filepath.Join(repo, ".git"), "--work-tree=" + root}
	run := func(a ...string) ([]byte, error) { return g.run(ctx, scratch, append(args, a...)...) }
	head, err := run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, ErrUnbornRepository
	}
	branch, _ := run("symbolic-ref", "--quiet", "HEAD")
	if err = checkIndexLayout(run); err != nil {
		return nil, err
	}
	tree, err := run("write-tree")
	if err != nil {
		return nil, err
	}
	treeID := strings.TrimSpace(string(tree))
	m := manifest{Version: version, Head: strings.TrimSpace(string(head)), Branch: strings.TrimSpace(string(branch)), Index: treeID, Format: format}
	if err = checkTree(ctx, g, repo, m.Head); err != nil {
		return nil, err
	}
	if err = checkTree(ctx, g, repo, m.Index); err != nil {
		return nil, err
	}
	commit, err := run("commit-tree", treeID, "-p", m.Head, "-m", "Workspace index checkpoint")
	if err != nil {
		return nil, err
	}
	if _, err = run("update-ref", indexRef, strings.TrimSpace(string(commit))); err != nil {
		return nil, err
	}
	bundle, err := run("bundle", "create", "-", "--all", "HEAD")
	if err != nil {
		return nil, err
	}
	m.Bundle = descriptor("repository.bundle", bundle, 0600)
	if exclude, err := readBounded(filepath.Join(root, ".git", "info", "exclude"), maxBytes); err == nil {
		m.Exclude = string(exclude)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	entries, files, total, err := captureWorkingFiles(run, root, selectedIgnored, maxBytes)
	if err != nil {
		return nil, err
	}
	m.Files = entries
	if err = checkObjects(ctx, g, repo, maxBytes-total); err != nil {
		return nil, err
	}
	return encode(m, bundle, files, maxBytes)
}

// mebibytes formats a byte count for an error message.
func mebibytes(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func splitZero(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
func copyGit(root, repo string, max int64) error {
	gitRoot := filepath.Join(root, ".git")
	for _, name := range []string{"shallow", "commondir", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(gitRoot, name)); err == nil {
			return fmt.Errorf("workspace: unsupported Git layout: %s", name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	size := int64(0)
	count := 0
	for _, name := range []string{"objects", "refs", "HEAD", "index", "packed-refs", "info/exclude"} {
		src := filepath.Join(gitRoot, name)
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if strings.HasSuffix(d.Name(), ".promisor") {
				return errors.New("workspace: partial repositories unsupported")
			}
			rel, e := filepath.Rel(gitRoot, p)
			if e != nil {
				return e
			}
			count++
			if count > maxEntries {
				return errors.New("workspace: too many Git metadata entries")
			}
			info, e := sourceEntry(root, filepath.Join(".git", rel))
			if e != nil || info == nil {
				return fmt.Errorf("workspace: selected Git metadata unavailable: %w", e)
			}
			dst := filepath.Join(repo, ".git", rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0700)
			}
			b, e := readBounded(p, max-size)
			if e != nil {
				return e
			}
			size += int64(len(b))
			if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
				return e
			}
			return os.WriteFile(dst, b, 0600)
		}); err != nil {
			return err
		}
	}
	shared, err := filepath.Glob(filepath.Join(gitRoot, "sharedindex.*"))
	if err != nil {
		return err
	}
	if len(shared) > 0 {
		return errors.New("workspace: split index unsupported")
	}
	return nil
}
func checkTree(ctx context.Context, g gitRunner, repo, tree string) error {
	out, err := g.run(ctx, repo, "ls-tree", "-rz", tree)
	if err != nil {
		return err
	}
	seen := map[string]string{}
	for _, item := range splitZero(out) {
		parts := strings.SplitN(item, "\t", 2)
		if len(parts) != 2 {
			return errors.New("workspace: invalid tree")
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !validPath(parts[1]) {
			return errors.New("workspace: unsupported tree path or mode")
		}
		if err = recordPortablePath(seen, parts[1]); err != nil {
			return err
		}
	}
	return nil
}
func checkObjects(ctx context.Context, g gitRunner, repo string, max int64) error {
	out, err := g.run(ctx, repo, "cat-file", "--batch-all-objects", "--batch-check=%(objectsize)")
	if err != nil {
		return err
	}
	// Sum every object, so the error can say how far over the limit it is.
	total := int64(0)
	for line := range strings.FieldsSeq(string(out)) {
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil || n < 0 || n > math.MaxInt64-total {
			return errors.New("workspace: invalid Git object size")
		}
		total += n
	}
	if total > max {
		return fmt.Errorf("%w: expanded Git objects total %s, and %s remains after working files",
			ErrByteLimit, mebibytes(total), mebibytes(max))
	}
	_, err = g.run(ctx, repo, "fsck", "--full", "--strict", "--no-reflogs")
	return err
}
func encode(m manifest, bundle []byte, files [][]byte, max int64) ([]byte, error) {
	meta, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := &boundedBuffer{limit: max}
	tw := tar.NewWriter(out)
	write := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err = write("manifest.json", meta); err != nil {
		return nil, err
	}
	if err = write("repository.bundle", bundle); err != nil {
		return nil, err
	}
	for i, b := range files {
		if err = write(fmt.Sprintf("files/%08d", i), b); err != nil {
			return nil, err
		}
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func checkIndexLayout(run func(...string) ([]byte, error)) error {
	conflict, err := run("ls-files", "--unmerged", "-z")
	if err != nil {
		return err
	}
	if len(conflict) > 0 {
		return errors.New("workspace: conflicted index unsupported")
	}
	flags, err := run("ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for _, line := range splitZero(flags) {
		if len(line) > 0 && (line[0] == 'S' || line[0] >= 'a' && line[0] <= 'z') {
			return errors.New("workspace: sparse or assume-unchanged index unsupported")
		}
	}
	if _, err = run("show-ref", "--verify", indexRef); err == nil {
		return errors.New("workspace: reserved checkpoint ref already exists")
	}
	return nil
}

func captureWorkingFiles(run func(...string) ([]byte, error), root string, selectedIgnored []string, maxBytes int64) ([]entry, [][]byte, int64, error) {
	var entries []entry
	names, err := run("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, 0, err
	}
	selected := map[string]bool{}
	for _, name := range splitZero(names) {
		selected[name] = true
	}
	indexModes, err := capturedIndexModes(run)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, name := range selectedIgnored {
		if !validPath(name) {
			return nil, nil, 0, fmt.Errorf("workspace: invalid ignored path %q", name)
		}
		ignored, err := run("ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", name)
		if err != nil {
			return nil, nil, 0, err
		}
		match := false
		for _, p := range splitZero(ignored) {
			if p == name {
				match = true
			}
		}
		if !match {
			info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
			switch {
			case os.IsNotExist(statErr):
				return nil, nil, 0, fmt.Errorf("workspace: ignored file %s does not exist in the repository", name)
			case statErr == nil && info.Mode().IsRegular():
				return nil, nil, 0, fmt.Errorf("workspace: %s is not ignored by Git, so it is copied already; "+
					"leave it out of --include-ignored", name)
			}
			return nil, nil, 0, fmt.Errorf("workspace: selection is not an ignored regular file: %s", name)
		}
		selected[name] = true
	}
	ordered := make([]string, 0, len(selected))
	if len(selected) > maxEntries {
		return nil, nil, 0, errors.New("workspace: too many selected entries")
	}
	for name := range selected {
		ordered = append(ordered, name)
	}
	slices.Sort(ordered)
	aliases := map[string]string{}
	files := make([][]byte, 0, len(ordered))
	total := int64(0)
	for _, name := range ordered {
		if !validPath(name) {
			return nil, nil, 0, fmt.Errorf("workspace: invalid file path %q", name)
		}
		if err = recordPortablePath(aliases, name); err != nil {
			return nil, nil, 0, err
		}
		file := filepath.Join(root, filepath.FromSlash(name))
		info, err := sourceEntry(root, name)
		if err != nil {
			return nil, nil, 0, err
		}
		if info == nil {
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, nil, 0, fmt.Errorf("workspace: nonregular selected file %q", name)
		}
		b, err := readBounded(file, maxBytes-total)
		if err != nil {
			return nil, nil, 0, err
		}
		total += int64(len(b))
		indexMode, tracked := indexModes[name]
		mode := capturedFileMode(info, indexMode, tracked)
		entries = append(entries, descriptor(name, b, mode))
		files = append(files, b)
	}
	return entries, files, total, nil
}

func capturedIndexModes(run func(...string) ([]byte, error)) (map[string]int64, error) {
	out, err := run("ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	modes := make(map[string]int64)
	for _, item := range splitZero(out) {
		meta, name, ok := strings.Cut(item, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, errors.New("workspace: unsupported index path or mode")
		}
		mode, parseErr := strconv.ParseInt(fields[0], 8, 64)
		if parseErr != nil {
			return nil, errors.New("workspace: invalid index mode")
		}
		modes[name] = mode
	}
	return modes, nil
}
