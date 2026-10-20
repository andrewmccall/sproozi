package agentprofile_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

const (
	reviewSkill = "skills/review/SKILL.md"
	codexAgents = "codex/AGENTS.md"
)

func skill(name, description string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + description + "\n---\nBody\n")
}

func TestLoadReadsInstructionsAndSkills(t *testing.T) {
	fsys := fstest.MapFS{
		agentprofile.InstructionsFile:       {Data: []byte("Prefer small commits.\n")},
		"README.txt":                        {Data: []byte("ignored")},
		reviewSkill:                         {Data: skill("review", "Review code")},
		"skills/review/scripts/check.sh":    {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		"skills/home-ops/SKILL.md":          {Data: skill("home-ops", "Operate the home lab")},
		"skills/home-ops/references/net.md": {Data: []byte("vlan 10")},
	}
	config, err := agentprofile.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if config.Instructions != "Prefer small commits.\n" {
		t.Fatalf("instructions = %q", config.Instructions)
	}
	if len(config.Skills) != 2 || config.Skills[0].Name != "home-ops" || config.Skills[1].Name != "review" {
		t.Fatalf("skills = %+v", config.Skills)
	}
	review := config.Skills[1]
	if len(review.Files) != 2 || review.Files[0].Path != "SKILL.md" || review.Files[1].Path != "scripts/check.sh" ||
		!review.Files[1].Executable || review.Files[0].Executable {
		t.Fatalf("review files = %+v", review.Files)
	}
}

func TestLoadAcceptsAnEmptyFolder(t *testing.T) {
	config, err := agentprofile.Load(fstest.MapFS{})
	if err != nil || config.Instructions != "" || len(config.Skills) != 0 {
		t.Fatalf("config = %+v, %v", config, err)
	}
}

func TestLoadRejectsUnsafeOrReservedContent(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"reserved skill":      {"skills/sproozi-access/SKILL.md": {Data: skill("sproozi-access", "x")}},
		"hidden skill":        {"skills/.system/SKILL.md": {Data: skill("system", "x")}},
		"invalid skill name":  {"skills/Review_Code/SKILL.md": {Data: skill("Review_Code", "x")}},
		"missing SKILL.md":    {"skills/review/notes.md": {Data: []byte("x")}},
		"file in skills":      {"skills/notes.md": {Data: []byte("x")}},
		"symlink":             {reviewSkill: {Data: skill("review", "x")}, "skills/review/link": {Data: []byte("/etc/passwd"), Mode: os.ModeSymlink}},
		"binary instructions": {agentprofile.InstructionsFile: {Data: []byte{0xff, 0xfe}}},
		"large instructions":  {agentprofile.InstructionsFile: {Data: []byte(strings.Repeat("a", agentprofile.MaxInstructionsBytes+1))}},
		"instructions dir":    {"AGENTS.md/x": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := agentprofile.Load(fsys); err == nil {
				t.Fatal("Load accepted unsafe profile")
			}
		})
	}
}

func TestLoadEnforcesFileAndByteLimits(t *testing.T) {
	many := fstest.MapFS{reviewSkill: {Data: skill("review", "x")}}
	for i := range agentprofile.MaxFiles {
		many[filepath.Join("skills/review/refs", strings.Repeat("f", 1+i%50)+string(rune('a'+i/50%26))+".md")] = &fstest.MapFile{Data: []byte("x")}
	}
	if _, err := agentprofile.Load(many); err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("file limit error = %v", err)
	}
	large := fstest.MapFS{
		reviewSkill:              {Data: skill("review", "x")},
		"skills/review/data.bin": {Data: make([]byte, agentprofile.MaxBytes+1)},
	}
	if _, err := agentprofile.Load(large); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("byte limit error = %v", err)
	}
}

func TestArchiveIsDeterministicAndRoundTrips(t *testing.T) {
	fsys := fstest.MapFS{
		agentprofile.InstructionsFile:    {Data: []byte("Be brief.\n"), Mode: 0o600},
		reviewSkill:                      {Data: skill("review", "Review code")},
		"skills/review/scripts/check.sh": {Data: []byte("#!/bin/sh\n"), Mode: 0o700},
	}
	config, err := agentprofile.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	first, digest, err := config.Archive()
	if err != nil {
		t.Fatal(err)
	}
	// Different source modes for non-executable files must not change identity.
	fsys[reviewSkill].Mode = 0o600
	again, err := agentprofile.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := again.Archive()
	if err != nil || string(first) != string(second) || digest != secondDigest {
		t.Fatalf("archive not deterministic: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest = %q", digest)
	}
	restored, err := agentprofile.FromArchive(first)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Instructions != config.Instructions || len(restored.Skills) != 1 ||
		len(restored.Skills[0].Files) != 2 || !restored.Skills[0].Files[1].Executable {
		t.Fatalf("restored = %+v", restored)
	}
	if _, err := agentprofile.FromArchive(append([]byte{}, first[:len(first)/2]...)); err == nil {
		t.Fatal("truncated archive accepted")
	}
}

func TestArchiveRejectsConfigurationOverTheConfigMapLimit(t *testing.T) {
	// Random-looking content defeats compression, so the archive exceeds the limit.
	data := make([]byte, agentprofile.MaxArchiveBytes+1024)
	state := uint32(1)
	for i := range data {
		state = state*1664525 + 1013904223
		data[i] = byte(state >> 24)
	}
	config, err := agentprofile.Load(fstest.MapFS{
		reviewSkill:              {Data: skill("review", "x")},
		"skills/review/data.bin": {Data: data},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.Archive(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("archive limit error = %v", err)
	}
}

// privateProfile writes files into a folder private to the test's user, as
// LoadDirectory requires.
func privateProfile(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(testutil.PrivateDir(t), "profile")
	if err := testutil.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := testutil.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := testutil.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Each harness receives the shared instructions, then only its own.
func TestHarnessInstructionsFollowTheSharedInstructions(t *testing.T) {
	dir := privateProfile(t, map[string][]byte{
		agentprofile.InstructionsFile: []byte("Shared rule.\n"),
		codexAgents:                   []byte("Codex rule."),
		"claude/CLAUDE.md":            []byte("Claude rule."),
		"claude/settings.json":        []byte("{}"),
		"codex/CLAUDE.md":             []byte("misplaced"),
		"notes/todo.md":               []byte("x"),
		reviewSkill:                   skill("review", "x"),
	})
	p, err := agentprofile.LoadDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.InstructionsFor(harness.NativeCodex); got != "Shared rule.\n\nCodex rule." {
		t.Fatalf("codex = %q", got)
	}
	if got := p.InstructionsFor(harness.NativeClaude); got != "Shared rule.\n\nClaude rule." {
		t.Fatalf("claude = %q", got)
	}
	archive, _, err := p.Archive()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := agentprofile.FromArchive(archive)
	if err != nil || restored.InstructionsFor(harness.NativeClaude) != "Shared rule.\n\nClaude rule." {
		t.Fatalf("restored = %+v, %v", restored, err)
	}
	ignored, err := agentprofile.Ignored(dir)
	if err != nil || strings.Join(ignored, ",") != "claude/settings.json,codex/CLAUDE.md,notes" {
		t.Fatalf("ignored = %q, %v", ignored, err)
	}
	if only, err := agentprofile.Load(fstest.MapFS{codexAgents: {Data: []byte("Codex only.")}}); err != nil ||
		only.InstructionsFor(harness.NativeCodex) != "Codex only." || only.InstructionsFor(harness.NativeClaude) != "" {
		t.Fatalf("codex only = %+v, %v", only, err)
	}
	if _, err = agentprofile.Load(fstest.MapFS{"claude/CLAUDE.md": {Data: []byte("a\x00b")}}); err == nil {
		t.Fatal("instructions with a NUL byte accepted")
	}
}

// Another local account must not be able to add instructions.
func TestLoadDirectoryRejectsFilesWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows expresses ownership with ACLs; privatefs tests cover them")
	}
	dir := privateProfile(t, map[string][]byte{codexAgents: []byte("x"), "notes/open.md": []byte("x")})
	if err := os.Chmod(filepath.Join(dir, "notes", "open.md"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := agentprofile.LoadDirectory(dir); err != nil {
		t.Fatalf("an ignored file blocked the profile: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "codex", "AGENTS.md"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := agentprofile.LoadDirectory(dir); err == nil || !strings.Contains(err.Error(), "not writable by others") {
		t.Fatalf("writable file = %v", err)
	}
}

func TestLoadDirectoryRejectsSymlinkedFiles(t *testing.T) {
	dir := privateProfile(t, map[string][]byte{reviewSkill: skill("review", "x")})
	if _, err := agentprofile.LoadDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, agentprofile.InstructionsFile)); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if _, err := agentprofile.LoadDirectory(dir); err == nil {
		t.Fatal("LoadDirectory followed a symlink")
	}
	missing, err := agentprofile.LoadDirectory(filepath.Join(dir, "absent"))
	if err != nil || missing.Instructions != "" || len(missing.Skills) != 0 {
		t.Fatalf("absent folder = %+v, %v", missing, err)
	}
}
