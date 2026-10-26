package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// releaseDownloads is where each release publishes its source bundle and
// checksums. Tests point it at a local server.
var releaseDownloads = repository + "/releases/download"

// installedSourceDir is where a package such as the Homebrew formula installs
// the release source next to the CLI: PREFIX/bin/sproozi and PREFIX/share/sproozi.
const installedSourceDir = "share/sproozi"

// Limits for a downloaded source bundle; v0.2.0 is about 13 MB compressed.
const (
	maxSourceBundle   = 256 << 20
	maxSourceExpanded = 1 << 30
	maxChecksumsFile  = 1 << 20
)

// imageSource resolves the source directory for images build, in order:
// --build DIR, the release source installed with this CLI, then the matching
// release source bundle. A development build has no release to fall back on.
func (q *options) imageSource(ctx context.Context) (dir, origin string, cleanup func(), err error) {
	none := func() {}
	if q.build != "" {
		return q.build, q.build, none, nil
	}
	if exe, exeErr := os.Executable(); exeErr == nil {
		if installed, ok := installedSource(exe); ok {
			return installed, installed, none, nil
		}
	}
	if build.version == sourceVersion {
		return "", "", none, usagef("this development CLI has no release source; " +
			"pass --build with the checkout you built from")
	}
	dir, err = os.MkdirTemp("", "sproozi-source-")
	if err != nil {
		return "", "", none, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	name := sourceBundleName(build.version)
	if err = downloadSource(ctx, releaseDownloads+"/v"+build.version, name, dir); err != nil {
		cleanup()
		return "", "", none, fmt.Errorf("fetch %s: %w", name, err)
	}
	return dir, releaseDownloads + "/v" + build.version + "/" + name, cleanup, nil
}

func sourceBundleName(version string) string {
	return "sproozi_" + version + "_source.tar.gz"
}

// installedSource returns the packaged release source beside the real path of
// exe when its release.json names this CLI's version. A stale tree is ignored.
func installedSource(exe string) (string, bool) {
	if build.version == sourceVersion {
		return "", false
	}
	exe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", false
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(exe)), filepath.FromSlash(installedSourceDir))
	metadata, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return "", false
	}
	var source struct{ Version string }
	if json.Unmarshal(metadata, &source) != nil || source.Version != build.version {
		return "", false
	}
	return dir, true
}

// downloadSource fetches the bundle, checks it against the release's
// checksums.txt and expands it into dir. The checksum file comes from the same
// release, so it detects corruption rather than a replaced release.
func downloadSource(ctx context.Context, base, name, dir string) error {
	checksums, err := fetch(ctx, base+"/checksums.txt", maxChecksumsFile)
	if err != nil {
		return err
	}
	want, err := checksumFor(checksums, name)
	if err != nil {
		return err
	}
	bundle, err := fetch(ctx, base+"/"+name, maxSourceBundle)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bundle)
	if hex.EncodeToString(sum[:]) != want {
		return errors.New("checksum does not match checksums.txt")
	}
	return extractSource(bundle, dir)
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return data, nil
}

func checksumFor(checksums []byte, name string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(checksums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == name && len(fields[0]) == sha256.Size*2 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

// extractSource expands a release source bundle, dropping its single top-level
// directory. Only regular files and directories inside dir are accepted.
func extractSource(bundle []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	archive := tar.NewReader(gz)
	var written int64
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		_, rest, found := strings.Cut(strings.TrimPrefix(header.Name, "./"), "/")
		if !found || rest == "" {
			continue
		}
		if !filepath.IsLocal(rest) {
			return fmt.Errorf("unsafe path %q in source bundle", header.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(rest))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if written += header.Size; written > maxSourceExpanded {
				return errors.New("source bundle expands beyond its limit")
			}
			if err := writeSourceFile(target, archive, header); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q in source bundle", header.Name)
		}
	}
}

func writeSourceFile(target string, r io.Reader, header *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if header.Mode&0o111 != 0 {
		mode = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = io.CopyN(f, r, header.Size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
