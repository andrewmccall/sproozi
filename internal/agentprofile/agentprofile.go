// Package agentprofile reads the user's profile: the instructions and skills
// added to every managed session beside the Sproozi integration.
//
// A profile folder holds an optional AGENTS.md and skills/<name>/SKILL.md with
// any supporting files. Other entries are ignored. The same rules apply to the
// live folder mounted into Docker and to the archive stored for Kubernetes.
package agentprofile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	InstructionsFile = "AGENTS.md"
	SkillsDirectory  = "skills"
	SkillFile        = "SKILL.md"

	MaxInstructionsBytes = 64 << 10
	MaxFiles             = 512
	MaxBytes             = 4 << 20
	// MaxArchiveBytes keeps the compressed archive, base64 encoded, inside one
	// Kubernetes ConfigMap.
	MaxArchiveBytes = harness.NativeProfileArchiveMaxBytes
	// EncodedArchiveBytes bounds an archive carried as base64 in JSON.
	EncodedArchiveBytes = ((MaxArchiveBytes + 2) / 3) * 4
)

var (
	skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	digest    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidDigest reports whether d has the form Archive returns.
func ValidDigest(d string) bool { return digest.MatchString(d) }

// Verify checks that archive is a valid profile whose digest is d.
func Verify(d string, archive []byte) error {
	sum := sha256.Sum256(archive)
	if !ValidDigest(d) || "sha256:"+hex.EncodeToString(sum[:]) != d {
		return errors.New("profile archive does not match its digest")
	}
	_, err := FromArchive(archive)
	return err
}

// File is one skill file, relative to its skill directory.
type File struct {
	Path       string
	Data       []byte
	Executable bool
}

type Skill struct {
	Name  string
	Files []File
}

// Profile is a validated profile. Skills and their files are sorted by name.
type Profile struct {
	Instructions string
	Skills       []Skill
}

type entry struct {
	path       string
	data       []byte
	executable bool
}

// LoadDirectory reads a profile folder. A missing folder is an empty profile.
func LoadDirectory(directory string) (Profile, error) {
	if _, err := os.Stat(directory); errors.Is(err, fs.ErrNotExist) {
		return Profile{}, nil
	}
	return Load(os.DirFS(directory))
}

// Load reads a profile from fsys, refusing symlinks and special files.
func Load(fsys fs.FS) (Profile, error) {
	var entries []entry
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		root, _, _ := strings.Cut(name, "/")
		if root != InstructionsFile && root != SkillsDirectory {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if name == InstructionsFile {
				return fmt.Errorf("profile %s is a directory", name)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("profile %s is a symlink or special file", name)
		}
		if len(entries) >= MaxFiles {
			return fmt.Errorf("profile has more than %d files", MaxFiles)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxBytes {
			return fmt.Errorf("profile is larger than %d bytes", MaxBytes)
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: name, data: data, executable: info.Mode()&0o111 != 0})
		return nil
	})
	if err != nil {
		return Profile{}, err
	}
	return assemble(entries)
}

func assemble(entries []entry) (Profile, error) {
	if len(entries) > MaxFiles {
		return Profile{}, fmt.Errorf("profile has more than %d files", MaxFiles)
	}
	var p Profile
	skills := map[string][]File{}
	total := 0
	for _, e := range entries {
		total += len(e.data)
		if total > MaxBytes {
			return Profile{}, fmt.Errorf("profile is larger than %d bytes", MaxBytes)
		}
		if e.path == InstructionsFile {
			if len(e.data) > MaxInstructionsBytes || !utf8.Valid(e.data) {
				return Profile{}, fmt.Errorf("profile %s must be UTF-8 text of at most %d bytes", InstructionsFile, MaxInstructionsBytes)
			}
			p.Instructions = string(e.data)
			continue
		}
		rest, ok := strings.CutPrefix(e.path, SkillsDirectory+"/")
		name, file, nested := strings.Cut(rest, "/")
		if !ok || !nested || file == "" || path.Clean(e.path) != e.path {
			return Profile{}, fmt.Errorf("profile file %s is not inside a skill directory", e.path)
		}
		if !skillName.MatchString(name) || name == harness.ManagedSkillName {
			return Profile{}, fmt.Errorf("profile skill name %q is invalid or reserved", name)
		}
		skills[name] = append(skills[name], File{Path: file, Data: e.data, Executable: e.executable})
	}
	for name, files := range skills {
		slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
		if !slices.ContainsFunc(files, func(f File) bool { return f.Path == SkillFile }) {
			return Profile{}, fmt.Errorf("profile skill %s has no %s", name, SkillFile)
		}
		p.Skills = append(p.Skills, Skill{Name: name, Files: files})
	}
	slices.SortFunc(p.Skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return p, nil
}

// Archive returns the profile as a reproducible tar.gz and its SHA-256 digest.
// It fails when the archive would not fit in a ConfigMap.
func (p Profile) Archive() ([]byte, string, error) {
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	write := func(name string, data []byte, executable bool) error {
		mode := int64(0o644)
		if executable {
			mode = 0o755
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)),
			Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := archive.Write(data)
		return err
	}
	if p.Instructions != "" {
		if err := write(InstructionsFile, []byte(p.Instructions), false); err != nil {
			return nil, "", err
		}
	}
	for _, s := range p.Skills {
		for _, f := range s.Files {
			if err := write(path.Join(SkillsDirectory, s.Name, f.Path), f.Data, f.Executable); err != nil {
				return nil, "", err
			}
		}
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	if err := compressed.Close(); err != nil {
		return nil, "", err
	}
	if buffer.Len() > MaxArchiveBytes {
		return nil, "", fmt.Errorf("profile archive is %d bytes; the limit is %d", buffer.Len(), MaxArchiveBytes)
	}
	sum := sha256.Sum256(buffer.Bytes())
	return buffer.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ArchiveDirectory archives the profile folder as it is now. An empty or
// missing profile has no archive and an empty digest.
func ArchiveDirectory(directory string) ([]byte, string, error) {
	p, err := LoadDirectory(directory)
	if err != nil || p.Instructions == "" && len(p.Skills) == 0 {
		return nil, "", err
	}
	return p.Archive()
}

// FromArchive reads an archive written by Archive, applying the same rules.
func FromArchive(data []byte) (Profile, error) {
	if len(data) > MaxArchiveBytes {
		return Profile{}, fmt.Errorf("profile archive is larger than %d bytes", MaxArchiveBytes)
	}
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return Profile{}, err
	}
	archive := tar.NewReader(compressed)
	var entries []entry
	total := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Profile{}, err
		}
		if header.Typeflag != tar.TypeReg || len(entries) >= MaxFiles {
			return Profile{}, fmt.Errorf("profile archive entry %s is not allowed", header.Name)
		}
		body, err := io.ReadAll(io.LimitReader(archive, int64(MaxBytes-total)+1))
		if err != nil {
			return Profile{}, err
		}
		total += len(body)
		if total > MaxBytes {
			return Profile{}, fmt.Errorf("profile is larger than %d bytes", MaxBytes)
		}
		entries = append(entries, entry{path: header.Name, data: body, executable: header.Mode&0o111 != 0})
	}
	return assemble(entries)
}
