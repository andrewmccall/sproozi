package home

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// SharedAgentsDirectory names the instruction directory every harness reads.
const SharedAgentsDirectory = "shared"

// maxAgentFile bounds one shared instruction file.
const maxAgentFile = 32 << 10

// AgentFiles lists, in precedence order, the shared agent files a harness
// receives: shared instructions first, then the harness's own. Later text
// refines earlier text. These are user preferences; approved developer
// instructions and every enforcement setting stay outside this directory.
func AgentFiles(kind harness.NativeKind) []string {
	directory, err := harness.NativeDirectory(kind)
	if err != nil {
		return nil
	}
	file := harness.NativeUserInstructionsFile(kind)
	return []string{filepath.Join(SharedAgentsDirectory, "AGENTS.md"), filepath.Join(directory, file)}
}

// UserInstructions composes the allowlisted shared instruction files for one
// harness. Missing files are skipped. Symbolic links, special files, files
// owned by another user and oversized or non-UTF-8 files are rejected rather
// than followed or silently trusted.
func UserInstructions(root string, kind harness.NativeKind) (string, error) {
	if root == "" {
		return "", nil
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("home: absolute agents directory required")
	}
	if err := ownedEntry(root, true); err != nil {
		return "", err
	}
	var parts []string
	for _, name := range AgentFiles(kind) {
		path := filepath.Join(root, name)
		if err := ownedEntry(filepath.Dir(path), true); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		data, err := readAgentFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			parts = append(parts, text)
		}
	}
	text := strings.Join(parts, "\n\n")
	if len(text) > harness.MaxUserInstructions {
		return "", errors.New("home: shared agent instructions exceed the native limit")
	}
	return text, nil
}

func readAgentFile(path string) ([]byte, error) {
	if err := ownedEntry(path, false); err != nil {
		return nil, err
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxAgentFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAgentFile || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, fmt.Errorf("home: shared agent file %s must be UTF-8 text of at most %d bytes", path, maxAgentFile)
	}
	return data, nil
}

// UnsupportedAgentEntries lists entries in the agents root that no harness
// reads, so diagnostics can report them instead of implying they apply.
func UnsupportedAgentEntries(root string) ([]string, error) {
	supported := map[string]bool{}
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		for _, name := range AgentFiles(kind) {
			supported[filepath.ToSlash(name)] = true
			supported[filepath.ToSlash(filepath.Dir(name))] = true
		}
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		if !supported[relative] || entry.Type()&os.ModeSymlink != 0 {
			out = append(out, relative)
			if entry.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	return out, err
}
